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

// identityBindCodeChallenge 一次等待私聊确认的绑定。
type identityBindCodeChallenge struct {
	Action identityBindAction

	// New 发起绑定的官方身份（官方 bot 侧的群 + 用户）
	New identityBindEndpoint
	// Old 被声明为"自己的"旧身份（旧 QQ 号 / 旧群）
	Old identityBindEndpoint

	// Code 验证码，仅用于私聊投递与校验
	Code string
	// DeliverTo 验证码要私聊发给谁。
	// 个人绑定 = 旧 QQ 号；群绑定 = 旧群的邀请人。
	DeliverTo string
	// ConfirmBy 只有这个身份回复才被接受。
	// 个人绑定 = 旧 QQ 号；群绑定 = 旧群的邀请人（""表示不做发信人校验）。
	ConfirmBy string

	Status   identityBindCodeStatus
	Attempts int
	// SentByEP 实际投递用的端点 ID，用于提示与排查
	SentByEP string

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

// identityBindCodeKey 个人绑定用旧 QQ 号做键，群绑定用旧群号做键。
// 同一个目标同时只允许一条挑战，天然防止重复轰炸。
func identityBindCodeKey(action identityBindAction, oldID string) string {
	return string(action) + "|" + oldID
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

// identityBindPutCode 登记一条新挑战，覆盖同一目标的旧挑战。
func identityBindPutCode(challenge *identityBindCodeChallenge) {
	if challenge == nil || challenge.Old.UserID == "" && challenge.Old.GroupID == "" {
		return
	}
	oldID := challenge.Old.UserID
	if challenge.Action == identityBindActionGroup {
		oldID = challenge.Old.GroupID
	}
	globalIdentityBindCodes.Store(identityBindCodeKey(challenge.Action, oldID), challenge)
}

// identityBindTakeCode 取出并删除一条待投递的挑战（不删除，只标记）。
func identityBindLoadCode(action identityBindAction, oldID string) (*identityBindCodeChallenge, bool) {
	v, ok := globalIdentityBindCodes.Load(identityBindCodeKey(action, oldID))
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

// identityBindUseVerificationCode 是否启用私聊验证码。
// 开启后，答题环节默认被跳过（可以再显式打开两者并用）。
func identityBindUseVerificationCode(d *Dice) bool {
	return d != nil && d.Config.IdentityBindUseVerificationCode
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

// identityBindQuizStillRequired 是否在做完验证码之后还要求答题。
// 默认不要求：验证码已经证明了归属，再答题只会增加摩擦。
func identityBindQuizStillRequired(d *Dice) bool {
	return d != nil && d.Config.IdentityBindKeepQuiz
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
		ep := identityBindFindOldBotEndPoint(session, nil)
		if ep == nil {
			// 没有民间 bot 连接就没法发验证码。
			// 不要在这里反复刷屏，标记一次让用户知道原因即可。
			if c.Reason == "" {
				c.Reason = "没有可用的民间 bot（OneBot）连接，无法发送验证码"
			}
			continue
		}

		who := "【鲸娘与豹】身份绑定验证码"
		body := fmt.Sprintf(
			"%s\n\n有人正在 QQ 官方机器人上把身份绑定到你这个号（%s）。\n"+
				"如果**是你本人**在操作，请把下面的验证码私聊发给我：\n\n"+
				"    %s\n\n"+
				"验证码 %s 内有效，不是本人操作请直接忽略本条消息。",
			who, c.Old.UserID, c.Code, identityBindFormatDuration(identityBindCodeExpiry(d)))

		if err := identityBindSendPrivate(d, ep, c.DeliverTo, body); err != nil {
			c.Reason = "验证码发送失败: " + err.Error()
			continue
		}
		c.Status = identityBindCodeDelivered
		c.SentByEP = ep.ID
		c.DeliveredAt = time.Now().Unix()
		c.Reason = ""
		d.Logger.Infof("身份绑定验证码已投递: 目标=%s 发起者=%s 端点=%s",
			c.Old.UserID, c.New.UserID, ep.ID)
	}
}

// ---------- 民间 bot 侧：接收验证码 ----------

// identityBindTryConsumeCode 处理一条私聊消息，看它是不是验证码回复。
//
// 返回 handled=true 表示这条消息就是验证码回复，调用方不应再当普通指令处理。
//
// 注意：这个函数在**民间 bot**（OneBot）侧被调用。它要负责：
//  1. 校验发信人确实是挑战里声明的那个旧账号（ConfirmBy）；
//  2. 校验验证码；
//  3. 真正建立绑定（此时才知道官方侧发起者是谁）；
//  4. 通过官方 bot 回执，让发起者知道结果。
func identityBindTryConsumeCode(ctx *MsgContext, msg *Message, text string) bool {
	if ctx == nil || ctx.Dice == nil || msg == nil {
		return false
	}
	d := ctx.Dice
	if !identityBindEnabled(d) || !identityBindUseVerificationCode(d) {
		return false
	}
	// 只在私聊里认验证码。群聊里一条纯数字消息绝不能被当成验证码吞掉
	// （那会让「1」「2」这类正常发言直接消失）。
	if !ctx.IsPrivate && msg.MessageType != "private" {
		return false
	}
	// 只有非官方端点（民间 bot）才处理，避免官方 bot 收到一串数字就误判
	if identityBindSupported(ctx.EndPoint) {
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

	// 扫描所有已投递、未过期的挑战，找验证码匹配的那一条
	var matched *identityBindCodeChallenge
	var matchedKey string
	globalIdentityBindCodes.Range(func(key string, c *identityBindCodeChallenge) bool {
		if c == nil || c.Status != identityBindCodeDelivered || c.expired() {
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
		// 不做任何提示：私聊里随便发个数字不该被骰子插嘴。
		return false
	}

	// 发信人必须是挑战里指定的确认人
	if matched.ConfirmBy != "" && !identityBindSameQQUser(msg.Sender.UserID, matched.ConfirmBy) {
		// 有人在用错误的号试别人的验证码 —— 记一次失败
		matched.Attempts++
		d.Logger.Warnf("身份绑定验证码发信人不符: 期望=%s 实际=%s 目标=%s",
			matched.ConfirmBy, msg.Sender.UserID, matched.Old.UserID)
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

	// 回执给民间 bot 侧本人
	if matched.Action == identityBindActionGroup {
		identityBindReplyPerson(ctx, msg.Sender.UserID, fmt.Sprintf(
			"验证通过！官方群已经和旧群 %s 绑定，双方现在共用同一份日志。", matched.Old.GroupID))
	} else {
		identityBindReplyPerson(ctx, msg.Sender.UserID, fmt.Sprintf(
			"验证通过！你的官方身份 %s 现在与旧 QQ 号共用同一份数据。", matched.New.UserID))
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
