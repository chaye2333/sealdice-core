package dice

import (
	crand "crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

// 本文件实现「私聊验证码」身份验证。
//
// 为什么需要它：答题（角色卡名 / 日志名）只能拦住"完全不知道你信息的人"，
// 拦不住抢号——只要对方知道你的旧 QQ 号、又碰巧或猜到你的卡名，
// 就能把你的整套数据（角色卡、属性、日志）接管过去。
//
// 验证码解决的是**归属证明**：官方 bot 这边无法确认"你就是旧 QQ 号 123456"，
// 但民间 bot 可以——它能给那个号码发私聊。所以流程是跨两个 bot 的：
//
//	1. 用户在官方 bot 发起 .bind 123456
//	2. 官方 bot 生成一个验证码，登记成一条 challenge
//	3. 后台 worker 通过**民间 bot**（OneBot 连接）给 123456 发私聊验证码
//	4. 拿到验证码的人在私聊里把它回给民间 bot
//	5. 民间 bot 校验通过 → 建立绑定关系（新身份 = 官方侧发起者）
//
// 因为只有真正在那个旧 QQ 号上的人才能收到私聊，所以这一步无法伪造。
//
// 群绑定同理：验证码发给旧群的邀请人（群主/拉骰子进群的人），由他确认。
//
// 绑定方向：**始终由官方 bot 侧发起**，民间 bot 只负责投递与确认。

// ---------- 挑战状态 ----------

type identityBindCodeStatus string

// identityBindCodeChannel 验证码走哪条通道投递。
type identityBindCodeChannel string

const (
	// identityBindCodeChannelNone 还没投递
	identityBindCodeChannelNone identityBindCodeChannel = ""
	// identityBindCodeChannelDM 民间 bot 私聊
	identityBindCodeChannelDM identityBindCodeChannel = "dm"
	// identityBindCodeChannelEmail QQ 邮箱
	identityBindCodeChannelEmail identityBindCodeChannel = "email"
)

const (
	// identityBindCodePending 已登记，等待民间 bot 投递
	identityBindCodePending identityBindCodeStatus = "pending"
	// identityBindCodeDelivered 已发出私聊，等待对方回复
	identityBindCodeDelivered identityBindCodeStatus = "delivered"
	// identityBindCodeUsed 已完成（无论成功还是被拒绝），可以清理
	identityBindCodeUsed identityBindCodeStatus = "used"
	// identityBindCodeExpired 超时未确认
	identityBindCodeExpired identityBindCodeStatus = "expired"
)

const (
	// identityBindCodeSessionTTL 一条验证码的有效期，默认 10 分钟。
	identityBindCodeSessionTTL = 10 * time.Minute

	// identityBindCodeMaxAttempts 单条验证码允许的失败次数，超过即作废。
	// 有了失败上限就不需要"答错锁 12 小时"那种重惩罚，体验好很多。
	identityBindCodeMaxAttempts = 5

	// identityBindCodeMinLen / identityBindCodeMaxLen 验证码长度范围。
	identityBindCodeMinLen = 4
	identityBindCodeMaxLen = 8

	// identityBindCodeDeliverInterval 后台投递 worker 的扫描间隔。
	identityBindCodeDeliverInterval = 3 * time.Second

	// identityBindCodeKeepRecordFor 已完成的挑战保留一段时间，便于排查。
	identityBindCodeKeepRecordFor = 10 * time.Minute
)

// identityBindCodeChallenge 一次等待确认的绑定。
type identityBindCodeChallenge struct {
	Action identityBindAction

	// New 发起绑定的官方身份（官方 bot 侧的群 + 用户）
	New identityBindEndpoint
	// Old 被声明为"自己的"旧身份（旧 QQ 号 / 旧群）
	Old identityBindEndpoint

	// Code 验证码，仅用于投递与校验
	Code string
	// DeliverTo 验证码要发给谁。
	// 个人绑定 = 旧 QQ 号（私聊通道）；群绑定 = 旧群的邀请人。
	DeliverTo string
	// ConfirmBy 只有这个身份回复才被接受。
	// 个人绑定 = 旧 QQ 号；群绑定 = 旧群的邀请人（""表示不做发信人校验）。
	ConfirmBy string
	// Channel 实际使用的投递通道，决定了「用户该去哪里拿码、去哪里回复」。
	Channel identityBindCodeChannel

	Status   identityBindCodeStatus
	Attempts int
	// SentByEP 实际投递用的端点 ID，用于提示与排查
	SentByEP string
	// SentTo 实际投递到的地址（私聊=QQ号，邮箱=邮箱地址），用于提示
	SentTo string

	CreatedAt   int64
	ExpiresAt   int64
	DeliveredAt int64
	FinishedAt  int64

	// Reason 完成/失败原因，用于通知与自检
	Reason string
}

func (c *identityBindCodeChallenge) expired() bool {
	if c == nil {
		return true
	}
	return c.ExpiresAt > 0 && time.Now().Unix() > c.ExpiresAt
}

// ---------- 存储 ----------

var (
	globalIdentityBindCodes       SyncMap[string, *identityBindCodeChallenge]
	globalIdentityBindDeliverOnce sync.Once
)

// identityBindChallengeOwnerID 一条挑战归属于谁——也就是**发起绑定的人**。
//
// 个人绑定取官方侧用户 ID，群绑定取真实群 ID。
// 为什么不用被声明的旧身份做键：那个键是"私聊投给谁"，不是"谁在等结果"。
// 用旧身份做键会导致 .bind cancel 查不到自己的挑战（发起者根本不知道自己
// 那个号对应的旧身份键是什么），从而永远取消不掉。
func identityBindChallengeOwnerID(action identityBindAction, newID identityBindEndpoint) string {
	if action == identityBindActionGroup {
		return newID.GroupID
	}
	return newID.UserID
}

// identityBindCodeKey 挑战的存储键：由「发起者 + 类型」构成。
// 同一个发起者同时只允许一条挑战，后发起的覆盖前一条。
func identityBindCodeKey(action identityBindAction, ownerID string) string {
	return string(action) + "|" + ownerID
}

// identityBindGenerateCode 生成一个数字验证码。
func identityBindGenerateCode(length int) (string, error) {
	if length < identityBindCodeMinLen {
		length = identityBindCodeMinLen
	}
	if length > identityBindCodeMaxLen {
		length = identityBindCodeMaxLen
	}
	var b strings.Builder
	b.Grow(length)
	for range length {
		n, err := crand.Int(crand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		b.WriteByte(byte('0' + n.Int64()))
	}
	return b.String(), nil
}

// identityBindPutCode 登记一条新挑战，覆盖同一发起者的旧挑战。
func identityBindPutCode(challenge *identityBindCodeChallenge) {
	if challenge == nil {
		return
	}
	owner := identityBindChallengeOwnerID(challenge.Action, challenge.New)
	if owner == "" {
		return
	}
	globalIdentityBindCodes.Store(identityBindCodeKey(challenge.Action, owner), challenge)
}

// identityBindLoadCode 按**发起者**取挑战。
// 传 userID = 官方侧用户 ID（个人）或真实群 ID（群）。
func identityBindLoadCode(action identityBindAction, ownerID string) (*identityBindCodeChallenge, bool) {
	v, ok := globalIdentityBindCodes.Load(identityBindCodeKey(action, ownerID))
	if !ok || v == nil {
		return nil, false
	}
	return v, true
}

// identityBindCleanupCodes 清理已过期 / 已完成的挑战。
func identityBindCleanupCodes() {
	now := time.Now().Unix()
	var toDelete []string
	globalIdentityBindCodes.Range(func(key string, c *identityBindCodeChallenge) bool {
		if c == nil {
			toDelete = append(toDelete, key)
			return true
		}
		// 过期的标记一下（只标记一次，方便告诉用户原因）
		if c.Status != identityBindCodeUsed && c.Status != identityBindCodeExpired && c.expired() {
			c.Status = identityBindCodeExpired
			c.Reason = "验证码已过期"
			c.FinishedAt = now
		}
		if c.Status == identityBindCodeUsed || c.Status == identityBindCodeExpired {
			if c.FinishedAt > 0 && now-c.FinishedAt > int64(identityBindCodeKeepRecordFor.Seconds()) {
				toDelete = append(toDelete, key)
			}
		}
		return true
	})
	for _, key := range toDelete {
		globalIdentityBindCodes.Delete(key)
	}
}

// ---------- 配置 ----------

// identityBindUseVerificationCode 是否启用验证码验证身份归属。
// 关掉等于放弃防抢号，仅在骰主完全无法提供任何投递通道时才考虑。
func identityBindUseVerificationCode(d *Dice) bool {
	return d != nil && d.Config.IdentityBindUseVerificationCode
}

// identityBindPreferEmailCode 是否优先用邮箱投递验证码。
func identityBindPreferEmailCode(d *Dice) bool {
	return d != nil && d.Config.IdentityBindPreferEmailCode
}

// identityBindCodeLength 取验证码长度并收敛到合法范围。
func identityBindCodeLength(d *Dice) int {
	n := 6
	if d != nil && d.Config.IdentityBindCodeLength > 0 {
		n = int(d.Config.IdentityBindCodeLength)
	}
	if n < identityBindCodeMinLen {
		n = identityBindCodeMinLen
	}
	if n > identityBindCodeMaxLen {
		n = identityBindCodeMaxLen
	}
	return n
}

// identityBindCodeExpiry 取验证码有效期并收敛到合法范围。
func identityBindCodeExpiry(d *Dice) time.Duration {
	sec := int64(0)
	if d != nil {
		sec = d.Config.IdentityBindCodeExpireSec
	}
	if sec <= 0 {
		sec = int64(identityBindCodeSessionTTL.Seconds())
	}
	if sec < 60 {
		sec = 60
	}
	if sec > 3600 {
		sec = 3600
	}
	return time.Duration(sec) * time.Second
}

// ---------- 端点的挑选 ----------

// identityBindFindOldBotEndPoint 找"民间 bot"那条连接。
//
// 选点规则：
//  1. 必须启用；
//  2. 能处理带 "QQ:" 前缀的裸 QQ 号（也就是 OneBot / walle-q 这类非官方实现）；
//  3. 排除官方端点本身。
//
// 返回 nil 表示当前实例上没有任何可用的民间 bot 连接，这时验证码发不出去，
// 调用方必须给出明确提示（而不是静默失败）。
func identityBindFindOldBotEndPoint(session *IMSession, exclude *EndPointInfo) *EndPointInfo {
	if session == nil {
		return nil
	}
	var fallback *EndPointInfo
	for _, ep := range session.EndPoints {
		if ep == nil || !ep.Enable || ep.Adapter == nil {
			continue
		}
		if exclude != nil && ep.ID == exclude.ID {
			continue
		}
		if identityBindSupported(ep) {
			// 官方端点不能用来给旧 QQ 号发私聊
			continue
		}
		if ep.Platform != "QQ" {
			continue
		}
		switch ep.ProtocolType {
		case "onebot", "walle-q", "red", "milky", "pureonebot":
			// 这些都能处理 QQ:<号>
		default:
			continue
		}
		if ep.ProtocolType == "onebot" || ep.ProtocolType == "pureonebot" {
			return ep
		}
		if fallback == nil {
			fallback = ep
		}
	}
	return fallback
}

// identityBindSendPrivate 通过指定端点给某个 QQ 号发私聊。
func identityBindSendPrivate(d *Dice, ep *EndPointInfo, targetRawID string, text string) error {
	if d == nil || ep == nil || ep.Adapter == nil {
		return errors.New("没有可用的民间 bot 连接")
	}
	if strings.TrimSpace(targetRawID) == "" {
		return errors.New("缺少收件人 QQ 号")
	}
	ctx := &MsgContext{
		Dice:        d,
		EndPoint:    ep,
		Session:     ep.Session,
		IsPrivate:   true,
		MessageType: "private",
	}
	ep.Adapter.SendToPerson(ctx, targetRawID, text, "skip")
	return nil
}

// ---------- 通道一：QQ 邮箱 ----------

// identityBindQQMailAddress 从旧 QQ 号推出 QQ 邮箱地址。
//
// 为什么用 <QQ号>@qq.com：这是唯一"零配置又有约束力"的方案。
// QQ 邮箱与 QQ 号绑定，所以能收到这封信 ≈ 控制着这个 QQ 号。
// **不会**接受用户自己填的邮箱——那既证明不了归属，又会让骰子变成发信机。
func identityBindQQMailAddress(oldUserID string) string {
	qq := identityBindExtractQQNumber(oldUserID)
	if qq == "" {
		return ""
	}
	return qq + "@qq.com"
}

// identityBindEmailCodeUsable 邮箱通道是否可用。
//
// **只看邮件配置本身**（发件邮箱 / 密钥 / SMTP 三者齐全），不再要求一个额外的开关：
// 骰主既然把 SMTP 配好了，就说明他想用邮件；再要求点一次开关纯属多余，
// 而且漏点会出现"配置明明好了却一直提示没配"的困惑。
//
// 走哪条通道由 identityBindPreferEmailCode 决定（见 identityBindDeliverPendingCodes）。
func identityBindEmailCodeUsable(d *Dice) bool {
	if d == nil {
		return false
	}
	return d.CanSendMail()
}

// identityBindMailSender 实际投递邮件的函数。
//
// 单独抽成变量有两个目的：
//  1. 让测试可以注入假发信器，从而在不起真 SMTP 的情况下测「投递走没走通」；
//  2. 保持生产行为不变——默认就是 Dice.SendMailRow。
var identityBindMailSender = func(d *Dice, subject string, to []string, body string) {
	d.SendMailRow(subject, to, body, nil)
}

// identityBindSendEmailCode 把验证码寄给旧 QQ 号的 QQ 邮箱。
//
// 注意：SendMailRow 是同步的，SMTP 可能阻塞好几秒，所以调用方必须在
// 后台 worker 里执行（identityBindDeliverPendingCodes 就是），绝不能在指令处理路径上直接调。
func identityBindSendEmailCode(d *Dice, c *identityBindCodeChallenge) error {
	if d == nil || c == nil {
		return errors.New("上下文为空")
	}
	if !identityBindEmailCodeUsable(d) {
		return errors.New("邮箱通道未启用或邮件配置不完整（需要 mailEnabled/mailFrom/mailPassword/mailSmtp）")
	}
	to := identityBindQQMailAddress(c.Old.UserID)
	if to == "" {
		return errors.New("无法从旧 QQ 号推出 QQ 邮箱地址")
	}

	subject := "身份绑定验证码"
	body := fmt.Sprintf(
		"有人正在 QQ 官方机器人上把身份绑定到这个 QQ 号（%s）。\n\n"+
			"如果**是你本人**在操作，请把下面的验证码回复给官方机器人：\n\n"+
			"    %s\n\n"+
			"验证码 %s 内有效。\n"+
			"不是本人操作请直接忽略本邮件，绑定不会生效。\n",
		c.Old.UserID, c.Code, identityBindFormatDuration(identityBindCodeExpiry(d)))

	identityBindMailSender(d, subject, []string{to}, body)
	c.SentTo = to
	return nil
}

// ---------- 投递 worker ----------

// identityBindStartCodeWorker 启动验证码投递后台任务。
// 幂等：重复调用只会启动一次。
func identityBindStartCodeWorker(d *Dice) {
	if d == nil {
		return
	}
	globalIdentityBindDeliverOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(identityBindCodeDeliverInterval)
			defer ticker.Stop()
			for range ticker.C {
				identityBindDeliverPendingCodes(d)
				identityBindCleanupCodes()
			}
		}()
	})
}

// identityBindDeliverPendingCodes 扫描所有待投递的挑战并尝试发送验证码。
func identityBindDeliverPendingCodes(d *Dice) {
	if d == nil || !identityBindEnabled(d) || !identityBindUseVerificationCode(d) {
		return
	}
	session := d.ImSession
	if session == nil {
		return
	}

	type pending struct {
		c *identityBindCodeChallenge
	}
	var todo []pending
	globalIdentityBindCodes.Range(func(_ string, c *identityBindCodeChallenge) bool {
		if c != nil && c.Status == identityBindCodePending && !c.expired() {
			todo = append(todo, pending{c: c})
		}
		return true
	})
	if len(todo) == 0 {
		return
	}

	for _, item := range todo {
		c := item.c

		// 两条通道按优先级依次尝试。
		// 默认私聊优先（只有它同时能证明"控制着这个 QQ 号"且不需要额外配置），
		// 骰主打开 IdentityBindPreferEmailCode 后改为邮箱优先。
		//
		// 邮箱通道只在个人绑定上有意义：群号推不出邮箱，而且群绑定要证明的是"群"，
		// 不是某个 QQ 号。
		emailUsable := c.Action == identityBindActionUser && identityBindEmailCodeUsable(d)

		tryEmail := func() bool {
			if !emailUsable {
				return false
			}
			if err := identityBindSendEmailCode(d, c); err != nil {
				c.Reason = "邮箱投递失败: " + err.Error()
				return false
			}
			c.Status = identityBindCodeDelivered
			c.Channel = identityBindCodeChannelEmail
			c.SentByEP = ""
			c.DeliveredAt = time.Now().Unix()
			c.Reason = ""
			d.Logger.Infof("身份绑定验证码已邮件投递: 目标=%s 收件=%s 发起者=%s",
				c.Old.UserID, c.SentTo, c.New.UserID)
			return true
		}

		tryDM := func() bool {
			ep := identityBindFindOldBotEndPoint(session, nil)
			if ep == nil {
				return false
			}
			body := fmt.Sprintf(
				"【鲸娘与豹】身份绑定验证码\n\n"+
					"有人正在 QQ 官方机器人上把身份绑定到你这个号（%s）。\n"+
					"如果**是你本人**在操作，请把下面的验证码私聊发给我：\n\n"+
					"    %s\n\n"+
					"验证码 %s 内有效，不是本人操作请直接忽略本条消息。",
				c.Old.UserID, c.Code, identityBindFormatDuration(identityBindCodeExpiry(d)))

			if err := identityBindSendPrivate(d, ep, c.DeliverTo, body); err != nil {
				if c.Reason == "" {
					c.Reason = "私聊投递失败: " + err.Error()
				}
				return false
			}
			c.Status = identityBindCodeDelivered
			c.Channel = identityBindCodeChannelDM
			c.SentByEP = ep.ID
			c.SentTo = c.DeliverTo
			c.DeliveredAt = time.Now().Unix()
			c.Reason = ""
			d.Logger.Infof("身份绑定验证码已私聊投递: 目标=%s 发起者=%s 端点=%s",
				c.Old.UserID, c.New.UserID, ep.ID)
			return true
		}

		// 按优先级依次尝试两条通道；先成功的算数。
		// 默认私聊优先，骰主把 IdentityBindPreferEmailCode 打开后邮箱优先。
		var delivered bool
		if identityBindPreferEmailCode(d) && emailUsable {
			// 邮箱优先：邮箱失败仍然退回私聊，不让邮件问题阻断绑定
			delivered = tryEmail() || tryDM()
		} else {
			delivered = tryDM() || tryEmail()
		}
		if delivered {
			continue
		}

		// 两条通道都不可用：把原因说清楚，别反复刷屏
		if c.Reason == "" || c.Channel == identityBindCodeChannelNone {
			switch {
			case c.Action == identityBindActionGroup:
				c.Reason = "没有可用的民间 bot（OneBot）连接，群绑定无法投递验证码"
			case emailUsable:
				c.Reason = "验证码投递失败，请稍后重试（私聊与邮箱都试过了）"
			default:
				c.Reason = "没有可用的民间 bot（OneBot）连接，邮箱也没配好；" +
					"请在管理界面配置「邮箱通知」（发件邮箱 / 密钥 / SMTP）作为备用通道"
			}
		}
	}
}

// ---------- 接收验证码 ----------

// identityBindTryConsumeCode 看一条消息是不是验证码回复。
//
// 返回 true 表示这条消息就是验证码回复，调用方不应再当普通指令处理。
//
// 两条通道的确认方式不同，所以匹配条件也不同：
//
//	私聊通道（民间 bot）：只有"被声明的那个旧账号"回复才有效。
//	                        发到哪个号、就由哪个号确认，天然绑定归属。
//	邮箱通道（官方 bot）：  码只进了发起者的 QQ 邮箱，所以由**官方侧的发起者**
//	                        在官方 bot 侧回复。发起者身份由官方 OpenID 保证。
//
// 两者共同点：验证码本身只有"被声明的旧账号"能拿到，所以都能证明归属。
func identityBindTryConsumeCode(ctx *MsgContext, msg *Message, text string) bool {
	if ctx == nil || ctx.Dice == nil || msg == nil {
		return false
	}
	d := ctx.Dice
	if !identityBindEnabled(d) || !identityBindUseVerificationCode(d) {
		return false
	}

	code := strings.TrimSpace(text)
	if code == "" || len(code) > identityBindCodeMaxLen {
		return false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return false
		}
	}

	onOfficial := identityBindSupported(ctx.EndPoint)
	isPrivate := ctx.IsPrivate || msg.MessageType == "private"

	// 扫描所有已投递、未过期的挑战，找验证码匹配、且**通道与当前端点相符**的那一条
	var matched *identityBindCodeChallenge
	var matchedKey string
	globalIdentityBindCodes.Range(func(key string, c *identityBindCodeChallenge) bool {
		if c == nil || c.Status != identityBindCodeDelivered || c.expired() {
			return true
		}
		// 通道必须对得上：邮箱码不在民间 bot 侧认，私聊码不在官方侧认。
		// 这条限制同时保证了"群聊里的纯数字消息不会被吞掉"——
		// 私聊通道要求 isPrivate，而邮箱通道要求发信人是发起者本人。
		switch c.Channel {
		case identityBindCodeChannelEmail:
			if !onOfficial {
				return true
			}
		case identityBindCodeChannelDM:
			if onOfficial || !isPrivate {
				return true
			}
		default:
			return true
		}
		if subtleCompareCode(c.Code, code) {
			matched = c
			matchedKey = key
			return false
		}
		return true
	})
	if matched == nil {
		// 不是验证码回复，交回给正常指令流程。
		// 不做任何提示：随手发个数字不该被骰子插嘴。
		return false
	}

	// 决定"谁有资格确认"
	wantConfirmer := matched.ConfirmBy
	if matched.Channel == identityBindCodeChannelEmail {
		// 邮箱码：由官方侧发起者确认
		wantConfirmer = matched.New.UserID
	}
	if wantConfirmer != "" && !identityBindSameIdentity(msg.Sender.UserID, wantConfirmer) {
		// 有人在用错误的号试别人的验证码 —— 记一次失败
		matched.Attempts++
		d.Logger.Warnf("身份绑定验证码发信人不符: 期望=%s 实际=%s 目标=%s",
			wantConfirmer, msg.Sender.UserID, matched.Old.UserID)
		identityBindReplyPerson(ctx, msg.Sender.UserID,
			"这个验证码不是发给你的，请让本人用他自己的号回复。")
		if matched.Attempts >= identityBindCodeMaxAttempts {
			matched.Status = identityBindCodeUsed
			matched.FinishedAt = time.Now().Unix()
			matched.Reason = "尝试次数过多"
		}
		return true
	}

	// 通过 —— 建立绑定
	record := &identityBindRecord{
		Action:  matched.Action,
		New:     matched.New,
		Old:     matched.Old,
		Created: time.Now().Unix(),
		Creator: matched.New.UserID,
	}
	if err := identityBindStoreOf(d).put(d, record); err != nil {
		matched.Attempts++
		identityBindReplyPerson(ctx, msg.Sender.UserID, fmt.Sprintf("绑定保存失败: %v", err))
		return true
	}

	matched.Status = identityBindCodeUsed
	matched.FinishedAt = time.Now().Unix()
	matched.Reason = "验证码确认成功"
	globalIdentityBindCodes.Store(matchedKey, matched)

	// 群绑定完成后，真实群（官方群）上残留的日志状态就失效了，清掉它，
	// 否则解绑或回退到官方主线时会"复活"成一份同名空日志（详见函数注释）。
	if matched.Action == identityBindActionGroup && ctx.Session != nil {
		if realGroup, ok := ctx.Session.ServiceAtNew.Load(matched.New.GroupID); ok && realGroup != nil {
			identityBindResetRealGroupLogState(ctx, realGroup)
		}
	}

	// 清理可能存在的同目标旧挑战与答题会话
	identityBindClearSession(identityBindSessionKey(ctx.EndPoint.ID, matched.New.UserID, matched.Action))

	// 回执给确认人。
	// 私聊通道：确认人是民间 bot 侧那个旧号 → 私聊回复他。
	// 邮箱通道：确认人就是官方侧发起者，而他正在官方群里等消息，
	//           identityBindNotifyNewSide 已经会通知到，这里不必再私聊一次。
	if matched.Channel == identityBindCodeChannelDM {
		if matched.Action == identityBindActionGroup {
			identityBindReplyPerson(ctx, msg.Sender.UserID, fmt.Sprintf(
				"验证通过！官方群已经和旧群 %s 绑定，双方现在共用同一份日志。", matched.Old.GroupID))
		} else {
			identityBindReplyPerson(ctx, msg.Sender.UserID, fmt.Sprintf(
				"验证通过！你的官方身份 %s 现在与旧 QQ 号共用同一份数据。", matched.New.UserID))
		}
	}

	// 通知官方 bot 侧的发起者
	identityBindNotifyNewSide(d, matched)

	d.LastUpdatedTime = time.Now().Unix()
	return true
}

// identityBindNotifyNewSide 通过官方 bot 把结果告诉发起绑定的人。
func identityBindNotifyNewSide(d *Dice, c *identityBindCodeChallenge) {
	if d == nil || c == nil || c.New.GroupID == "" && c.New.UserID == "" {
		return
	}
	ep := identityBindFindNewBotEndPoint(d.ImSession)
	if ep == nil {
		return
	}

	var text string
	if c.Action == identityBindActionGroup {
		text = fmt.Sprintf("【绑定成功】验证码已确认，官方群已与旧群 %s 绑定。\n"+
			"双方现在共用同一份日志：`.log list` 即可看到。", c.Old.GroupID)
	} else {
		text = fmt.Sprintf("【绑定成功】验证码已确认，%s 现在与旧 QQ 号 %s 共用同一份数据。\n"+
			"发送 `.pc list` / `.st show` 即可看到旧角色卡。", c.New.UserID, c.Old.UserID)
	}

	mctx := &MsgContext{Dice: d, EndPoint: ep, Session: ep.Session, MessageType: "group", IsCurGroupBotOn: true}
	if c.New.GroupID != "" {
		mctx.Group = &GroupInfo{GroupID: c.New.GroupID}
	}
	ReplyGroup(mctx, &Message{GroupID: c.New.GroupID}, text)
}

// identityBindFindNewBotEndPoint 找官方 bot 那条连接（用于发通知）。
func identityBindFindNewBotEndPoint(session *IMSession) *EndPointInfo {
	if session == nil {
		return nil
	}
	for _, ep := range session.EndPoints {
		if ep == nil || !ep.Enable || ep.Adapter == nil {
			continue
		}
		if identityBindSupported(ep) {
			return ep
		}
	}
	return nil
}

// identityBindReplyPerson 直接给某个号发私聊（用于回执，不依赖指令上下文）。
func identityBindReplyPerson(ctx *MsgContext, targetRawID string, text string) {
	if ctx == nil || ctx.EndPoint == nil || ctx.EndPoint.Adapter == nil {
		return
	}
	ctx.EndPoint.Adapter.SendToPerson(ctx, targetRawID, text, "skip")
}

// identityBindSameIdentity 判断两个身份标识是否指向同一个人。
//
// 先做**全等比较**，这样官方 OpenID 这类非数字标识也能正确匹配。
// 只有全等失败时才退回"提取纯 QQ 号再比"，用来兼容 "QQ:12345" 与裸 "12345"。
//
// 为什么必须这样：官方 ID 形如 "OpenQQ:<UIN>-<MemberOpenID>"，
// 而 MemberOpenID 通常是十六进制串。老实现只做数字提取 → 得到空串 → 比对永远失败，
// 于是**邮箱通道下发起者本人也确认不了自己的绑定**。
func identityBindSameIdentity(a, b string) bool {
	ta := strings.TrimSpace(a)
	tb := strings.TrimSpace(b)
	if ta == "" || tb == "" {
		return false
	}
	if ta == tb {
		return true
	}
	return identityBindSameQQUser(ta, tb)
}

// identityBindSameQQUser 判断两个身份是不是同一个 QQ 号。
// 兼容 "QQ:12345" 与裸 "12345" 两种写法。
func identityBindSameQQUser(a, b string) bool {
	na := identityBindExtractQQNumber(a)
	nb := identityBindExtractQQNumber(b)
	if na == "" || nb == "" {
		return false
	}
	return na == nb
}

// identityBindExtractQQNumber 从各种写法里取出纯 QQ 号。
func identityBindExtractQQNumber(id string) string {
	s := strings.TrimSpace(id)
	if s == "" {
		return ""
	}
	// 取最后一个 ':' 之后的部分，兼容 "QQ:12345"、"OpenQQ:...-12345" 之类
	if idx := strings.LastIndex(s, ":"); idx >= 0 {
		s = s[idx+1:]
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return s
}

// subtleCompareCode 常数时间比较验证码，避免时序侧信道。
func subtleCompareCode(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range len(a) {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
