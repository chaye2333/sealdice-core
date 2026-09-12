//nolint:testpackage
package dice

import (
	"strings"
	"sync"
	"testing"
	"time"

	ds "github.com/sealdice/dicescript"
)

// 这一组针对第二轮排查里确认的高危问题：
//
//	1. 官方端点掉线（Enable 仍为 true）时，通知路径会调 nil 接口 → panic → 冲掉连接；
//	2. 群里任何普通成员都能 .group unbind / .group cancel，越权破坏群绑定；
//	3. 邮箱通道没有任何失败次数上限，4 位码 + 长有效期时可以被硬猜穿；
//	4. 绑群之后 .log end / .log del / .stat log 仍在真实群上读写。

// TestIdentityBindNotifyNewSideSkipsDisconnectedOfficialBot
// 官方端点"配置还在、其实已掉线"时，通知必须直接放弃，绝不能碰适配器。
func TestIdentityBindNotifyNewSideSkipsDisconnectedOfficialBot(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()

	env.ctx.EndPoint.State = StateConnectionFailed // failConnect() 之后的样子
	if ep := identityBindFindNewBotEndPoint(env.d.ImSession); ep != nil {
		t.Fatalf("a disconnected official endpoint must not be returned, got %q", ep.ID)
	}

	challenge := &identityBindCodeChallenge{
		Action: identityBindActionUser,
		New:    identityBindEndpoint{GroupID: bindTestNewGroupID, UserID: bindTestNewUserID},
		Old:    identityBindEndpoint{UserID: bindTestOldUserID},
	}
	before := env.recorder.groupCount()
	identityBindNotifyNewSide(env.d, challenge) // 老实现会在这里 panic
	if env.recorder.groupCount() != before {
		t.Fatal("no message should be sent through a disconnected endpoint")
	}
}

// TestIdentityBindPrivateChatBindNotifiesByPerson 私聊里发起绑定时，
// New.GroupID 是 "PG-..." 这种伪群号，通知必须走私聊，否则用户什么都收不到。
func TestIdentityBindPrivateChatBindNotifiesByPerson(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	env.ctx.EndPoint.State = StateConnected

	challenge := &identityBindCodeChallenge{
		Action: identityBindActionUser,
		New:    identityBindEndpoint{GroupID: "PG-" + bindTestNewUserID, UserID: bindTestNewUserID},
		Old:    identityBindEndpoint{UserID: bindTestOldUserID},
	}

	before := env.recorder.personCount()
	identityBindNotifyNewSide(env.d, challenge)
	got := env.recorder.waitPersonReply(t, before)
	if !strings.Contains(got, "绑定成功") {
		t.Fatalf("private-chat bind should be notified by DM, got %q", got)
	}
	if env.recorder.groupCount() != 0 {
		t.Fatal("must not try to send a group message to a pseudo group id")
	}
}

// TestIdentityBindGroupUnbindRequiresAdmin 普通成员不能解除群绑定。
func TestIdentityBindGroupUnbindRequiresAdmin(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()

	record := &identityBindRecord{
		Action: identityBindActionGroup,
		New:    identityBindEndpoint{GroupID: bindTestNewGroupID, UserID: bindTestNewUserID},
		Old:    identityBindEndpoint{GroupID: bindTestOldGroupID},
	}
	if err := identityBindStoreOf(env.d).put(env.d, record); err != nil {
		t.Fatalf("put group binding: %v", err)
	}

	// 普通成员（权限 0）
	orig := env.ctx.PrivilegeLevel
	env.ctx.PrivilegeLevel = 0
	before := env.recorder.groupCount()
	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"unbind"}})
	reply := env.recorder.waitGroupReply(t, before)
	env.ctx.PrivilegeLevel = orig

	if !strings.Contains(reply, "管理权限") {
		t.Fatalf("non-admin unbind should be refused, got %q", reply)
	}
	if _, ok := identityBindStoreOf(env.d).find(env.d, bindTestOldGroupID); !ok {
		t.Fatal("the group binding must survive a non-admin unbind attempt")
	}

	// 管理员（权限 50）可以
	env.ctx.PrivilegeLevel = 50
	before = env.recorder.groupCount()
	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"unbind"}})
	if got := env.recorder.waitGroupReply(t, before); !strings.Contains(got, "已解除绑定") {
		t.Fatalf("admin unbind should work, got %q", got)
	}
	if _, ok := identityBindStoreOf(env.d).find(env.d, bindTestOldGroupID); ok {
		t.Fatal("the group binding should be gone after an admin unbind")
	}
}

// TestIdentityBindGroupCancelRequiresAdmin 普通成员不能清掉群维度的验证码。
func TestIdentityBindGroupCancelRequiresAdmin(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()

	challenge := &identityBindCodeChallenge{
		Action:    identityBindActionGroup,
		New:       identityBindEndpoint{GroupID: bindTestNewGroupID, UserID: bindTestNewUserID},
		Old:       identityBindEndpoint{GroupID: bindTestOldGroupID},
		Code:      "246810",
		DeliverTo: bindTestOldUserID,
		ConfirmBy: bindTestOldUserID,
		Channel:   identityBindCodeChannelEmail,
		Status:    identityBindCodeDelivered,
		CreatedAt: 1,
		ExpiresAt: 1<<62 - 1,
	}
	identityBindPutCode(challenge)

	orig := env.ctx.PrivilegeLevel
	env.ctx.PrivilegeLevel = 0
	before := env.recorder.groupCount()
	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"cancel"}})
	env.recorder.waitGroupReply(t, before)
	env.ctx.PrivilegeLevel = orig

	if got, _ := identityBindLoadCode(identityBindActionGroup, bindTestNewGroupID); got == nil || got.Status != identityBindCodeDelivered {
		t.Fatalf("a non-admin must not cancel the group challenge, got %+v", got)
	}
}

// TestIdentityBindEmailWrongGuessesInvalidateChallenge 邮箱通道也要有失败上限。
func TestIdentityBindEmailWrongGuessesInvalidateChallenge(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	withMailCapture(t)

	challenge := &identityBindCodeChallenge{
		Action:    identityBindActionUser,
		New:       identityBindEndpoint{UserID: bindTestNewUserID},
		Old:       identityBindEndpoint{UserID: bindTestOldUserID},
		Code:      "999999",
		DeliverTo: bindTestOldUserID,
		ConfirmBy: bindTestOldUserID,
		Channel:   identityBindCodeChannelEmail,
		Status:    identityBindCodeDelivered,
		CreatedAt: 1,
		ExpiresAt: 1<<62 - 1,
	}
	identityBindPutCode(challenge)

	// 发起者自己猜错：必须计次
	for i := range identityBindCodeMaxAttempts {
		ctx, msg := newQuitCommandTestContext(t, env.d, env.ctx.EndPoint, bindTestNewUserID, bindTestNewGroupID, "新群")
		msg.MessageType = "group"
		if identityBindTryConsumeCode(ctx, msg, "00000"+string(rune('0'+i))) {
			t.Fatalf("a wrong code must not be consumed (attempt %d)", i+1)
		}
	}
	if challenge.Attempts != identityBindCodeMaxAttempts {
		t.Fatalf("attempts = %d, want %d", challenge.Attempts, identityBindCodeMaxAttempts)
	}
	if challenge.Status != identityBindCodeUsed {
		t.Fatalf("the challenge should be invalidated after %d wrong guesses, got %q",
			identityBindCodeMaxAttempts, challenge.Status)
	}
	if _, ok := identityBindStoreOf(env.d).find(env.d, bindTestOldUserID); ok {
		t.Fatal("wrong guesses must never create a binding")
	}
}

// TestIdentityBindEmailWrongGuessesByBystanderDoNotBurnChallenge
// 路人乱发数字不能把别人的验证码作废（否则是白送的骚扰手段）。
func TestIdentityBindEmailWrongGuessesByBystanderDoNotBurnChallenge(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	withMailCapture(t)

	challenge := &identityBindCodeChallenge{
		Action:    identityBindActionUser,
		New:       identityBindEndpoint{UserID: bindTestNewUserID},
		Old:       identityBindEndpoint{UserID: bindTestOldUserID},
		Code:      "999999",
		DeliverTo: bindTestOldUserID,
		ConfirmBy: bindTestOldUserID,
		Channel:   identityBindCodeChannelEmail,
		Status:    identityBindCodeDelivered,
		CreatedAt: 1,
		ExpiresAt: 1<<62 - 1,
	}
	identityBindPutCode(challenge)

	for i := range identityBindCodeMaxAttempts + 3 {
		ctx, msg := newQuitCommandTestContext(t, env.d, env.ctx.EndPoint, "OpenQQ:100-bystander", bindTestNewGroupID, "新群")
		msg.MessageType = "group"
		identityBindTryConsumeCode(ctx, msg, "11111"+string(rune('0'+i)))
	}
	if challenge.Attempts != 0 || challenge.Status != identityBindCodeDelivered {
		t.Fatalf("a bystander must not be able to burn someone else's challenge, got attempts=%d status=%q",
			challenge.Attempts, challenge.Status)
	}
}

// TestIdentityBindEmailCodeStillWorksWithinBudget 猜错几次之后，正确的码仍然能用。
func TestIdentityBindEmailCodeStillWorksWithinBudget(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	withMailCapture(t)

	challenge := &identityBindCodeChallenge{
		Action:    identityBindActionUser,
		New:       identityBindEndpoint{UserID: bindTestNewUserID},
		Old:       identityBindEndpoint{UserID: bindTestOldUserID},
		Code:      "999999",
		DeliverTo: bindTestOldUserID,
		ConfirmBy: bindTestOldUserID,
		Channel:   identityBindCodeChannelEmail,
		Status:    identityBindCodeDelivered,
		CreatedAt: 1,
		ExpiresAt: 1<<62 - 1,
	}
	identityBindPutCode(challenge)

	wrongCtx, wrongMsg := newQuitCommandTestContext(t, env.d, env.ctx.EndPoint, bindTestNewUserID, bindTestNewGroupID, "新群")
	wrongMsg.MessageType = "group"
	identityBindTryConsumeCode(wrongCtx, wrongMsg, "000000")

	okCtx, okMsg := newQuitCommandTestContext(t, env.d, env.ctx.EndPoint, bindTestNewUserID, bindTestNewGroupID, "新群")
	okMsg.MessageType = "group"
	if !identityBindTryConsumeCode(okCtx, okMsg, "999999") {
		t.Fatal("the correct code should still work after a few wrong guesses")
	}
	if _, ok := identityBindStoreOf(env.d).find(env.d, bindTestOldUserID); !ok {
		t.Fatal("the binding should be created")
	}
}

// ctxCapturingAdapter 记录发送时用的上下文，用来断言"是不是按被动回复发的"。
type ctxCapturingAdapter struct {
	mockPlatformAdapter
	mu      sync.Mutex
	lastCtx *MsgContext
	texts   []string
}

func (a *ctxCapturingAdapter) SendToGroup(ctx *MsgContext, _ string, text string, _ string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastCtx = ctx
	a.texts = append(a.texts, text)
}

func (a *ctxCapturingAdapter) ctx() *MsgContext {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastCtx
}

func (a *ctxCapturingAdapter) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.texts)
}

// TestIdentityBindSuccessNoticeUsesPassiveReply 刚发起就确认时，通知要走被动回复。
//
// 官方平台的主动群消息受"群内主动发言"权限与额度限制，很容易发不出去
// （用户实测：group bind 成功后那条成功通知发不出来）。
// 被动回复（引用刚收到的那条指令消息）没有这个限制。
func TestIdentityBindSuccessNoticeUsesPassiveReply(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	env.ctx.EndPoint.State = StateConnected

	capturer := &ctxCapturingAdapter{}
	env.ctx.EndPoint.Adapter = capturer

	group := &GroupInfo{
		Active:          true,
		GroupID:         bindTestNewGroupID,
		GroupName:       "官方群",
		DiceIDActiveMap: new(SyncMap[string, bool]),
		DiceIDExistsMap: new(SyncMap[string, bool]),
		BotList:         new(SyncMap[string, bool]),
		Players:         new(SyncMap[string, *GroupPlayerInfo]),
		PlayerGroups:    new(SyncMap[string, []string]),
	}
	env.d.ImSession.ServiceAtNew.Store(bindTestNewGroupID, group)

	challenge := &identityBindCodeChallenge{
		Action:   identityBindActionGroup,
		New:      identityBindEndpoint{GroupID: bindTestNewGroupID, UserID: bindTestNewUserID},
		Old:      identityBindEndpoint{GroupID: bindTestOldGroupID},
		NewMsgID: "raw-msg-1",
		NewMsgAt: time.Now().Unix(),
	}
	identityBindNotifyNewSide(env.d, challenge)

	if capturer.count() != 1 {
		t.Fatalf("expected exactly one group notice, got %d", capturer.count())
	}
	sendCtx := capturer.ctx()
	if sendCtx == nil {
		t.Fatal("send context should be captured")
	}
	if sendCtx.Player == nil {
		t.Fatal("send context must always carry a non-nil player")
	}
	msgID, ok := VarGetValueStr(sendCtx, "$tMsgID")
	if !ok || msgID != "raw-msg-1" {
		t.Fatalf("expected a passive reply carrying $tMsgID, got (%q, %v)", msgID, ok)
	}
}

// TestIdentityBindSuccessNoticeFallsBackToActiveMessage 确认太晚时退回主动消息。
func TestIdentityBindSuccessNoticeFallsBackToActiveMessage(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	env.ctx.EndPoint.State = StateConnected

	capturer := &ctxCapturingAdapter{}
	env.ctx.EndPoint.Adapter = capturer

	group := &GroupInfo{
		Active:          true,
		GroupID:         bindTestNewGroupID,
		GroupName:       "官方群",
		DiceIDActiveMap: new(SyncMap[string, bool]),
		DiceIDExistsMap: new(SyncMap[string, bool]),
		BotList:         new(SyncMap[string, bool]),
		Players:         new(SyncMap[string, *GroupPlayerInfo]),
		PlayerGroups:    new(SyncMap[string, []string]),
	}
	env.d.ImSession.ServiceAtNew.Store(bindTestNewGroupID, group)

	challenge := &identityBindCodeChallenge{
		Action:   identityBindActionGroup,
		New:      identityBindEndpoint{GroupID: bindTestNewGroupID, UserID: bindTestNewUserID},
		Old:      identityBindEndpoint{GroupID: bindTestOldGroupID},
		NewMsgID: "raw-msg-1",
		NewMsgAt: time.Now().Add(-30 * time.Minute).Unix(), // 早过了被动窗口
	}
	identityBindNotifyNewSide(env.d, challenge)

	if capturer.count() != 1 {
		t.Fatalf("expected exactly one group notice, got %d", capturer.count())
	}
	if msgID, ok := VarGetValueStr(capturer.ctx(), "$tMsgID"); ok && msgID != "" {
		t.Fatalf("an expired window must not be used as a passive reply, got %q", msgID)
	}
}

// TestIdentityBindEmailConfirmRepliesToConfirmer 邮箱通道确认后必须给确认人回执。
//
// 回归：旧实现只给私聊通道回执，邮箱通道完全依赖官方群里的**主动**通知；
// 那条通知发不出去时，用户就一点反馈都没有。
func TestIdentityBindEmailConfirmRepliesToConfirmer(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	withMailCapture(t)
	env.ctx.EndPoint.State = StateConnected

	challenge := &identityBindCodeChallenge{
		Action:    identityBindActionUser,
		New:       identityBindEndpoint{GroupID: bindTestNewGroupID, UserID: bindTestNewUserID},
		Old:       identityBindEndpoint{UserID: bindTestOldUserID},
		Code:      "135791",
		DeliverTo: bindTestOldUserID,
		ConfirmBy: bindTestOldUserID,
		Channel:   identityBindCodeChannelEmail,
		Status:    identityBindCodeDelivered,
		CreatedAt: 1,
		ExpiresAt: 1<<62 - 1,
	}
	identityBindPutCode(challenge)

	before := env.recorder.personCount()
	env.recorder.waitPersonReply(t, before) // 清掉可能存在的旧消息
	before = env.recorder.personCount()

	confirmCtx, confirmMsg := newQuitCommandTestContext(t, env.d, env.ctx.EndPoint, bindTestNewUserID, bindTestNewGroupID, "新群")
	confirmMsg.MessageType = "group"
	if !identityBindTryConsumeCode(confirmCtx, confirmMsg, "135791") {
		t.Fatal("the correct code should be consumed")
	}
	got := env.recorder.waitPersonReply(t, before)
	if !strings.Contains(got, "验证通过") {
		t.Fatalf("the confirmer should get a receipt, got %q", got)
	}
}

// TestOfficialQQStatusBarHiddenWhenMarkdownOff 关掉 markdown 时不能注入公式。
//
// 回归：默认配置就是 markdown 关闭，而适配器此时按纯文本发送，
// 注入的 `$\scriptsize\textcolor{...}$` 会原样显示在每条掷骰回复顶部。
func TestOfficialQQStatusBarHiddenWhenMarkdownOff(t *testing.T) {
	ctx, cleanup := newOfficialQQBarTestCtx(t, "coc7", map[string]*ds.VMValue{"血条": intVal(12)})
	defer cleanup()
	ctx.Player.AutoSetNameTemplate = "{$t玩家_RAW} HP{血条}"

	if bar := officialQQCharacterStatusBar(ctx); bar == "" {
		t.Fatal("setup: markdown on, expected a status bar")
	}

	ctx.Dice.Config.OfficialQQUseMarkdown = false
	if bar := officialQQCharacterStatusBar(ctx); bar != "" {
		t.Fatalf("markdown off must hide the status bar, got %q", bar)
	}
}
