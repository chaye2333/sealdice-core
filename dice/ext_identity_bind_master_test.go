//nolint:testpackage
package dice

import (
	"strings"
	"testing"
	"time"
)

// 这一组测试针对"通道不诚实"和"没有出口"两类问题：
//
//  1. 民间 bot 根本没连上，却回一句"已通过民间 bot 发送了私聊验证码"；
//  2. 邮箱 SMTP 配错了，却回一句"验证码已寄出"；
//  3. 两条通道都不通时，说好的"骰主人工确认"没有任何通知出口。
//
// 这些都是用户实际撞到的，所以必须有回归测试钉住。

// personCount / lastPersonText 用来看"私聊发出去了什么"（骰主通知走的就是私聊）。
func (a *recordingAdapter) personCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.personTexts)
}

func (a *recordingAdapter) lastPersonText() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.personTexts) == 0 {
		return ""
	}
	return a.personTexts[len(a.personTexts)-1]
}

// groupCount / lastGroupText / waitGroupReply 只看"给人看的群回复"。
//
// 为什么不能直接用 messageCount + last：骰主通知是**私聊**（personTexts），
// 而给用户的回复是群消息（groupTexts）。混在一起计数时私聊通知会先出现，
// 于是"等下一条回复"拿到的是通知，不是用户真正看到的那句。
func (a *recordingAdapter) groupCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.groupTexts)
}

func (a *recordingAdapter) lastGroupText() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.groupTexts) == 0 {
		return ""
	}
	return a.groupTexts[len(a.groupTexts)-1]
}

func (a *recordingAdapter) waitGroupReply(t *testing.T, before int) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a.groupCount() > before {
			return a.lastGroupText()
		}
		time.Sleep(10 * time.Millisecond)
	}
	return ""
}

// waitPersonReply 等一条**私聊**回复（私聊上下文里 ReplyToSender 走的是 SendToPerson）。
func (a *recordingAdapter) waitPersonReply(t *testing.T, before int) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a.personCount() > before {
			return a.lastPersonText()
		}
		time.Sleep(10 * time.Millisecond)
	}
	return ""
}

// disableOldBotEndpoints 把民间 bot 改成"配置里有、但没连上"。
func disableOldBotEndpoints(d *Dice) {
	for _, ep := range d.ImSession.EndPoints {
		if ep != nil && ep.ProtocolType == "onebot" {
			ep.State = StateDisconnected
		}
	}
}

// removeOldBotEndpoints 直接摘掉民间 bot，模拟"骰主根本没配 OneBot"。
func removeOldBotEndpoints(d *Dice) {
	var kept []*EndPointInfo
	for _, ep := range d.ImSession.EndPoints {
		if ep != nil && ep.ProtocolType == "onebot" {
			continue
		}
		kept = append(kept, ep)
	}
	d.ImSession.EndPoints = kept
}

// TestIdentityBindOfflineOldBotIsNotUsable 没连上的 OneBot 连接不算可用。
//
// 回归：老实现只看 Enable，于是挑战被标成"已私聊投递"，
// 用户按提示去私聊里等一条永远不会来的验证码。
func TestIdentityBindOfflineOldBotIsNotUsable(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	mailBox := withMailCapture(t)
	enableMailConfig(env.d)

	disableOldBotEndpoints(env.d)
	if ep := identityBindFindOldBotEndPoint(env.d.ImSession, nil); ep != nil {
		t.Fatalf("an endpoint that is not connected must not be usable, got %q", ep.ID)
	}

	before := env.recorder.groupCount()
	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001"}})
	reply := env.recorder.waitGroupReply(t, before)

	if got, _ := identityBindLoadCode(identityBindActionUser, bindTestNewUserID); got == nil ||
		got.Status != identityBindCodeDelivered || got.Channel != identityBindCodeChannelEmail {
		t.Fatalf("should fall back to email, got %+v", got)
	}
	if mailBox.count() == 0 {
		t.Fatal("the code should have been mailed instead of DM'ed")
	}
	if !strings.Contains(reply, "邮箱") {
		t.Fatalf("reply should point at the email channel, got %q", reply)
	}
	if strings.Contains(reply, "民间 bot 给") {
		t.Fatalf("reply must not claim a DM was sent when the bot is offline, got %q", reply)
	}
}

// TestIdentityBindMailFailureIsReportedNotHidden SMTP 失败必须说出来。
func TestIdentityBindMailFailureIsReportedNotHidden(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	enableMailConfig(env.d)
	env.ctx.EndPoint.State = StateConnected
	removeOldBotEndpoints(env.d)
	env.d.DiceMasters = nil // 本用例只看用户侧文案

	prev := identityBindMailSender
	identityBindMailSender = failSender()
	t.Cleanup(func() { identityBindMailSender = prev })

	before := env.recorder.groupCount()
	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001"}})
	reply := env.recorder.waitGroupReply(t, before)

	if strings.Contains(reply, "已寄到") {
		t.Fatalf("must not claim the mail was sent, got %q", reply)
	}
	if !strings.Contains(reply, "SMTP") {
		t.Fatalf("the failure reason should mention SMTP, got %q", reply)
	}
	if got, _ := identityBindLoadCode(identityBindActionUser, bindTestNewUserID); got == nil || got.Status == identityBindCodeDelivered {
		t.Fatalf("a failed SMTP send must not be marked as delivered, got %+v", got)
	}
}

// TestIdentityBindNoChannelNotifiesMasterAndIsIdempotent
// 两条通道都不通时：转人工 + 私聊通知骰主，而且只通知一次。
func TestIdentityBindNoChannelNotifiesMasterAndIsIdempotent(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	env.ctx.EndPoint.State = StateConnected
	removeOldBotEndpoints(env.d)

	const masterID = "OpenQQ:100-master"
	env.d.DiceMasters = []string{"UI:1001", masterID}

	before := env.recorder.groupCount()
	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001"}})
	reply := env.recorder.waitGroupReply(t, before)

	got, _ := identityBindLoadCode(identityBindActionUser, bindTestNewUserID)
	if got == nil || !got.NeedMaster {
		t.Fatalf("challenge should be escalated to the master, got %+v", got)
	}
	// UI:1001 不是聊天账号，只有官方侧的骰主能收到
	if len(got.MasterNotifiedTo) != 1 || got.MasterNotifiedTo[0] != masterID {
		t.Fatalf("expected only the chat-capable master to be notified, got %#v", got.MasterNotifiedTo)
	}
	notice := env.recorder.lastPersonText()
	if !strings.Contains(notice, "人工确认") || !strings.Contains(notice, ".bind approve 2001") {
		t.Fatalf("the master notice should explain what to do, got %q", notice)
	}
	if !strings.Contains(reply, "骰主") {
		t.Fatalf("the user should be told the master was notified, got %q", reply)
	}

	// 投递 worker 每 3 秒跑一次，不能把骰主私聊刷爆
	personBefore := env.recorder.personCount()
	identityBindDeliverPendingCodes(env.d)
	identityBindDeliverPendingCodes(env.d)
	if env.recorder.personCount() != personBefore {
		t.Fatal("the master must not be notified repeatedly by the delivery worker")
	}
}

// TestIdentityBindPendingAndMasterApprove .bind pending / .bind approve 走通。
func TestIdentityBindPendingAndMasterApprove(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	env.ctx.EndPoint.State = StateConnected
	removeOldBotEndpoints(env.d)
	env.d.DiceMasters = []string{"OpenQQ:100-master"}

	before := env.recorder.groupCount()
	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001"}})
	env.recorder.waitGroupReply(t, before)

	// 非骰主看不到待确认列表（就地改权限再改回来，避免复制带锁的 MsgContext）
	origPrivilege := env.ctx.PrivilegeLevel
	env.ctx.PrivilegeLevel = 50
	b := env.recorder.groupCount()
	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"pending"}})
	got := env.recorder.waitGroupReply(t, b)
	env.ctx.PrivilegeLevel = origPrivilege
	if !strings.Contains(got, "master") {
		t.Fatalf("pending list must be master-only, got %q", got)
	}

	// 骰主能看到列表，并且能看到卡在哪一步
	b = env.recorder.groupCount()
	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"pending"}})
	list := env.recorder.waitGroupReply(t, b)
	if !strings.Contains(list, "旧QQ 2001") || !strings.Contains(list, "等骰主确认") {
		t.Fatalf("pending list should describe the request, got %q", list)
	}

	// 骰主确认 → 绑定落库，且归属是**发起者**
	b = env.recorder.groupCount()
	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"approve", "2001"}})
	approveReply := env.recorder.waitGroupReply(t, b)
	if !strings.Contains(approveReply, "绑定成功") {
		t.Fatalf("approve should report success, got %q", approveReply)
	}
	record, ok := identityBindStoreOf(env.d).find(env.d, bindTestOldUserID)
	if !ok {
		t.Fatal("approve should create the binding record")
	}
	if record.Action != identityBindActionUser || record.New.UserID != bindTestNewUserID {
		t.Fatalf("binding should belong to the initiator, got %+v", record)
	}
	// 确认过的申请不再是"待确认"
	if got, _ := identityBindLoadCode(identityBindActionUser, bindTestNewUserID); got != nil && got.NeedMaster {
		t.Fatalf("the challenge should be finished after approve, got %+v", got)
	}
}

// TestIdentityBindGroupApproveWithoutGroupContext 骰主可以在私聊里处理群绑定申请。
//
// 这就是 ctx.Group == nil 的场景：以前 .group 一进来就 return，
// 骰主在私聊里根本没法确认，只能跑到那个群里去敲指令。
func TestIdentityBindGroupApproveWithoutGroupContext(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	env.ctx.EndPoint.State = StateConnected
	removeOldBotEndpoints(env.d)

	challenge := &identityBindCodeChallenge{
		Action: identityBindActionGroup,
		New: identityBindEndpoint{
			Platform: "QQ", Protocol: "official",
			GroupID: bindTestNewGroupID, UserID: bindTestNewUserID,
		},
		Old: identityBindEndpoint{
			Platform: "QQ", Protocol: "onebot",
			GroupID: bindTestOldGroupID, GroupName: "旧群",
		},
		Code:       "135790",
		DeliverTo:  bindTestOldUserID,
		ConfirmBy:  bindTestOldUserID,
		Status:     identityBindCodePending,
		NeedMaster: true,
		CreatedAt:  1,
		ExpiresAt:  1<<62 - 1,
	}
	identityBindPutCode(challenge)

	// 私聊上下文：Group 为 nil（不复制 MsgContext，它带锁）
	privateCtx := &MsgContext{
		MessageType:    "private",
		IsPrivate:      true,
		Player:         env.ctx.Player,
		EndPoint:       env.ctx.EndPoint,
		Session:        env.ctx.Session,
		Dice:           env.d,
		PrivilegeLevel: 100,
	}
	privateMsg := &Message{
		MessageType: "private",
		Sender:      env.msg.Sender,
	}

	before := env.recorder.personCount()
	runIdentityBindGroupCommand(privateCtx, privateMsg, &CmdArgs{Args: []string{"approve", "1001"}})
	reply := env.recorder.waitPersonReply(t, before)

	if !strings.Contains(reply, "绑定成功") {
		t.Fatalf("approve from private chat should work, got %q", reply)
	}
	record, ok := identityBindStoreOf(env.d).find(env.d, bindTestOldGroupID)
	if !ok || record.Action != identityBindActionGroup || record.New.GroupID != bindTestNewGroupID {
		t.Fatalf("group binding should be created from the challenge, got %+v", record)
	}
}

// TestIdentityBindNotifyNewSideSurvivesMissingGroupContext
// 绑定成功通知不能把官方 bot 的连接打崩（用户实测的 panic）。
func TestIdentityBindNotifyNewSideSurvivesMissingGroupContext(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()

	const ghostGroup = "OpenQQ-Group:100-ghost"
	challenge := &identityBindCodeChallenge{
		Action: identityBindActionUser,
		New:    identityBindEndpoint{GroupID: ghostGroup, UserID: bindTestNewUserID},
		Old:    identityBindEndpoint{UserID: bindTestOldUserID},
	}

	// 群不在内存里（ServiceAtNew 没有它）→ 也必须能安全发出通知
	before := env.recorder.groupCount()
	identityBindNotifyNewSide(env.d, challenge)
	if got := env.recorder.waitNextReply(t, before); !strings.Contains(got, "绑定成功") {
		t.Fatalf("notification should still be sent, got %q", got)
	}
}

// TestVarGetValueToleratesNilPlayer 直接钉住 panic 的现场。
//
// 官方适配器发群消息前会取 `$tMsgID`（见 SendToGroup），
// 而后台通知拼出来的上下文里没有玩家；老实现直接读 ctx.Player.ValueMapTemp，
// nil 解引用 panic，并被官方 SDK 的 PanicHandler 放大成"整条连接断掉"。
func TestVarGetValueToleratesNilPlayer(t *testing.T) {
	d, _, _, cleanup := newExecuteNewTestDice(t)
	defer cleanup()

	// 生产现场：有 Dice，但没有玩家（后台通知自己拼出来的上下文）
	ctx := &MsgContext{Dice: d}
	if _, ok := VarGetValue(ctx, "$tMsgID"); ok {
		t.Fatal("a context without a player must simply have no temp vars")
	}
	// 更极端的情况也不能 panic
	if _, ok := VarGetValue(&MsgContext{}, "$tMsgID"); ok {
		t.Fatal("a context without a dice must simply have no temp vars")
	}
	if _, ok := VarGetValue(nil, "$tMsgID"); ok {
		t.Fatal("a nil context must simply have no temp vars")
	}
}
