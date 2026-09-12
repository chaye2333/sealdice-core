package dice

import (
	crand "crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	ds "github.com/sealdice/dicescript"
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

	// identityBindCodeKeepForMaster 等待骰主人工确认的挑战保留多久。
	// 比普通挑战长得多：它卡在"等人操作"，10 分钟就丢掉的话骰主一觉醒来
	// 就再也找不到这条申请了。
	identityBindCodeKeepForMaster = 12 * time.Hour

	// identityBindPassiveReplyWindow 「绑定成功」通知还能当被动回复发出去的时限。
	// 官方平台的被动回复窗口约 5 分钟，这里留 1 分钟余量。
	identityBindPassiveReplyWindow = 4 * time.Minute
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

	// NeedMaster 两条通道都投递不出去，已转成"等骰主人工确认"。
	// 置位后不再自动过期（人等多久都有可能），由 .bind approve / .group bindforce 收尾。
	NeedMaster bool
	// MasterNotifiedAt 通知骰主的时间，0 表示还没通知过。
	// 存在的意义：投递 worker 每 3 秒扫一次，没有这个标记就会疯狂刷骰主私聊。
	MasterNotifiedAt int64
	// MasterNotifiedTo 实际通知到的骰主，用来在回执里说清楚"通知了谁 / 一个都没通知到"
	MasterNotifiedTo []string
	// ConfirmedBy 实际确认这条验证码的身份（审计用：邮箱通道允许代确认）
	ConfirmedBy string

	// NewMsgID 发起绑定时那条指令消息的原始 ID（官方侧）。
	// 用来把"绑定成功"通知当作**被动回复**发出去：官方平台的主动消息有权限/额度限制，
	// 而被动回复（引用一条刚收到的消息）一定能发。超过被动窗口就只能退回主动消息。
	NewMsgID string
	// NewMsgAt 发起时间（NewMsgID 的新鲜度判断用）
	NewMsgAt int64

	// Delivering 是否正在投递（受 identityBindCodesMu 保护）。
	// 作用：指令路径会同步投递一次，worker 每 3 秒也会扫一遍，
	// 没有这个标记同一条挑战会被投两遍（甚至把 Channel 覆盖成错的那个）。
	Delivering bool
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

// identityBindCodesMu 保护**挑战对象内部的字段**（SyncMap 只保护 map 本身）。
//
// 三条并发路径都会碰同一条挑战：
//   - 后台投递 worker（每 3 秒一次 Range）
//   - 消息分发里的验证码拦截 identityBindTryConsumeCode
//   - 指令处理（.bind / .group / .bind approve）
//
// 规矩：**任何**对挑战字段的读写都在这个锁里做；其中绝不能做 I/O
// （SMTP / 私聊可能阻塞数秒，握着锁会把消息分发一起堵死）。
// 投递流程因此是"锁里抢占 + 拷贝快照 → 锁外发消息 → 锁里写回结果"。
var identityBindCodesMu sync.Mutex

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
	identityBindCodesMu.Lock()
	globalIdentityBindCodes.Store(identityBindCodeKey(challenge.Action, owner), challenge)
	identityBindCodesMu.Unlock()
}

// identityBindLoadCode 按**发起者**取挑战。
// 传 userID = 官方侧用户 ID（个人）或真实群 ID（群）。
//
// 返回的是**副本**：调用方可以随便读，不会和后台投递 worker 抢同一块内存。
// 需要改状态请用 identityBindCodeUpdate。
func identityBindLoadCode(action identityBindAction, ownerID string) (*identityBindCodeChallenge, bool) {
	identityBindCodesMu.Lock()
	defer identityBindCodesMu.Unlock()
	v, ok := globalIdentityBindCodes.Load(identityBindCodeKey(action, ownerID))
	if !ok || v == nil {
		return nil, false
	}
	return identityBindCodeCopy(v), true
}

// identityBindCodeCopy 复制一条挑战（字段逐个列出，避免 copylocks）。
// 调用方必须已持有 identityBindCodesMu。
func identityBindCodeCopy(c *identityBindCodeChallenge) *identityBindCodeChallenge {
	if c == nil {
		return nil
	}
	out := &identityBindCodeChallenge{
		Action:           c.Action,
		New:              c.New,
		Old:              c.Old,
		Code:             c.Code,
		DeliverTo:        c.DeliverTo,
		ConfirmBy:        c.ConfirmBy,
		Channel:          c.Channel,
		Status:           c.Status,
		Attempts:         c.Attempts,
		SentByEP:         c.SentByEP,
		SentTo:           c.SentTo,
		CreatedAt:        c.CreatedAt,
		ExpiresAt:        c.ExpiresAt,
		DeliveredAt:      c.DeliveredAt,
		FinishedAt:       c.FinishedAt,
		Reason:           c.Reason,
		NeedMaster:       c.NeedMaster,
		MasterNotifiedAt: c.MasterNotifiedAt,
		ConfirmedBy:      c.ConfirmedBy,
		NewMsgID:         c.NewMsgID,
		NewMsgAt:         c.NewMsgAt,
		Delivering:       c.Delivering,
	}
	if len(c.MasterNotifiedTo) > 0 {
		out.MasterNotifiedTo = append([]string(nil), c.MasterNotifiedTo...)
	}
	return out
}

// identityBindCodeUpdate 在锁里改一条挑战。
//
// 所有对挑战状态的写入都必须走这里：worker（每 3 秒）与消息分发/指令处理
// 会并发碰同一条挑战，之前是直接改字段，属于数据竞争。
func identityBindCodeUpdate(key string, fn func(c *identityBindCodeChallenge)) {
	if fn == nil {
		return
	}
	identityBindCodesMu.Lock()
	defer identityBindCodesMu.Unlock()
	v, ok := globalIdentityBindCodes.Load(key)
	if !ok || v == nil {
		return
	}
	fn(v)
}

// identityBindCleanupCodes 清理已过期 / 已完成的挑战。
func identityBindCleanupCodes() {
	now := time.Now().Unix()
	var toDelete []string
	identityBindCodesMu.Lock()
	globalIdentityBindCodes.Range(func(key string, c *identityBindCodeChallenge) bool {
		if c == nil {
			toDelete = append(toDelete, key)
			return true
		}
		// 正在投递的那条先别动，免得状态被改坏
		if c.Delivering {
			return true
		}
		// 等骰主人工确认的挑战不参与"超时作废"：验证码根本没发出去，
		// 再谈 10 分钟有效期就没有意义了。只按一个很长的兜底时间回收。
		if c.NeedMaster && c.Status != identityBindCodeUsed {
			if c.CreatedAt > 0 && now-c.CreatedAt > int64(identityBindCodeKeepForMaster.Seconds()) {
				toDelete = append(toDelete, key)
			}
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
	identityBindCodesMu.Unlock()
	identityBindPruneTargetThrottle(now)
}

// ---------- 防刷：按目标号限频 ----------

const (
	// identityBindTargetCooldown 同一个目标（被声明的旧号 / 旧群）两次发起验证的最小间隔。
	//
	// 为什么需要：发起一次绑定 = 给那个 QQ 号寄一封邮件 / 发一条私聊。
	// 没有这个限制的话，任何人都能反复 .bind <别人的号>，
	// 拿骰子当"给任意 QQ 邮箱发信"的工具，消耗骰主的 SMTP 信誉
	// （被投诉/被拉黑之后所有通知邮件都废了）与民间 bot 的私聊额度。
	identityBindTargetCooldown = 10 * time.Minute

	// identityBindGlobalRateWindowSec 全局限频窗口：每小时。
	identityBindGlobalRateWindowSec = 3600
	// identityBindGlobalRateLimit 全局窗口内最多发起多少次验证码投递。
	identityBindGlobalRateLimit = 20
)

var (
	// globalIdentityBindTargetLast 目标 -> 上次发起时间，用于按目标限频。
	globalIdentityBindTargetLast SyncMap[string, int64]

	globalIdentityBindRateMu          sync.Mutex
	globalIdentityBindRateWindowStart int64
	globalIdentityBindRateCount       int
)

// identityBindTargetThrottleRemaining 目标还要等多久才能再次发起（0 = 现在可以）。
func identityBindTargetThrottleRemaining(target string, now int64) time.Duration {
	target = strings.TrimSpace(target)
	if target == "" {
		return 0
	}
	last, ok := globalIdentityBindTargetLast.Load(target)
	if !ok || last <= 0 {
		return 0
	}
	deadline := last + int64(identityBindTargetCooldown.Seconds())
	if now >= deadline {
		return 0
	}
	return time.Duration(deadline-now) * time.Second
}

// identityBindChallengeTargetKey 一条挑战的"目标"——被声明的旧号 / 旧群。
// 限频就是按它算的：同一个目标 10 分钟内最多发一次验证码。
func identityBindChallengeTargetKey(c *identityBindCodeChallenge) string {
	if c == nil {
		return ""
	}
	if c.Action == identityBindActionGroup {
		return c.Old.GroupID
	}
	return c.Old.UserID
}

// identityBindMarkTargetAttempt 记一次"给这个目标发过验证码"。
func identityBindMarkTargetAttempt(target string, now int64) {
	target = strings.TrimSpace(target)
	if target == "" {
		return
	}
	globalIdentityBindTargetLast.Store(target, now)
}

// identityBindGlobalRateAllow 全局限频（滑动窗口换成固定窗口，够用且简单）。
func identityBindGlobalRateAllow(now int64) bool {
	globalIdentityBindRateMu.Lock()
	defer globalIdentityBindRateMu.Unlock()
	if now-globalIdentityBindRateWindowStart >= identityBindGlobalRateWindowSec {
		globalIdentityBindRateWindowStart = now
		globalIdentityBindRateCount = 0
	}
	if globalIdentityBindRateCount >= identityBindGlobalRateLimit {
		return false
	}
	globalIdentityBindRateCount++
	return true
}

// identityBindPruneTargetThrottle 清掉过期的限频记录，避免 SyncMap 无限增长。
func identityBindPruneTargetThrottle(now int64) {
	ttl := int64(identityBindTargetCooldown.Seconds()) * 2
	globalIdentityBindTargetLast.Range(func(key string, last int64) bool {
		if last <= 0 || now-last > ttl {
			globalIdentityBindTargetLast.Delete(key)
		}
		return true
	})
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
//  1. 必须启用，且**当前已连上**（State == StateConnected）；
//  2. 能处理带 "QQ:" 前缀的裸 QQ 号（也就是 OneBot / walle-q 这类非官方实现）；
//  3. 排除官方端点本身。
//
// 第 1 条里的"已连上"是必须的：只判断 Enable 会把"配置里存在但没连上"的连接
// 也当成可用，于是验证码根本没发出去，却给用户回一句
// "已通过民间 bot 发送了私聊验证码"——把人引到一个永远收不到码的地方。
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
		// 没连上就发不出去，直接跳过（让调用方退回邮箱通道）
		if ep.State != StateConnected {
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
//
// 说明：适配器的 SendToPerson 不返回错误，所以这里能做的检查就是"端点是不是真的在线"。
// 这一条足以挡住最常见的假成功：民间 bot 根本没连上（或已掉线），
// 消息一条都发不出去，却把挑战标成"已投递"。
func identityBindSendPrivate(d *Dice, ep *EndPointInfo, targetRawID string, text string) error {
	if d == nil || ep == nil || ep.Adapter == nil {
		return errors.New("没有可用的民间 bot 连接")
	}
	if ep.State != StateConnected {
		return fmt.Errorf("民间 bot 连接未在线（端点 %s 当前状态码 %d）", ep.ID, ep.State)
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

// identityBindMailTargetQQ 取「验证码要寄给哪个 QQ 号」。
//
//   - 个人绑定：被声明的旧 QQ 号（用户就是要证明这个号是他的）
//   - 群绑定：旧群的**邀请人**（DeliverTo），因为群号本身推不出邮箱；
//     而邀请人必须是旧群成员，他的 QQ 邮箱能证明"他确实是那个人"。
//
// 注意：两种情况下地址都来自**我们自己数据库里的记录**，不是用户随口填的。
// 这是"邮箱方案仍然有约束力"的前提——否则就变成任意邮箱发信机了。
func identityBindMailTargetQQ(c *identityBindCodeChallenge) string {
	if c == nil {
		return ""
	}
	if c.Action == identityBindActionGroup {
		// 群绑定：寄给邀请人
		if qq := identityBindExtractQQNumber(c.DeliverTo); qq != "" {
			return qq
		}
		// 兜底：老记录里 DeliverTo 可能没填，但 Old.UserID 一般就是邀请人
		return identityBindExtractQQNumber(c.Old.UserID)
	}
	return identityBindExtractQQNumber(c.Old.UserID)
}

// identityBindQQMailAddress 推出收件用的 QQ 邮箱地址。
//
// 为什么用 <QQ号>@qq.com：这是唯一"零配置又有约束力"的方案。
// QQ 邮箱与 QQ 号绑定，所以能收到这封信 ≈ 控制着这个 QQ 号。
// **不会**接受用户自己填的邮箱——那既证明不了归属，又会让骰子变成发信机。
func identityBindQQMailAddress(c *identityBindCodeChallenge) string {
	qq := identityBindMailTargetQQ(c)
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
//
// 返回 error：SMTP 失败必须能传回来，否则骰主 SMTP 配错时用户会一直被告知
// "验证码已寄出"，然后干等一封永远不来的邮件。
var identityBindMailSender = func(d *Dice, subject string, to []string, body string) error {
	return d.SendMailRow(subject, to, body, nil)
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
		return errors.New("邮件配置不完整（需要 mailFrom / mailPassword / mailSmtp 三项齐全）")
	}
	to := identityBindQQMailAddress(c)
	if to == "" {
		return errors.New("无法推出收件用的 QQ 邮箱地址")
	}

	subject := "身份绑定验证码"
	body := fmt.Sprintf(
		"有人正在 QQ 官方机器人上申请绑定（%s）。\n\n"+
			"如果**是你本人**在操作，请把下面的验证码回复给官方机器人：\n\n"+
			"    %s\n\n"+
			"验证码 %s 内有效。\n"+
			"不是本人操作请直接忽略本邮件，绑定不会生效。\n",
		identityBindMailSceneText(c), c.Code, identityBindFormatDuration(identityBindCodeExpiry(d)))

	if err := identityBindMailSender(d, subject, []string{to}, body); err != nil {
		return fmt.Errorf("SMTP 发送失败: %w", err)
	}
	c.SentTo = to
	return nil
}

// identityBindMailSceneText 邮件正文里描述"这次绑定在做什么"。
func identityBindMailSceneText(c *identityBindCodeChallenge) string {
	if c == nil {
		return "身份绑定"
	}
	if c.Action == identityBindActionGroup {
		return fmt.Sprintf("把官方群与旧群 %s 绑定，绑定后两群共用同一份日志", c.Old.GroupID)
	}
	return fmt.Sprintf("把身份绑定到这个 QQ 号（%s）", c.Old.UserID)
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
//
// 并发要点（与 identityBindCodesMu 的注释配套）：
//  1. 锁里只做"抢占 + 拷贝快照"：把 Delivering 置 true 并复制一份挑战出来；
//  2. 锁外发消息（SMTP / 私聊都可能阻塞数秒，绝不能握着锁）；
//  3. 锁里写回结果。
//
// 抢占让"指令路径同步投递一次"与"worker 每 3 秒扫一遍"不会重复发送，
// 也不会把 Channel 覆盖成与实际不符的那个。
func identityBindDeliverPendingCodes(d *Dice) {
	if d == nil || !identityBindEnabled(d) || !identityBindUseVerificationCode(d) {
		return
	}
	session := d.ImSession
	if session == nil {
		return
	}

	type pending struct {
		key  string
		snap *identityBindCodeChallenge
	}
	var todo []pending
	identityBindCodesMu.Lock()
	globalIdentityBindCodes.Range(func(key string, c *identityBindCodeChallenge) bool {
		if c == nil || c.Delivering || c.Status != identityBindCodePending || c.expired() {
			return true
		}
		c.Delivering = true
		todo = append(todo, pending{key: key, snap: identityBindCodeCopy(c)})
		return true
	})
	identityBindCodesMu.Unlock()
	if len(todo) == 0 {
		return
	}

	for _, item := range todo {
		// 注意：c 是**快照**，改它不会影响全局那条；结果由 finishDelivery 写回。
		c := item.snap

		// 两条通道按优先级依次尝试。
		// 默认私聊优先，骰主打开 IdentityBindPreferEmailCode 后改为邮箱优先。
		//
		// 群绑定也能走邮箱：收件人是旧群的**邀请人**（见 identityBindMailTargetQQ）。
		// 之前这里写死了「只有个人绑定能用邮箱」，导致骰主放弃民间 bot 之后
		// 群绑定彻底无路可走，属于设计错误。
		emailUsable := identityBindEmailCodeUsable(d) && identityBindMailTargetQQ(c) != ""

		var (
			delivered bool
			sentTo    string
			sentByEP  string
		)

		tryEmail := func() bool {
			if !emailUsable {
				return false
			}
			if err := identityBindSendEmailCode(d, c); err != nil {
				c.Reason = "邮箱投递失败: " + err.Error()
				return false
			}
			c.Channel = identityBindCodeChannelEmail
			sentTo = c.SentTo
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
			c.Channel = identityBindCodeChannelDM
			sentByEP = ep.ID
			sentTo = c.DeliverTo
			d.Logger.Infof("身份绑定验证码已私聊投递: 目标=%s 发起者=%s 端点=%s",
				c.Old.UserID, c.New.UserID, ep.ID)
			return true
		}

		// 按优先级依次尝试两条通道；先成功的算数。
		// 默认私聊优先，骰主把 IdentityBindPreferEmailCode 打开后邮箱优先。
		if identityBindPreferEmailCode(d) && emailUsable {
			// 邮箱优先：邮箱失败仍然退回私聊，不让邮件问题阻断绑定
			delivered = tryEmail() || tryDM()
		} else {
			delivered = tryDM() || tryEmail()
		}

		reason := c.Reason
		if delivered {
			// 真正发出去了才记限频额度：防的是"拿骰子当发信机"，
			// 而不是"用户想重试"。
			identityBindMarkTargetAttempt(identityBindChallengeTargetKey(c), time.Now().Unix())
			identityBindFinishDelivery(item.key, identityBindCodeDelivered, c.Channel, sentTo, sentByEP, "", false, nil)
			continue
		}

		// 两条通道都不可用：把原因说清楚，别反复刷屏。
		// 只在还没有原因时才写通用文案——tryEmail / tryDM 已经写进具体错误
		// （比如 "SMTP 发送失败: dial tcp ..."），覆盖掉它只会让骰主无从排查。
		if reason == "" {
			switch {
			case c.Action == identityBindActionGroup:
				reason = "没有可用的民间 bot（OneBot）连接，群绑定无法投递验证码"
			case emailUsable:
				reason = "验证码投递失败，请稍后重试（私聊与邮箱都试过了）"
			default:
				reason = "没有可用的民间 bot（OneBot）连接，邮箱也没配好；" +
					"请在管理界面配置「邮箱通知」（发件邮箱 / 密钥 / SMTP）作为备用通道"
			}
		}

		// 转人工：两条通道都不通时，私聊把这件事告诉骰主，让他来确认。
		// 只通知一次——worker 每 3 秒跑一遍，不做标记会把骰主私聊刷爆。
		// 这里先落 NeedMaster 再通知（锁内），保证并发的另一次投递不会重复通知。
		c.Reason = reason
		c.NeedMaster = true
		c.Channel = identityBindCodeChannelNone
		identityBindFinishDelivery(item.key, identityBindCodePending, identityBindCodeChannelNone, "", "", reason, true, nil)

		if identityBindClaimMasterNotify(item.key) {
			notified := identityBindNotifyMasters(d, c)
			identityBindCodeUpdate(item.key, func(live *identityBindCodeChallenge) {
				live.MasterNotifiedAt = time.Now().Unix()
				live.MasterNotifiedTo = notified
			})
			if len(notified) > 0 {
				d.Logger.Infof("身份绑定已转人工确认并通知骰主: 目标=%s 骰主=%s",
					identityBindChallengeTargetText(c), strings.Join(notified, ","))
			} else {
				d.Logger.Warnf("身份绑定投递失败且无法通知到任何骰主: 目标=%s 原因=%s",
					identityBindChallengeTargetText(c), reason)
			}
		}
	}
}

// identityBindFinishDelivery 把投递结果写回全局那条挑战，并解除"投递中"标记。
func identityBindFinishDelivery(
	key string, status identityBindCodeStatus, channel identityBindCodeChannel,
	sentTo, sentByEP, reason string, needMaster bool, notifiedTo []string,
) {
	identityBindCodeUpdate(key, func(c *identityBindCodeChallenge) {
		c.Delivering = false
		c.Channel = channel
		c.Status = status
		if sentTo != "" {
			c.SentTo = sentTo
		}
		c.SentByEP = sentByEP
		c.Reason = reason
		if status == identityBindCodeDelivered {
			c.DeliveredAt = time.Now().Unix()
			c.NeedMaster = false
		}
		if needMaster {
			c.NeedMaster = true
		}
		if notifiedTo != nil {
			c.MasterNotifiedTo = notifiedTo
		}
	})
}

// identityBindClaimMasterNotify 抢占"通知骰主"这件事，true 表示这次由我来通知。
//
// 之前是 `if c.MasterNotifiedAt == 0 { c.MasterNotifiedAt = now; ... }`：
// 判断与赋值不在一个临界区里，两个 goroutine 可能同时通过判断，把骰主私聊刷两遍。
func identityBindClaimMasterNotify(key string) bool {
	claimed := false
	identityBindCodeUpdate(key, func(c *identityBindCodeChallenge) {
		if c.MasterNotifiedAt != 0 {
			return
		}
		c.MasterNotifiedAt = time.Now().Unix()
		claimed = true
	})
	return claimed
}

// identityBindChallengeTargetText 用一句话描述"这条挑战在绑定什么"，用于日志。
func identityBindChallengeTargetText(c *identityBindCodeChallenge) string {
	if c == nil {
		return ""
	}
	if c.Action == identityBindActionGroup {
		return fmt.Sprintf("%s -> 旧群 %s", c.New.GroupID, c.Old.GroupID)
	}
	return fmt.Sprintf("%s -> 旧QQ %s", c.New.UserID, c.Old.UserID)
}

// ---------- 骰主人工兜底 ----------

// identityBindNotifyMasters 两条通道都投递不出去时，私聊通知所有骰主来人工确认。
//
// 为什么需要它：验证码依赖"民间 bot 在线"或"邮箱配好"，现实中两者都可能没有。
// 这时如果只回一句"投递失败"，用户就彻底卡住了；而骰主本来就有
// `.bind approve` / `.group bindforce` 这个口子，缺的只是"骰主不知道有人在等"。
//
// 可达性（骰主 ID 是"平台:账号"格式，见 Dice.DiceMasters）：
//   - QQ:123456    → 走民间 bot 私聊。官方 bot 只能按 OpenID 发，发不了裸 QQ 号，
//     所以"民间 bot 没连上"时这条通知确实发不出去（下面会如实报告）。
//   - OpenQQ:...   → 走官方 bot 私聊。
//   - UI:1001 等   → 不是聊天账号，跳过。
//
// 返回实际通知成功的骰主 ID 列表（可能是空）。
func identityBindNotifyMasters(d *Dice, c *identityBindCodeChallenge) []string {
	if d == nil || c == nil {
		return nil
	}
	var notified []string
	text := identityBindMasterNoticeText(c)
	seen := map[string]bool{}
	for _, masterID := range d.DiceMasters {
		id := strings.TrimSpace(masterID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if err := identityBindNotifyOneMaster(d, id, text); err != nil {
			d.Logger.Infof("通知骰主失败: 骰主=%s 原因=%v", id, err)
			continue
		}
		notified = append(notified, id)
	}
	return notified
}

// identityBindNotifyOneMaster 按骰主 ID 的形式挑一条能到达他的连接并私聊。
func identityBindNotifyOneMaster(d *Dice, masterID, text string) error {
	if d == nil || d.ImSession == nil {
		return errors.New("会话未初始化")
	}
	id := strings.TrimSpace(masterID)
	if id == "" {
		return errors.New("空的骰主标识")
	}
	// WebUI 里的骰主（UI:1001）不是一个能收私聊的聊天账号
	if strings.HasPrefix(strings.ToUpper(id), "UI:") {
		return errors.New("WebUI 账号不是聊天账号")
	}
	if isOfficialQQID(id) {
		ep := identityBindFindNewBotEndPoint(d.ImSession)
		if ep == nil {
			return errors.New("没有可用的官方 bot 连接")
		}
		return identityBindSendPrivate(d, ep, id, text)
	}
	if identityBindExtractQQNumber(id) == "" {
		return fmt.Errorf("无法识别的骰主标识 %q", masterID)
	}
	ep := identityBindFindOldBotEndPoint(d.ImSession, nil)
	if ep == nil {
		return errors.New("没有在线的民间 bot（OneBot）连接，发不了 QQ 私聊")
	}
	return identityBindSendPrivate(d, ep, id, text)
}

// identityBindMasterNoticeText 给骰主的通知正文：说清楚谁在申请什么、为什么自动通道失败了、
// 以及骰主该敲哪条指令。
func identityBindMasterNoticeText(c *identityBindCodeChallenge) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("【身份绑定 · 需要你人工确认】\n")
	if c.Action == identityBindActionGroup {
		fmt.Fprintf(&b, "有人申请把官方群 %s 绑定到旧群 %s。\n",
			c.New.GroupID, identityBindBareNumber(c.Old.GroupID))
	} else {
		fmt.Fprintf(&b, "有人申请把官方身份 %s 绑定到旧 QQ 号 %s。\n",
			c.New.UserID, identityBindBareNumber(c.Old.UserID))
	}
	fmt.Fprintf(&b, "发起者: %s\n", c.New.UserID)
	if c.Reason != "" {
		fmt.Fprintf(&b, "自动投递失败原因: %s\n", c.Reason)
	}
	b.WriteString("\n验证码发不出去（民间 bot 不在线，或邮箱没配好），所以需要你核实身份后手动确认。\n")
	if c.Action == identityBindActionGroup {
		fmt.Fprintf(&b, "确认指令: .group bindforce %s\n", identityBindBareNumber(c.Old.GroupID))
	} else {
		fmt.Fprintf(&b, "确认指令: .bind approve %s\n", identityBindBareNumber(c.Old.UserID))
	}
	b.WriteString("也可以用 .bind pending 查看当前所有待确认申请。\n")
	b.WriteString("⚠️ 手动确认会**跳过验证码**，请先自行核实对方确实是那个号 / 那个群的主人。")
	return b.String()
}

// identityBindBareNumber 把 "QQ:123" / "QQ-Group:456" / "OpenQQ:1-2" 里的号码部分取出来。
func identityBindBareNumber(id string) string {
	if n := identityBindExtractQQNumber(id); n != "" {
		return n
	}
	return id
}

// identityBindMaskNumber 把号码或邮箱地址打码，只留头尾便于本人确认。
//
// 用在**群聊回复**里：群绑定的收件人是旧群邀请人，他的 QQ 号 / 邮箱
// 不该被官方群里所有人看到（第三方信息泄露）。
//
//	2431692084        -> 243****084
//	2431692084@qq.com -> 243****084@qq.com
func identityBindMaskNumber(value string) string {
	s := strings.TrimSpace(value)
	if s == "" {
		return ""
	}
	// 邮箱只打码 @ 前面的本地部分
	at := strings.LastIndex(s, "@")
	if at > 0 {
		return identityBindMaskNumber(s[:at]) + s[at:]
	}
	runes := []rune(s)
	if len(runes) <= 4 {
		return strings.Repeat("*", len(runes))
	}
	if len(runes) <= 7 {
		return string(runes[:2]) + strings.Repeat("*", len(runes)-2)
	}
	return string(runes[:3]) + "****" + string(runes[len(runes)-3:])
}

// ---------- 待确认申请（骰主视角） ----------

// identityBindPendingChallenges 列出还等着处理的申请，按登记时间从早到晚。
//
// onlyNeedMaster 为 true 时只列"两条通道都没投递出去、已经转人工"的那批；
// 为 false 时把"已投递但对方还没回复"的也算上，方便骰主排查到底卡在哪一步。
func identityBindPendingChallenges(onlyNeedMaster bool) []*identityBindCodeChallenge {
	var list []*identityBindCodeChallenge
	identityBindCodesMu.Lock()
	defer identityBindCodesMu.Unlock()
	globalIdentityBindCodes.Range(func(_ string, c *identityBindCodeChallenge) bool {
		if c == nil || c.Status == identityBindCodeUsed {
			return true
		}
		if onlyNeedMaster {
			if !c.NeedMaster {
				return true
			}
		} else if !c.NeedMaster && c.expired() {
			// 已投递但过期的就不列了；转人工的不受有效期约束
			return true
		}
		// 返回副本：骰主指令接下来会读这些字段，不能和 worker 共用一个对象
		list = append(list, identityBindCodeCopy(c))
		return true
	})
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt })
	return list
}

// identityBindFindPendingChallenge 按"被声明的旧号 / 旧群"找一条待确认的申请。
//
// 这里用 identityBindSameIdentity 比较，所以 .bind approve 123456、
// .bind approve QQ:123456、带不带前缀都能对上。
func identityBindFindPendingChallenge(action identityBindAction, rawTarget string) (*identityBindCodeChallenge, bool) {
	target := strings.TrimSpace(rawTarget)
	if target == "" {
		return nil, false
	}
	for _, c := range identityBindPendingChallenges(false) {
		if c.Action != action {
			continue
		}
		oldID := c.Old.UserID
		if action == identityBindActionGroup {
			oldID = c.Old.GroupID
		}
		if identityBindSameIdentity(oldID, target) {
			return c, true
		}
	}
	return nil, false
}

// identityBindCommitChallenge 把一条待确认的申请直接落成绑定记录（骰主人工确认用）。
//
// 注意：绑定记录用的是**挑战里登记的发起者身份**（c.New），不是骰主自己的身份。
// 骰主只是"替这个申请人作证"，绝不能把群绑到自己头上。
func identityBindCommitChallenge(d *Dice, c *identityBindCodeChallenge) error {
	if d == nil || c == nil {
		return errors.New("上下文为空")
	}
	record := &identityBindRecord{
		Action:  c.Action,
		New:     c.New,
		Old:     c.Old,
		Created: time.Now().Unix(),
		Creator: c.New.UserID,
	}
	if err := identityBindStoreOf(d).put(d, record); err != nil {
		return err
	}
	// 状态改动走锁：这条挑战可能正被 worker 或消息分发处理
	key := identityBindCodeKey(c.Action, identityBindChallengeOwnerID(c.Action, c.New))
	identityBindCodeUpdate(key, func(live *identityBindCodeChallenge) {
		live.Status = identityBindCodeUsed
		live.NeedMaster = false
		live.Delivering = false
		live.FinishedAt = time.Now().Unix()
		live.Reason = "骰主手动确认"
	})
	return nil
}

// identityBindDescribeChallenge 把一条申请渲染成一行可读文本。
func identityBindDescribeChallenge(c *identityBindCodeChallenge) string {
	if c == nil {
		return ""
	}
	when := time.Unix(c.CreatedAt, 0).Format("01-02 15:04")
	if c.Action == identityBindActionGroup {
		return fmt.Sprintf("· 群绑定 %s → 旧群 %s  %s  [%s]",
			c.New.GroupID, identityBindBareNumber(c.Old.GroupID), identityBindPendingStageText(c), when)
	}
	return fmt.Sprintf("· 个人绑定 %s → 旧QQ %s  %s  [%s]",
		c.New.UserID, identityBindBareNumber(c.Old.UserID), identityBindPendingStageText(c), when)
}

// identityBindPendingStageText 描述一条申请当前卡在哪一步。
func identityBindPendingStageText(c *identityBindCodeChallenge) string {
	if c == nil {
		return ""
	}
	if c.NeedMaster {
		return "已转人工，等骰主确认"
	}
	switch c.Channel {
	case identityBindCodeChannelDM:
		return "已私聊投递，等对方回复"
	case identityBindCodeChannelEmail:
		return "已邮件投递，等对方回复"
	case identityBindCodeChannelNone:
		return "待投递"
	default:
		return "待投递（未知通道）"
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

	// 扫描所有已投递、未过期的挑战，找验证码匹配、且**通道与当前端点相符**的那一条。
	//
	// 整段都在锁里：这是与后台投递 worker 唯一的交汇点。锁里只读不写 I/O，
	// 提示与通知都等到解锁之后再发。
	var matched *identityBindCodeChallenge
	var matchedKey string
	// 码猜错时也要计次（见下面 identityBindIsExpectedConfirmer）：
	// 邮箱通道曾经完全没有失败上限，4 位码 + 1 小时有效期时是可以被硬猜穿的。
	var wrongGuessKeys []string
	var expiredByAttempts *identityBindCodeChallenge
	identityBindCodesMu.Lock()
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
		if identityBindIsExpectedConfirmer(c, msg.Sender.UserID, onOfficial, isPrivate) {
			wrongGuessKeys = append(wrongGuessKeys, key)
		}
		return true
	})
	if matched != nil {
		matched = identityBindCodeCopy(matched)
	}
	identityBindCodesMu.Unlock()

	// 猜错了：只对"本来就该由这个人确认"的挑战计次。
	// 为什么不给所有人计次——群里随便一个人连发五个数字就能把别人的验证码作废，
	// 那是白送的骚扰手段。
	//
	// 注意：这一段必须在**解锁之后**做，identityBindCodeUpdate 自己会加锁。
	for _, key := range wrongGuessKeys {
		identityBindCodeUpdate(key, func(c *identityBindCodeChallenge) {
			if c.Status != identityBindCodeDelivered {
				return
			}
			c.Attempts++
			c.Reason = fmt.Sprintf("验证码错误 %d/%d 次", c.Attempts, identityBindCodeMaxAttempts)
			if c.Attempts >= identityBindCodeMaxAttempts {
				c.Status = identityBindCodeUsed
				c.FinishedAt = time.Now().Unix()
				c.Reason = "验证码错误次数过多，已作废"
				expiredByAttempts = identityBindCodeCopy(c)
			}
		})
	}

	if expiredByAttempts != nil {
		d.Logger.Warnf("身份绑定验证码错误次数过多已作废: 目标=%s 发起者=%s",
			identityBindChallengeTargetText(expiredByAttempts), expiredByAttempts.New.UserID)
		identityBindReplyPerson(ctx, msg.Sender.UserID,
			"验证码错误次数过多，这条申请已作废。请重新发起绑定。")
	}
	if matched == nil {
		// 不是验证码回复，交回给正常指令流程。
		// 不做任何提示：随手发个数字不该被骰子插嘴。
		return false
	}

	// 决定"谁有资格确认"
	wantConfirmer := matched.ConfirmBy
	if matched.Channel == identityBindCodeChannelEmail {
		// 邮箱码：**不再要求必须是发起者本人回复**。三条理由：
		//  1. 官方平台的"群内 OpenID"和"私聊 OpenID"不是同一个值。在群里发起绑定、
		//     再到私聊里回复验证码，就会被误判成"不是本人"
		//     （表现就是那句"这个验证码不是发给你的，请让本人用他自己的号回复"）；
		//  2. 群绑定的码是寄给**旧群邀请人**的，而张罗群绑定的往往是群里另一位管理，
		//     要求"回复者 == 发起者"会把群绑定的邮箱通道整条堵死；
		//  3. 真正证明身份的是"能不能拿到那封邮件"，不是"谁按的发送键"。
		//     而且落库用的是挑战里登记的发起者身份（matched.New），
		//     旁人代回也只会把绑定发给发起者，抢不走。
		//
		// 私聊通道（民间 bot）仍然严格校验发信人：码发到哪个号，就得哪个号回，
		// 那才是"持有旧号"的证明。
		wantConfirmer = ""
	}
	if wantConfirmer != "" && !identityBindSameIdentity(msg.Sender.UserID, wantConfirmer) {
		// 有人在用错误的号试别人的验证码 —— 记一次失败（状态改动走锁）
		identityBindCodeUpdate(matchedKey, func(c *identityBindCodeChallenge) {
			c.Attempts++
			if c.Attempts >= identityBindCodeMaxAttempts {
				c.Status = identityBindCodeUsed
				c.FinishedAt = time.Now().Unix()
				c.Reason = "尝试次数过多"
			}
		})
		d.Logger.Warnf("身份绑定验证码发信人不符: 期望=%s 实际=%s 目标=%s",
			wantConfirmer, msg.Sender.UserID, matched.Old.UserID)
		identityBindReplyPerson(ctx, msg.Sender.UserID,
			"这个验证码不是发给你的，请让本人用他自己的号回复。")
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
	// 落库前再查一次唯一性：从"发起挑战"到"确认"之间可能隔了很久，
	// 期间同一个旧号/旧群可能已经被别的挑战绑走了。两个挑战都提交会破坏
	// "一个旧身份只属于一个人"这条不变量。
	oldID := matched.Old.UserID
	newID := matched.New.UserID
	if matched.Action == identityBindActionGroup {
		oldID = matched.Old.GroupID
		newID = matched.New.GroupID
	}
	if existing, ok := identityBindStoreOf(d).find(d, oldID); ok {
		identityBindFinishChallenge(matchedKey, "确认时该旧身份已被绑定")
		identityBindReplyPerson(ctx, msg.Sender.UserID, fmt.Sprintf(
			"绑定失败：%s 已经被绑定到 %s 了，请先解除那边的绑定。",
			oldID, identityBindRecordEndpointID(existing)))
		return true
	}
	if existing, ok := identityBindStoreOf(d).find(d, newID); ok {
		identityBindFinishChallenge(matchedKey, "确认时发起者已有绑定")
		identityBindReplyPerson(ctx, msg.Sender.UserID, fmt.Sprintf(
			"绑定失败：当前身份已经绑定了 %s，请先发送 `.unbind`。",
			identityBindRecordOldID(existing)))
		return true
	}
	if err := identityBindStoreOf(d).put(d, record); err != nil {
		identityBindCodeUpdate(matchedKey, func(c *identityBindCodeChallenge) { c.Attempts++ })
		identityBindReplyPerson(ctx, msg.Sender.UserID, fmt.Sprintf("绑定保存失败: %v", err))
		return true
	}

	identityBindCodeUpdate(matchedKey, func(c *identityBindCodeChallenge) {
		c.Status = identityBindCodeUsed
		c.FinishedAt = time.Now().Unix()
		c.Reason = "验证码确认成功"
		// 审计：邮箱通道允许代确认，所以记下到底是谁回的这个码
		c.ConfirmedBy = msg.Sender.UserID
	})
	if matched.Channel == identityBindCodeChannelEmail && !identityBindSameIdentity(msg.Sender.UserID, matched.New.UserID) {
		d.Logger.Infof("身份绑定验证码由非发起者回复: 确认人=%s 发起者=%s 目标=%s",
			msg.Sender.UserID, matched.New.UserID, identityBindChallengeTargetText(matched))
	}

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
	//
	// 两种通道都回：这是**被动回复**（引用对方刚发的那条消息），一定能发出去，
	// 是"绑定已完成"最可靠的一次确认。之前只给私聊通道回执，
	// 而邮箱通道靠官方群里的主动通知——那条通知受"群内主动发言"权限/额度限制，
	// 发不出去时用户就完全收不到任何反馈（用户实测正是这个症状）。
	if matched.Action == identityBindActionGroup {
		identityBindReplyPerson(ctx, msg.Sender.UserID, fmt.Sprintf(
			"验证通过！官方群已经和旧群 %s 绑定，双方现在共用同一份日志。", matched.Old.GroupID))
	} else {
		identityBindReplyPerson(ctx, msg.Sender.UserID, fmt.Sprintf(
			"验证通过！你的官方身份 %s 现在与旧 QQ 号 %s 共用同一份数据。",
			matched.New.UserID, matched.Old.UserID))
	}

	// 通知官方 bot 侧的发起者
	identityBindNotifyNewSide(d, matched)

	d.LastUpdatedTime = time.Now().Unix()
	return true
}

// identityBindFinishChallenge 把一条挑战标记为已完结（状态改动走锁）。
func identityBindFinishChallenge(key, reason string) {
	identityBindCodeUpdate(key, func(c *identityBindCodeChallenge) {
		c.Status = identityBindCodeUsed
		c.FinishedAt = time.Now().Unix()
		c.Reason = reason
		c.Delivering = false
	})
}

// identityBindNotifyNewSide 通过官方 bot 把结果告诉发起绑定的人。
//
// 这里**刻意不走 ReplyGroup**。ReplyGroup 是给"用户刚发来的那条消息"用的，
// 它假设 ctx.Player / ctx.Group 都是活生生的对象（要过限流、敏感词、文案模板、
// 状态栏……）。而后台任务是"没有来消息也要说话"，之前只拼了一个只填 GroupID 的
// 壳 GroupInfo，结果官方适配器为了发被动消息去取 `$tMsgID`，
// 在 VarGetValue 里读 ctx.Player.ValueMapTemp 直接 nil 解引用 panic，
// 而官方 SDK 会让这个 panic 冲掉整条 websocket 连接（日志里表现为
// close 4004 / invalid session，机器人得重连）。
//
// 所以这里明确地"自己拼一个上下文，然后直接调适配器"：
// Player 一定非 nil，Group 拿不到真实对象时就置 nil（适配器会跳过需要群的分支）。
//
// 发送方式：优先当**被动回复**（引用发起绑定那条指令消息），因为官方平台的
// 主动群消息受"主动发言"权限与额度限制，常常发不出去（用户实测正是这个症状）；
// 超出被动窗口时再退回主动消息，并把结果写进日志便于排查。
func identityBindNotifyNewSide(d *Dice, c *identityBindCodeChallenge) {
	if d == nil || c == nil || c.New.GroupID == "" && c.New.UserID == "" {
		return
	}
	ep := identityBindFindNewBotEndPoint(d.ImSession)
	if ep == nil || ep.Adapter == nil {
		if d.Logger != nil {
			d.Logger.Warnf("身份绑定成功通知未发出：官方 bot 连接不可用（目标=%s）",
				identityBindChallengeTargetText(c))
		}
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

	if c.New.GroupID == "" && c.New.UserID == "" {
		// 没地方可发（理论上不会出现），至少别让它崩
		return
	}
	// 私聊里发起的绑定：New.GroupID 是 "PG-<用户ID>" 这种**伪群号**，
	// 发给 SendToGroup 会被判成 Unknown 只剩一行错误日志，用户什么也收不到。
	// 这种情况直接私聊回复本人。
	if strings.HasPrefix(c.New.GroupID, "PG-") || c.New.GroupID == "" {
		sendCtx := identityBindSafeSendCtx(d, ep, "", c.New.UserID, c.NewMsgID)
		ep.Adapter.SendToPerson(sendCtx, c.New.UserID, text, "skip")
		return
	}

	replyMsgID := identityBindPassiveReplyMsgID(c)
	sendCtx := identityBindSafeSendCtx(d, ep, c.New.GroupID, c.New.UserID, replyMsgID)
	if d.Logger != nil {
		mode := "主动消息"
		if replyMsgID != "" {
			mode = "被动回复"
		}
		d.Logger.Infof("发送身份绑定成功通知: 群=%s 方式=%s", c.New.GroupID, mode)
	}
	ep.Adapter.SendToGroup(sendCtx, c.New.GroupID, text, "skip")
}

// identityBindPassiveReplyMsgID 判断能否把通知当作被动回复发出。
//
// 官方平台的被动回复要求"引用一条最近收到的消息"（窗口约 5 分钟、每个消息 ID 有次数上限）。
// 这里留 4 分钟余量：超过就老实走主动消息，免得撞上"msg_id 已过期"。
func identityBindPassiveReplyMsgID(c *identityBindCodeChallenge) string {
	if c == nil || c.NewMsgID == "" || c.NewMsgAt <= 0 {
		return ""
	}
	if time.Since(time.Unix(c.NewMsgAt, 0)) > identityBindPassiveReplyWindow {
		return ""
	}
	return c.NewMsgID
}

// identityBindSafeSendCtx 为"后台主动通知"拼一个不会 panic 的群上下文。
//
// 与正常消息路径的区别：
//   - Group 取内存里的真实群对象；取不到就留 nil（官方适配器会在 ctx.Group == nil
//     时跳过取 `$tMsgID` 那一段，转而走主动消息），绝不塞壳对象；
//   - Player **保证非 nil**：上游有多处会直接读 ctx.Player 的字段，
//     nil 会 panic 并连带干掉官方 bot 的连接。拿不到真实玩家就放一个占位；
//   - replyMsgID 非空时塞进 `$tMsgID`，让官方适配器把它当**被动回复**发出
//     （主动消息受"群内主动发言"权限/额度限制，常常根本发不出去）。
func identityBindSafeSendCtx(d *Dice, ep *EndPointInfo, groupID, userID, replyMsgID string) *MsgContext {
	// 私聊（groupID 为空）时要标成 private：官方适配器的 SendToPerson
	// 只在 ctx.MessageType == "private" 且 ctx.Player.UserID 对得上时
	// 才把消息当被动回复发。
	msgType := "group"
	isPrivate := false
	if groupID == "" {
		msgType = "private"
		isPrivate = true
	}
	ctx := &MsgContext{
		Dice:            d,
		EndPoint:        ep,
		MessageType:     msgType,
		IsPrivate:       isPrivate,
		IsCurGroupBotOn: true,
	}
	if ep != nil {
		ctx.Session = ep.Session
	}
	if ep != nil && ep.Session != nil && groupID != "" {
		if group, ok := ep.Session.ServiceAtNew.Load(groupID); ok && group != nil {
			ctx.Group = group
			if userID != "" {
				if p := group.PlayerGet(d.DBOperator, userID); p != nil {
					ctx.Player = p
				}
			}
		}
	}
	if ctx.Player == nil {
		ctx.Player = &GroupPlayerInfo{UserID: userID}
	}
	if replyMsgID != "" {
		// 与官方适配器自己的 newEventMsgContext 用同一套写法（见 platform_adapter_official_qq.go），
		// 这样 SendToGroup 里的 VarGetValueStr(ctx, "$tMsgID") 就能取到值。
		ctx.vm = ds.NewVM()
		ctx.vm.Attrs.Store("$tMsgID", ds.NewStrVal(replyMsgID))
	}
	return ctx
}

// identityBindFindNewBotEndPoint 找官方 bot 那条连接（用于发通知）。
//
// 与 identityBindFindOldBotEndPoint 一样，**必须**要求 State == StateConnected。
// 原因很硬：官方适配器连接失败时走 failConnect()，它把 pa.Api 置 nil、State 置 3，
// 但**不会**清 Enable（配置里那条连接还在）。此时如果照发消息，
// 适配器内部会调 nil 接口的 PostGroupMessage → panic，
// 而这个 panic 发生在 botgo 的事件 goroutine 里，会把整条 websocket 带走
// （日志表现为 close 4004 / invalid session，机器人得重连）。
func identityBindFindNewBotEndPoint(session *IMSession) *EndPointInfo {
	if session == nil {
		return nil
	}
	for _, ep := range session.EndPoints {
		if ep == nil || !ep.Enable || ep.Adapter == nil {
			continue
		}
		if ep.State != StateConnected {
			continue
		}
		if identityBindSupported(ep) {
			return ep
		}
	}
	return nil
}

// identityBindIsExpectedConfirmer 判断"这条猜错的码"该不该记到这条挑战头上。
//
// 只有"本来就有资格确认的人"猜错才计次：
//
//	邮箱通道 → 发起者本人（码进了他声称的那个号的邮箱，他最有动机去猜）
//	私聊通道 → 被声明的旧账号
//
// 换成"任何人都计次"会有反效果：群里任意一个人连发五个数字，
// 就能把别人的验证码作废掉。
func identityBindIsExpectedConfirmer(c *identityBindCodeChallenge, senderID string, onOfficial, isPrivate bool) bool {
	if c == nil || strings.TrimSpace(senderID) == "" {
		return false
	}
	switch c.Channel {
	case identityBindCodeChannelEmail:
		return onOfficial && identityBindSameIdentity(senderID, c.New.UserID)
	case identityBindCodeChannelDM:
		return !onOfficial && isPrivate && identityBindSameIdentity(senderID, c.ConfirmBy)
	default:
		return false
	}
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
