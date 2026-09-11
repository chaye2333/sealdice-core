//nolint:testpackage
package dice

import (
	"strings"
	"testing"
	"time"
)

// ---------- 验证码：纯函数 ----------

func TestIdentityBindGenerateCodeShape(t *testing.T) {
	for _, n := range []int{4, 6, 8} {
		code, err := identityBindGenerateCode(n)
		if err != nil {
			t.Fatalf("generate(%d): %v", n, err)
		}
		if len(code) != n {
			t.Fatalf("generate(%d) = %q, len %d", n, code, len(code))
		}
		for _, r := range code {
			if r < '0' || r > '9' {
				t.Fatalf("generate(%d) = %q contains a non-digit", n, code)
			}
		}
	}
	// 越界会被收敛，而不是报错
	short, err := identityBindGenerateCode(1)
	if err != nil || len(short) != identityBindCodeMinLen {
		t.Fatalf("generate(1) = (%q, %v), want len %d", short, err, identityBindCodeMinLen)
	}
	long, err := identityBindGenerateCode(99)
	if err != nil || len(long) != identityBindCodeMaxLen {
		t.Fatalf("generate(99) = (%q, %v), want len %d", long, err, identityBindCodeMaxLen)
	}
}

// TestIdentityBindCodeCompareIsConstantTimeShape 验证码比较必须按位比较、长度不符即失败。
func TestIdentityBindCodeCompareIsConstantTimeShape(t *testing.T) {
	if !subtleCompareCode("123456", "123456") {
		t.Fatal("identical codes should match")
	}
	if subtleCompareCode("123456", "123457") {
		t.Fatal("different codes must not match")
	}
	if subtleCompareCode("12345", "123456") {
		t.Fatal("different lengths must not match")
	}
	if subtleCompareCode("", "") {
		// 空码相等在数学上成立，但调用方永远不该把空码放进来；
		// 这里只是把当前语义固定住，避免以后被误用。
		t.Log("note: empty codes compare equal by definition")
	}
}

func TestIdentityBindExtractQQNumber(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"QQ:12345", "12345"},
		{"12345", "12345"},
		{" QQ:12345 ", "12345"},
		{"OpenQQ:100-abc", ""},  // 非纯数字
		{"QQ-Group:123", "123"}, // 取最后一段，调用方负责只在用户维度用
		{"", ""},
		{"abc", ""},
	}
	for _, c := range cases {
		if got := identityBindExtractQQNumber(c.in); got != c.want {
			t.Fatalf("extractQQNumber(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if !identityBindSameQQUser("QQ:201234", "201234") {
		t.Fatal("QQ:201234 and bare 201234 must be the same user")
	}
	if identityBindSameQQUser("QQ:201234", "QQ:201235") {
		t.Fatal("different numbers must not be considered the same user")
	}
}

// TestIdentityBindCodeConfigClamps 验证码相关配置要被收敛到合法范围。
func TestIdentityBindCodeConfigClamps(t *testing.T) {
	d := &Dice{}
	d.Config.IdentityBindCodeLength = 0
	d.Config.IdentityBindCodeExpireSec = 0
	d.Config.FixIdentityBindConfig()
	if d.Config.IdentityBindCodeLength != identityBindCodeMinLen && d.Config.IdentityBindCodeLength != DefaultConfig.IdentityBindCodeLength {
		t.Fatalf("code length = %d, want a sane default", d.Config.IdentityBindCodeLength)
	}
	if d.Config.IdentityBindCodeExpireSec != DefaultConfig.IdentityBindCodeExpireSec {
		t.Fatalf("expire = %d, want default %d", d.Config.IdentityBindCodeExpireSec, DefaultConfig.IdentityBindCodeExpireSec)
	}

	d.Config.IdentityBindCodeLength = 999
	d.Config.IdentityBindCodeExpireSec = 999999
	d.Config.FixIdentityBindConfig()
	if d.Config.IdentityBindCodeLength != identityBindCodeMaxLen {
		t.Fatalf("code length = %d, want clamp to %d", d.Config.IdentityBindCodeLength, identityBindCodeMaxLen)
	}
	if d.Config.IdentityBindCodeExpireSec != 3600 {
		t.Fatalf("expire = %d, want clamp to 3600", d.Config.IdentityBindCodeExpireSec)
	}

	// 默认必须是"验证码开启"：防抢号优先。
	// 但它依赖民间 bot 在线，所以要保证关掉之后能完全回到答题流程（下面有专门用例）。
	if !DefaultConfig.IdentityBindUseVerificationCode {
		t.Fatal("verification code should default to ON")
	}
	if DefaultConfig.IdentityBindPreferEmailCode {
		t.Fatal("email should not be preferred by default (DM first)")
	}
}

// ---------- 验证码：端到端 ----------

// newCodeTestEnv 建一个既有官方 bot、又有民间 bot（OneBot）的环境。
func newCodeTestEnv(t *testing.T) *bindTestEnv {
	t.Helper()
	env := newBindTestEnv(t)
	env.d.Config.IdentityBindUseVerificationCode = true

	// 民间 bot 端点：OneBot，能处理 QQ:<号>
	oldRecorder := &recordingAdapter{}
	oldEP := &EndPointInfo{
		EndPointInfoBase: EndPointInfoBase{
			ID:           "test-ep-onebot",
			Platform:     "QQ",
			ProtocolType: "onebot",
			UserID:       "QQ:900000",
			Nickname:     "OldBot",
			Enable:       true,
		},
		Adapter: oldRecorder,
	}
	oldEP.Session = env.d.ImSession
	env.d.ImSession.EndPoints = append(env.d.ImSession.EndPoints, oldEP)
	return env
}

// codeOldEndPoint 取环境里的民间 bot 端点。
func codeOldEndPoint(t *testing.T, env *bindTestEnv) *EndPointInfo {
	t.Helper()
	ep := identityBindFindOldBotEndPoint(env.d.ImSession, nil)
	if ep == nil {
		t.Fatal("expected an OneBot endpoint to be found")
	}
	return ep
}

// codeRecordFor 直接往存储里塞一条个人绑定记录（绕过流程），用于准备"已被占用"等前置状态。
func codeRecordFor(t *testing.T, env *bindTestEnv, newUserID, oldUserID string) {
	t.Helper()
	if err := identityBindStoreOf(env.d).put(env.d, &identityBindRecord{
		Action: identityBindActionUser,
		New:    identityBindEndpoint{UserID: newUserID},
		Old:    identityBindEndpoint{UserID: oldUserID},
	}); err != nil {
		t.Fatalf("put record: %v", err)
	}
}

// TestIdentityBindCodeFlowEndToEnd 这是防抢号的核心验收：
// 官方 bot 发起 → 民间 bot 私聊发码 → 旧号本人回复 → 绑定成立。
func TestIdentityBindCodeFlowEndToEnd(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	env.addOldCard(t, "调查员甲")

	// 1) 官方 bot 侧发起绑定
	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001", "1001"}})
	reply := waitGroupMessage(t, env)
	if !strings.Contains(reply, "验证码") {
		t.Fatalf("expected a code-flow prompt, got %q", reply)
	}
	if !strings.Contains(reply, "私聊") {
		t.Fatalf("prompt should mention the private message, got %q", reply)
	}
	// 关键：不应该再出题了（默认 keepQuiz=false）
	if strings.Contains(reply, "共 1 题") {
		t.Fatalf("with the code flow the quiz should be skipped, got %q", reply)
	}

	// 2) 挑战必须已经登记并被投递
	challenge, ok := identityBindLoadCode(identityBindActionUser, bindTestNewUserID)
	if !ok {
		t.Fatal("expected a registered code challenge")
	}
	if challenge.Code == "" || len(challenge.Code) != identityBindCodeLength(env.d) {
		t.Fatalf("unexpected code %q", challenge.Code)
	}
	if challenge.DeliverTo != bindTestOldUserID {
		t.Fatalf("deliverTo = %q, want the old QQ id %q", challenge.DeliverTo, bindTestOldUserID)
	}
	if challenge.ConfirmBy != bindTestOldUserID {
		t.Fatalf("confirmBy = %q, want %q", challenge.ConfirmBy, bindTestOldUserID)
	}
	// 发起前后同步投递了一次，所以这时应该已经是 delivered
	if challenge.Status != identityBindCodeDelivered {
		t.Fatalf("status = %q, want delivered (reason=%q)", challenge.Status, challenge.Reason)
	}

	// 3) 民间 bot 侧：旧号本人回复验证码
	oldEP := codeOldEndPoint(t, env)
	oldCtx, oldMsg := newQuitCommandTestContext(t, env.d, oldEP, bindTestOldUserID, bindTestOldGroupID, "旧群")
	oldCtx.IsPrivate = true
	oldMsg.MessageType = "private"

	if !identityBindTryConsumeCode(oldCtx, oldMsg, challenge.Code) {
		t.Fatal("the code reply should be consumed")
	}

	// 4) 绑定必须成立，且方向正确（新=官方侧发起者，旧=被声明的旧号）
	record, ok := identityBindStoreOf(env.d).find(env.d, bindTestOldUserID)
	if !ok {
		t.Fatal("expected the binding to be created")
	}
	if record.New.UserID != bindTestNewUserID {
		t.Fatalf("binding New.UserID = %q, want the official initiator %q", record.New.UserID, bindTestNewUserID)
	}
	if record.Old.UserID != bindTestOldUserID {
		t.Fatalf("binding Old.UserID = %q, want %q", record.Old.UserID, bindTestOldUserID)
	}
	// 挑战应被标记为已完成
	if c, _ := identityBindLoadCode(identityBindActionUser, bindTestNewUserID); c != nil && c.Status != identityBindCodeUsed {
		t.Fatalf("challenge status = %q, want used", c.Status)
	}
}

// TestIdentityBindCodeCannotBeUsedByWrongPerson 抢号场景：
// 攻击者知道验证码也没用，因为只有被声明的那个号回复才被接受。
func TestIdentityBindCodeCannotBeUsedByWrongPerson(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	env.addOldCard(t, "调查员甲")

	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001", "1001"}})
	waitGroupMessage(t, env)

	challenge, ok := identityBindLoadCode(identityBindActionUser, bindTestNewUserID)
	if !ok || challenge.Status != identityBindCodeDelivered {
		t.Fatalf("expected a delivered challenge, got %+v / %v", challenge, ok)
	}

	// 另一个人（不同的旧 QQ 号）拿着正确验证码来试
	oldEP := codeOldEndPoint(t, env)
	attackerCtx, attackerMsg := newQuitCommandTestContext(t, env.d, oldEP, "QQ:999999", bindTestOldGroupID, "旧群")
	attackerCtx.IsPrivate = true
	attackerMsg.MessageType = "private"

	if !identityBindTryConsumeCode(attackerCtx, attackerMsg, challenge.Code) {
		t.Fatal("the attempt should still be consumed (it is a code-shaped DM)")
	}

	// 绑定绝不能成立
	if _, ok := identityBindStoreOf(env.d).find(env.d, bindTestOldUserID); ok {
		t.Fatal("a third party must not be able to complete the binding with the right code")
	}
	// 失败次数要记上
	c, _ := identityBindLoadCode(identityBindActionUser, bindTestNewUserID)
	if c == nil || c.Attempts == 0 {
		t.Fatalf("expected the attempt to be recorded, got %+v", c)
	}
}

// TestIdentityBindCodeWrongCodeIsIgnored 不是验证码的私聊不该被消费，
// 否则用户在私聊里随手发个数字就会被骰子"吃掉"。
func TestIdentityBindCodeWrongCodeIsIgnored(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	env.addOldCard(t, "调查员甲")

	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001", "1001"}})
	waitGroupMessage(t, env)

	oldEP := codeOldEndPoint(t, env)
	oldCtx, oldMsg := newQuitCommandTestContext(t, env.d, oldEP, bindTestOldUserID, bindTestOldGroupID, "旧群")
	oldCtx.IsPrivate = true
	oldMsg.MessageType = "private"

	// 没登记过验证码之前的随机数字：不消费
	if identityBindTryConsumeCode(oldCtx, oldMsg, "000000") {
		t.Fatal("an unrelated digit string must not be consumed")
	}
	// 非数字也不消费
	if identityBindTryConsumeCode(oldCtx, oldMsg, "你好") {
		t.Fatal("non-digit text must not be consumed")
	}
	// 太长的数字不消费
	if identityBindTryConsumeCode(oldCtx, oldMsg, "1234567890123") {
		t.Fatal("an over-long digit string must not be consumed")
	}
	// 群聊里的数字更不消费
	groupCtx, groupMsg := newQuitCommandTestContext(t, env.d, oldEP, bindTestOldUserID, bindTestOldGroupID, "旧群")
	groupCtx.IsPrivate = false
	groupMsg.MessageType = "group"
	if identityBindTryConsumeCode(groupCtx, groupMsg, "123456") {
		t.Fatal("group messages must never be consumed as codes")
	}
}

// TestIdentityBindCodeExpiry 过期验证码不再可用。
func TestIdentityBindCodeExpiry(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	env.addOldCard(t, "调查员甲")

	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001", "1001"}})
	waitGroupMessage(t, env)

	challenge, ok := identityBindLoadCode(identityBindActionUser, bindTestNewUserID)
	if !ok {
		t.Fatal("expected a challenge")
	}
	challenge.ExpiresAt = time.Now().Add(-time.Minute).Unix()

	oldEP := codeOldEndPoint(t, env)
	oldCtx, oldMsg := newQuitCommandTestContext(t, env.d, oldEP, bindTestOldUserID, bindTestOldGroupID, "旧群")
	oldCtx.IsPrivate = true
	oldMsg.MessageType = "private"

	if identityBindTryConsumeCode(oldCtx, oldMsg, challenge.Code) {
		t.Fatal("an expired code must not be consumable")
	}
	if _, ok := identityBindStoreOf(env.d).find(env.d, bindTestOldUserID); ok {
		t.Fatal("an expired code must not create a binding")
	}
}

// TestIdentityBindCodeDeliveryFailsWithoutOldBot 没有民间 bot 连接时，
// 必须明确告诉用户原因，而不是静默失败。
func TestIdentityBindCodeDeliveryFailsWithoutOldBot(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()
	env.d.Config.IdentityBindUseVerificationCode = true
	env.addOldCard(t, "调查员甲")

	// 环境里只有官方端点，没有 OneBot
	if ep := identityBindFindOldBotEndPoint(env.d.ImSession, nil); ep != nil {
		t.Fatalf("expected no OneBot endpoint, got %q", ep.ID)
	}

	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001", "1001"}})
	reply := waitGroupMessage(t, env)
	if !strings.Contains(reply, "没有可用的民间 bot") {
		t.Fatalf("should explain that no OneBot connection is available, got %q", reply)
	}
	// 挑战仍然登记着，等民间 bot 上线后会自动重试
	if c, ok := identityBindLoadCode(identityBindActionUser, bindTestNewUserID); !ok || c.Status != identityBindCodePending {
		t.Fatalf("challenge should stay pending, got %+v / %v", c, ok)
	}
}

// TestIdentityBindCodeOccupiedOldAccountRejected 已被绑定的旧号不能被重新绑定。
func TestIdentityBindCodeOccupiedOldAccountRejected(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	env.addOldCard(t, "调查员甲")
	// 先占用这个旧号
	codeRecordFor(t, env, "OpenQQ:100-someone-else", bindTestOldUserID)

	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001", "1001"}})
	reply := waitGroupMessage(t, env)
	if !strings.Contains(reply, "已经被绑定") {
		t.Fatalf("expected the takeover to be rejected, got %q", reply)
	}
	if _, ok := identityBindLoadCode(identityBindActionUser, bindTestNewUserID); ok {
		t.Fatal("no challenge should be registered for an already-claimed account")
	}
}

// TestIdentityBindCodeGroupFlowUsesInviter 群绑定的确认人是旧群邀请人。
func TestIdentityBindCodeGroupFlowUsesInviter(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	env.addOldLog(t, "第一话")

	// 旧群要有邀请人才能确定确认对象
	oldGroup := groupForTest(t, env, bindTestOldGroupID)
	oldGroup.InviteUserID = bindTestOldUserID

	// 官方群侧需要有管理权限
	env.ctx.PrivilegeLevel = 100

	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"bind", "1001"}})
	reply := waitGroupMessage(t, env)
	if !strings.Contains(reply, "验证码") {
		t.Fatalf("expected the group code flow, got %q", reply)
	}
	if !strings.Contains(reply, bindTestOldUserID) {
		t.Fatalf("the prompt should name the confirmer %q, got %q", bindTestOldUserID, reply)
	}

	challenge, ok := identityBindLoadCode(identityBindActionGroup, bindTestNewGroupID)
	if !ok {
		t.Fatal("expected a registered group challenge")
	}
	if challenge.DeliverTo != bindTestOldUserID {
		t.Fatalf("group challenge deliverTo = %q, want the inviter %q", challenge.DeliverTo, bindTestOldUserID)
	}

	// 邀请人回复验证码 → 群绑定成立
	oldEP := codeOldEndPoint(t, env)
	oldCtx, oldMsg := newQuitCommandTestContext(t, env.d, oldEP, bindTestOldUserID, bindTestOldGroupID, "旧群")
	oldCtx.IsPrivate = true
	oldMsg.MessageType = "private"
	if !identityBindTryConsumeCode(oldCtx, oldMsg, challenge.Code) {
		t.Fatal("the inviter's reply should be consumed")
	}

	record, ok := identityBindStoreOf(env.d).find(env.d, bindTestOldGroupID)
	if !ok {
		t.Fatal("expected the group binding to be created")
	}
	if record.Action != identityBindActionGroup {
		t.Fatalf("record action = %q, want group", record.Action)
	}
	if record.New.GroupID != bindTestNewGroupID || record.Old.GroupID != bindTestOldGroupID {
		t.Fatalf("group binding = %+v, want %s → %s", record, bindTestNewGroupID, bindTestOldGroupID)
	}
}

// TestIdentityBindCodeGroupWithoutInviterExplains 拿不到邀请人时要明确说明。
func TestIdentityBindCodeGroupWithoutInviterExplains(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	env.addOldLog(t, "第一话")

	oldGroup := groupForTest(t, env, bindTestOldGroupID)
	oldGroup.InviteUserID = "" // 没有邀请人信息

	env.ctx.PrivilegeLevel = 100
	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"bind", "1001"}})
	reply := waitGroupMessage(t, env)
	if !strings.Contains(reply, "邀请人") {
		t.Fatalf("should explain the missing inviter, got %q", reply)
	}
	if _, ok := identityBindLoadCode(identityBindActionGroup, bindTestNewGroupID); ok {
		t.Fatal("no challenge should be registered without a confirmer")
	}
}

// TestIdentityBindCodeCleanupRemovesFinished 已完成的挑战会被清理，不会无限增长。
func TestIdentityBindCodeCleanupRemovesFinished(t *testing.T) {
	resetIdentityBindGlobals()
	identityBindPutCode(&identityBindCodeChallenge{
		Action:     identityBindActionUser,
		New:        identityBindEndpoint{UserID: "OpenQQ:1-a"},
		Old:        identityBindEndpoint{UserID: "QQ:1"},
		Status:     identityBindCodeUsed,
		FinishedAt: time.Now().Add(-2 * identityBindCodeKeepRecordFor).Unix(),
	})
	identityBindPutCode(&identityBindCodeChallenge{
		Action:    identityBindActionUser,
		New:       identityBindEndpoint{UserID: "OpenQQ:1-b"},
		Old:       identityBindEndpoint{UserID: "QQ:2"},
		Status:    identityBindCodePending,
		ExpiresAt: time.Now().Add(-time.Minute).Unix(),
	})
	identityBindCleanupCodes()

	if _, ok := identityBindLoadCode(identityBindActionUser, "OpenQQ:1-a"); ok {
		t.Fatal("a long-finished challenge should be cleaned up")
	}
	// 过期的先被标记，再留一段时间用于排查
	c, ok := identityBindLoadCode(identityBindActionUser, "OpenQQ:1-b")
	if !ok {
		t.Fatal("a just-expired challenge should be kept briefly for diagnosis")
	}
	if c.Status != identityBindCodeExpired {
		t.Fatalf("status = %q, want expired", c.Status)
	}
}

// TestIdentityBindFindOldBotEndpointSkipsOfficial 挑民间 bot 时必须排除官方端点。
func TestIdentityBindFindOldBotEndpointSkipsOfficial(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()

	// 把官方端点当作 exclude 传进去，仍然应该找到 OneBot
	ep := identityBindFindOldBotEndPoint(env.d.ImSession, env.ctx.EndPoint)
	if ep == nil {
		t.Fatal("expected to find the OneBot endpoint")
	}
	if identityBindSupported(ep) {
		t.Fatalf("must not pick an official endpoint, got %q/%q", ep.Platform, ep.ProtocolType)
	}
	if !identityBindSameQQUser(ep.UserID, "900000") {
		t.Fatalf("unexpected endpoint picked: %q", ep.UserID)
	}
}
