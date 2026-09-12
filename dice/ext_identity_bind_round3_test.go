//nolint:testpackage
package dice

import (
	"strings"
	"sync"
	"testing"
	"time"

	"sealdice-core/dice/service"
)

// 这一组针对第三轮修复：
//
//	1. 私聊伪群号 "PG-<用户ID>" 没归一 —— 官方私聊与旧号私聊各存一份属性/默认卡；
//	2. 挑战对象并发无锁 —— worker 与消息分发会同时改同一条挑战；
//	3. 暗骰（私聊回复）内容因为 CommandHideFlag 没归一而不进日志；
//	4. 对外发信没有按目标号限频 —— 可以拿骰子刷任意 QQ 邮箱/私聊；
//	5. .sn off 关不掉状态栏（本侧模板被清空后，又去沿用绑定另一侧的模板）；
//	6. 群绑定回执把旧群邀请人的 QQ 号/邮箱暴露给全群。

// TestIdentityBindPrivatePseudoGroupIsNormalized 私聊属性/默认卡必须两边共通。
//
// Sealdice 把私聊伪装成 "PG-<用户ID>"，这个伪群号是拿**真实**用户 ID 拼的，
// 直接查群绑定永远查不到，于是官方私聊和旧号私聊各存一份属性。
func TestIdentityBindPrivatePseudoGroupIsNormalized(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()

	record := &identityBindRecord{
		Action: identityBindActionUser,
		New:    identityBindEndpoint{GroupID: bindTestNewGroupID, UserID: bindTestNewUserID},
		Old:    identityBindEndpoint{GroupID: bindTestOldGroupID, UserID: bindTestOldUserID},
	}
	if err := identityBindStoreOf(env.d).put(env.d, record); err != nil {
		t.Fatalf("put binding: %v", err)
	}

	got := identityCanonicalGroupID(env.d, identityBindPrivateGroupPrefix+bindTestNewUserID)
	want := identityBindPrivateGroupPrefix + bindTestOldUserID
	if got != want {
		t.Fatalf("official private pseudo group = %q, want %q", got, want)
	}
	// 旧号那一侧本来就是规范侧，保持不变
	if got := identityCanonicalGroupID(env.d, identityBindPrivateGroupPrefix+bindTestOldUserID); got != want {
		t.Fatalf("old-side private pseudo group should stay %q, got %q", want, got)
	}
	// 没绑定的人不受影响
	if got := identityCanonicalGroupID(env.d, identityBindPrivateGroupPrefix+"QQ:9999"); got != "PG-QQ:9999" {
		t.Fatalf("unbound private pseudo group should be unchanged, got %q", got)
	}
}

// TestIdentityBindDarkRollIsLoggedUnderCanonicalGroup 暗骰回复要写进归一后的日志。
//
// CommandHideFlag 里存的是真实群号；绑群之后状态与日志行都在旧群名下，
// 用真实群取状态会得到"未开启"，暗骰内容静默丢失。
func TestIdentityBindDarkRollIsLoggedUnderCanonicalGroup(t *testing.T) {
	env, _, oldCtx := logShareEnv(t)
	defer env.cleanup()

	// 旧群上开启记录（归一后的状态就在旧群对象上）
	runLog(t, env, oldCtx, "new", "暗骰测试")

	logExt := env.d.ExtFind("log", false)
	if logExt == nil {
		t.Fatal("log extension is not registered")
	}
	if logExt.OnMessageSend == nil {
		t.Fatal("log extension has no OnMessageSend")
	}

	// 模拟"暗骰"：指令在官方群发起，公开回执走群、结果走私聊，
	// 因此 CommandHideFlag 记的是**真实群号**。
	hideCtx, _ := newQuitCommandTestContext(t, env.d, env.ctx.EndPoint, bindTestNewUserID, bindTestNewGroupID, "新群")
	hideCtx.CommandHideFlag = bindTestNewGroupID
	hideCtx.CommandID = 424242
	hideCtx.CommandInfo = map[string]any{}

	// 上游就是靠"群里的那条回执"登记 CommandID 的，这里先走一遍
	logExt.OnMessageSend(hideCtx, &Message{
		MessageType: "group", GroupID: bindTestNewGroupID,
		Message: "（公开回执）", Sender: SenderBase{UserID: env.ctx.EndPoint.UserID},
	}, "")

	// 然后再走私聊回复 —— 这一条必须落进归一后的日志
	hideCtx.IsPrivate = true
	logExt.OnMessageSend(hideCtx, &Message{
		MessageType: "private", GroupID: bindTestNewGroupID,
		Message: "暗骰结果", Sender: SenderBase{UserID: env.ctx.EndPoint.UserID},
	}, "")

	state := getGroupLogState(groupForTest(t, env, bindTestOldGroupID))
	if !state.On || state.Name != "暗骰测试" {
		t.Fatalf("setup: the old group should be recording, got %+v", state)
	}
	lines, err := service.LogGetAllLines(env.d.DBOperator, bindTestOldGroupID, state.Name)
	if err != nil {
		t.Fatalf("read log lines: %v", err)
	}
	for _, line := range lines {
		if strings.Contains(line.Message, "暗骰结果") {
			return
		}
	}
	t.Fatalf("dark roll should be recorded under the bound (old) group, got %d lines", len(lines))
}

// TestIdentityBindTargetThrottleBlocksRepeatedSends 同一个目标号不能反复触发发信。
func TestIdentityBindTargetThrottleBlocksRepeatedSends(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	withMailCapture(t)
	enableMailConfig(env.d)
	env.ctx.EndPoint.State = StateConnected
	removeOldBotEndpoints(env.d)
	env.d.DiceMasters = nil

	// 第一次可以发起
	before := env.recorder.groupCount()
	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001"}})
	if got := env.recorder.waitGroupReply(t, before); strings.Contains(got, "刚刚发起过") {
		t.Fatalf("the first attempt should be allowed, got %q", got)
	}

	// 第二次（同目标）必须被限频拦下，而不是又寄一封邮件
	before = env.recorder.groupCount()
	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001"}})
	got := env.recorder.waitGroupReply(t, before)
	if !strings.Contains(got, "刚刚发起过") {
		t.Fatalf("a repeated attempt on the same target must be throttled, got %q", got)
	}

	// 换一个目标仍可发起（限频是按目标号，不是把功能关掉）
	before = env.recorder.groupCount()
	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"3003"}})
	if got := env.recorder.waitGroupReply(t, before); strings.Contains(got, "刚刚发起过") {
		t.Fatalf("a different target should still be allowed, got %q", got)
	}
}

// TestIdentityBindSnOffDisablesStatusBar 明确关掉名片的用户不该再看到状态栏。
//
// 回归：本侧模板被 .sn off 清空后，identityBindPlayerNameTemplate 会去沿用
// 绑定另一侧的模板，于是状态栏又冒出来，看起来像"关不掉"。
func TestIdentityBindSnOffDisablesStatusBar(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()

	record := &identityBindRecord{
		Action: identityBindActionUser,
		New:    identityBindEndpoint{GroupID: bindTestNewGroupID, UserID: bindTestNewUserID},
		Old:    identityBindEndpoint{GroupID: bindTestOldGroupID, UserID: bindTestOldUserID},
	}
	if err := identityBindStoreOf(env.d).put(env.d, record); err != nil {
		t.Fatalf("put binding: %v", err)
	}

	// 旧号那一侧设过 .sn（迁移前留下的名片格式）
	oldGroup, _ := env.d.ImSession.ServiceAtNew.Load(bindTestOldGroupID)
	oldPlayer := oldGroup.PlayerGet(env.d.DBOperator, bindTestOldUserID)
	if oldPlayer == nil {
		t.Fatal("setup: old player should exist")
	}
	oldPlayer.AutoSetNameTemplate = "{$t玩家_RAW} HP{生命值}"

	// 官方侧没设过 → 沿用旧侧（这是想要的行为）
	if got := identityBindPlayerNameTemplate(env.ctx); got == "" {
		t.Fatal("before .sn off, the template should be inherited from the bound side")
	}

	// 官方侧执行 .sn off
	identityBindNoteNameTemplate(env.ctx, true)
	if got := identityBindPlayerNameTemplate(env.ctx); got != "" {
		t.Fatalf("after .sn off the template must stay empty, got %q", got)
	}

	// 再 .sn coc 就该恢复
	identityBindNoteNameTemplate(env.ctx, false)
	env.ctx.Player.AutoSetNameTemplate = "{$t玩家_RAW} HP{生命值}"
	if got := identityBindPlayerNameTemplate(env.ctx); got == "" {
		t.Fatal("setting a template again should re-enable it")
	}
}

// TestIdentityBindInviterInfoIsMaskedInGroupReply 群回执不能把邀请人的 QQ/邮箱公开。
func TestIdentityBindInviterInfoIsMaskedInGroupReply(t *testing.T) {
	const qq = "2431692084"
	masked := identityBindMaskNumber(qq)
	if masked == qq || strings.Contains(masked, qq) {
		t.Fatalf("QQ number should be masked, got %q", masked)
	}
	if !strings.HasPrefix(masked, "243") || !strings.HasSuffix(masked, "084") {
		t.Fatalf("mask should keep head and tail for recognition, got %q", masked)
	}
	maskedMail := identityBindMaskNumber(qq + "@qq.com")
	if strings.Contains(maskedMail, qq) || !strings.HasSuffix(maskedMail, "@qq.com") {
		t.Fatalf("mail address should be masked but keep the domain, got %q", maskedMail)
	}

	// 群绑定的回执文本里不能出现完整号码
	text := identityBindDeliverToText(identityBindActionGroup, "QQ:"+qq)
	if strings.Contains(text, qq) {
		t.Fatalf("group receipt leaks the inviter QQ: %q", text)
	}
	// 个人绑定显示的是用户自己填的号，保持原样
	if got := identityBindDeliverToText(identityBindActionUser, "QQ:"+qq); got != "QQ:"+qq {
		t.Fatalf("personal binding should keep the number, got %q", got)
	}
}

// TestIdentityBindConcurrentDeliverySendsOnce 并发投递同一条挑战只能发一次。
//
// 指令路径会同步投递一次，worker 每 3 秒也会扫一遍；没有抢占的话两边都会发，
// 而且 Channel 会被后写覆盖。这个用例同时给 -race 提供并发覆盖面。
func TestIdentityBindConcurrentDeliverySendsOnce(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	mailBox := withMailCapture(t)
	enableMailConfig(env.d)
	removeOldBotEndpoints(env.d) // 只剩邮箱通道，便于计数

	challenge := &identityBindCodeChallenge{
		Action:    identityBindActionUser,
		New:       identityBindEndpoint{GroupID: bindTestNewGroupID, UserID: bindTestNewUserID},
		Old:       identityBindEndpoint{UserID: bindTestOldUserID},
		Code:      "864209",
		DeliverTo: bindTestOldUserID,
		ConfirmBy: bindTestOldUserID,
		Status:    identityBindCodePending,
		CreatedAt: time.Now().Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}
	identityBindPutCode(challenge)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			identityBindDeliverPendingCodes(env.d)
		}()
	}
	wg.Wait()

	if mailBox.count() != 1 {
		t.Fatalf("the code should be delivered exactly once, got %d mails", mailBox.count())
	}
	got, _ := identityBindLoadCode(identityBindActionUser, bindTestNewUserID)
	if got == nil || got.Status != identityBindCodeDelivered || got.Channel != identityBindCodeChannelEmail {
		t.Fatalf("challenge should be delivered via email, got %+v", got)
	}
	if got.Delivering {
		t.Fatal("the delivering flag must be cleared after delivery")
	}
}
