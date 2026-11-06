//nolint:testpackage
package dice

import (
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"sealdice-core/model"
	"sealdice-core/utils/constant"
)

// 本文件锁定群绑定日志清单的展示边界：
//  1. **发起/验证阶段**（绑定还没成立）绝不能把旧群日志名/条数发到 QQ；
//  2. **绑定成立之后**的回执/通知可以列出「迁移了哪些日志」，方便用户确认；
//  3. 完整清单无论如何都要写进骰主后台日志，否则 .log list 为空时无从排查。

// captureLogWriter 收集 zap 输出的原始 JSON，供断言后台日志内容（并发安全）。
type captureLogWriter struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (w *captureLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *captureLogWriter) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// captureBindLogs 把骰子日志改接到内存里，返回读取全文的方法。
func captureBindLogs(env *bindTestEnv) func() string {
	writer := &captureLogWriter{}
	enc := zap.NewProductionEncoderConfig()
	enc.TimeKey = "time"
	enc.EncodeTime = zapcore.ISO8601TimeEncoder
	enc.EncodeLevel = zapcore.CapitalLevelEncoder
	enc.NameKey = "module"
	enc.EncodeName = zapcore.FullNameEncoder
	core := zapcore.NewCore(zapcore.NewJSONEncoder(enc), zapcore.AddSync(writer), zapcore.InfoLevel)
	env.d.Logger = zap.New(core).Sugar()
	return writer.text
}

// groupBindChallengeForTest 从已发起的群绑定里取出验证码挑战。
func groupBindChallengeForTest(t *testing.T, env *bindTestEnv) *identityBindCodeChallenge {
	t.Helper()
	challenge, ok := identityBindLoadCode(identityBindActionGroup, bindTestNewGroupID)
	if !ok || challenge == nil {
		t.Fatal("expected a registered group challenge")
	}
	if challenge.Status != identityBindCodeDelivered {
		t.Fatalf("expected the group challenge to be delivered, got %+v", challenge)
	}
	return challenge
}

// confirmGroupBindWithCode 让旧群邀请人在民间 bot 私聊里回复验证码，返回回执。
func confirmGroupBindWithCode(t *testing.T, env *bindTestEnv, code string) string {
	t.Helper()
	oldEP := codeOldEndPoint(t, env)
	// 回执是 identityBindReplyPerson 发回「民间 bot 侧回复验证码的那个人」，
	// 所以要去旧端点的 recorder 里取。
	oldRecorder, ok := oldEP.Adapter.(*recordingAdapter)
	if !ok {
		t.Fatalf("unexpected old endpoint adapter type %T", oldEP.Adapter)
	}
	oldCtx, oldMsg := newQuitCommandTestContext(t, env.d, oldEP, bindTestOldUserID, bindTestOldGroupID, "旧群")
	oldCtx.IsPrivate = true
	oldMsg.MessageType = "private"
	before := oldRecorder.messageCount()
	if !identityBindTryConsumeCode(oldCtx, oldMsg, code) {
		t.Fatal("the inviter code reply should be consumed")
	}
	return oldRecorder.waitNextReply(t, before)
}

// TestGroupBindStartHidesInventoryButSuccessShowsIt 发起阶段不泄露，成功后回执可以显示。
func TestGroupBindStartHidesInventoryButSuccessShowsIt(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()

	groupForTest(t, env, bindTestOldGroupID).InviteUserID = bindTestOldUserID
	env.addOldLog(t, "追书人")
	env.addOldLog(t, "第一话")

	readLogs := captureBindLogs(env)

	env.ctx.PrivilegeLevel = 100
	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"bind", "1001"}})
	prompt := waitGroupMessage(t, env)
	// 还没验证，绝不能出现旧群日志清单
	if strings.Contains(prompt, "追书人") || strings.Contains(prompt, "第一话") ||
		strings.Contains(prompt, "旧群现有日志") || strings.Contains(prompt, "日志迁移结果") {
		t.Fatalf("the start prompt must not leak the old group log inventory, got: %s", prompt)
	}

	challenge := groupBindChallengeForTest(t, env)
	reply := confirmGroupBindWithCode(t, env, challenge.Code)
	// 绑定已经成立，回执里应该告诉用户迁移了哪些日志
	for _, want := range []string{"日志迁移结果", "追书人", "第一话"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("the success reply should contain %q, got: %s", want, reply)
		}
	}

	// 后台日志始终要有完整清单，骰主才能排查
	logged := readLogs()
	for _, want := range []string{"日志接续", "追书人", "第一话"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("the dice log should contain %q, got: %s", want, logged)
		}
	}
}

// TestGroupBindEmptyOldGroupStartSilentSuccessWarns 旧群没有日志：发起时不提示，成功后回执警告。
func TestGroupBindEmptyOldGroupStartSilentSuccessWarns(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()

	groupForTest(t, env, bindTestOldGroupID).InviteUserID = bindTestOldUserID

	readLogs := captureBindLogs(env)

	env.ctx.PrivilegeLevel = 100
	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"bind", "1001"}})
	prompt := waitGroupMessage(t, env)
	if strings.Contains(prompt, "名下没有任何日志") || strings.Contains(prompt, "没有查到任何日志") {
		t.Fatalf("the start prompt must not mention the old group's logs, got: %s", prompt)
	}

	challenge := groupBindChallengeForTest(t, env)
	reply := confirmGroupBindWithCode(t, env, challenge.Code)
	if !strings.Contains(reply, "没有查到任何日志") {
		t.Fatalf("the success reply should warn that the list stays empty, got: %s", reply)
	}

	logged := readLogs()
	if !strings.Contains(logged, "旧群日志=0份") {
		t.Fatalf("the dice log should record the empty old group, got: %s", logged)
	}
}

// TestGroupBindCandidateGroupsShownAfterSuccess 候选群提示只在成功后显示，同时进后台日志。
func TestGroupBindCandidateGroupsShownAfterSuccess(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()

	groupForTest(t, env, bindTestOldGroupID).InviteUserID = bindTestOldUserID
	// 历史数据落在「只写号码」的群 ID 下：绑 1001 时旧群名下查不到，但候选里能看到。
	row := &model.LogInfo{GroupID: "1001", Name: "旧团记录"}
	if err := env.operator.GetLogDB(constant.WRITE).Create(row).Error; err != nil {
		t.Fatalf("seed candidate log: %v", err)
	}

	readLogs := captureBindLogs(env)

	env.ctx.PrivilegeLevel = 100
	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"bind", "1001"}})
	prompt := waitGroupMessage(t, env)
	if strings.Contains(prompt, "旧团记录") || strings.Contains(prompt, "可能与旧群号对不上") {
		t.Fatalf("the start prompt must not leak candidate groups, got: %s", prompt)
	}

	challenge := groupBindChallengeForTest(t, env)
	reply := confirmGroupBindWithCode(t, env, challenge.Code)
	if !strings.Contains(reply, "可能与旧群号对不上") || !strings.Contains(reply, "旧团记录") {
		t.Fatalf("the success reply should suggest candidate groups, got: %s", reply)
	}

	logged := readLogs()
	if !strings.Contains(logged, "候选群") || !strings.Contains(logged, "旧团记录") {
		t.Fatalf("the dice log should record the candidate groups, got: %s", logged)
	}
}

// TestLogListExplainsEmptyBinding 列表为空时必须说清楚「读的是哪个群」（不含日志清单）。
func TestLogListExplainsEmptyBinding(t *testing.T) {
	env, _, _ := logShareEnv(t)
	defer env.cleanup()

	officialCtx, _ := newQuitCommandTestContext(t, env.d, env.ctx.EndPoint, bindTestNewUserID, bindTestNewGroupID, "新群")
	before := env.recorder.messageCount()
	runLog(t, env, officialCtx, "list")
	out := env.recorder.waitNextReply(t, before)
	if !containsAny(out, "暂无记录") {
		t.Fatalf("expected an empty list, got: %s", out)
	}
	if !containsAny(out, "已绑定旧群") || !containsAny(out, bindTestOldGroupID) {
		t.Fatalf("the empty list should explain the binding, got: %s", out)
	}
}

// TestGroupStatusDoesNotLeakLogInventory .group status 任何人都能看，不能列出旧群日志。
func TestGroupStatusDoesNotLeakLogInventory(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()

	env.addOldLog(t, "追书人")
	bindGroupForTest(t, env)

	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"status"}})
	reply := waitGroupMessage(t, env)
	if strings.Contains(reply, "追书人") || strings.Contains(reply, "旧群日志") {
		t.Fatalf("group status must not leak the old group log inventory, got: %s", reply)
	}
	if !strings.Contains(reply, "已绑定旧群") {
		t.Fatalf("group status should still show the binding, got: %s", reply)
	}
}

// TestGroupDoctorLogInventoryGoesOnlyToBackend 自检回执发在群里，同样不能带日志清单。
func TestGroupDoctorLogInventoryGoesOnlyToBackend(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()

	env.addOldLog(t, "追书人")
	bindGroupForTest(t, env)

	readLogs := captureBindLogs(env)
	env.ctx.PrivilegeLevel = 100
	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"doctor"}})
	reply := waitGroupMessage(t, env)
	if strings.Contains(reply, "追书人") || strings.Contains(reply, "旧群日志") {
		t.Fatalf("group doctor must not leak the old group log inventory, got: %s", reply)
	}
	if !strings.Contains(reply, "[正常]") {
		t.Fatalf("group doctor should still report a healthy binding, got: %s", reply)
	}

	logged := readLogs()
	if !strings.Contains(logged, "绑定自检: 旧群=") || !strings.Contains(logged, "追书人") {
		t.Fatalf("group doctor should write the log inventory to the dice log, got: %s", logged)
	}
}

// TestGroupUnbindReportsLogRestore 解绑回执要说明「日志恢复各看各的」。
func TestGroupUnbindReportsLogRestore(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()
	bindGroupForTest(t, env)

	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"unbind"}})
	reply := waitGroupMessage(t, env)
	if !strings.Contains(reply, "已解除绑定") || !strings.Contains(reply, "旧群") {
		t.Fatalf("unbind should report the log restore, got: %s", reply)
	}
}
