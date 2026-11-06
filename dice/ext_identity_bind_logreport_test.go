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

// 本文件锁定「群绑定到底接上了哪些日志」这件事必须被输出。
//
// 以前的问题：绑定成功只回一句「双方共用同一份日志」，后台日志里什么都没有。
// 旧群没有日志时用户看到的只是一句「暂无记录」，既不知道是旧群号填错了，
// 也不知道该去哪里看原因 —— 本文件的用例就是防止这种静默回归。

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

// TestGroupBindReportsMigratedLogs 群绑定成功时必须列出「接上了哪些日志」。
func TestGroupBindReportsMigratedLogs(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()

	groupForTest(t, env, bindTestOldGroupID).InviteUserID = bindTestOldUserID
	env.addOldLog(t, "追书人")
	env.addOldLog(t, "第一话")

	readLogs := captureBindLogs(env)

	env.ctx.PrivilegeLevel = 100
	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"bind", "1001"}})
	prompt := waitGroupMessage(t, env)
	// 发起阶段就要把旧群现有的日志摆出来
	if !strings.Contains(prompt, "旧群现有日志") || !strings.Contains(prompt, "追书人") {
		t.Fatalf("the start prompt should list the old group logs, got: %s", prompt)
	}

	challenge := groupBindChallengeForTest(t, env)
	reply := confirmGroupBindWithCode(t, env, challenge.Code)
	if !strings.Contains(reply, "日志迁移结果") {
		t.Fatalf("the success reply should contain the log migration report, got: %s", reply)
	}
	if !strings.Contains(reply, "追书人") || !strings.Contains(reply, "第一话") {
		t.Fatalf("the success reply should list every shared log name, got: %s", reply)
	}

	// 后台日志必须有同样一份记录，方便骰主事后排查
	logged := readLogs()
	if !strings.Contains(logged, "日志接续") || !strings.Contains(logged, "追书人") {
		t.Fatalf("the dice log should record which logs were shared, got: %s", logged)
	}
}

// TestGroupBindWarnsWhenOldGroupHasNoLogs 旧群没有日志时，
// 发起阶段与成功回执都必须明说「.log list 会是空的」。
func TestGroupBindWarnsWhenOldGroupHasNoLogs(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()

	groupForTest(t, env, bindTestOldGroupID).InviteUserID = bindTestOldUserID

	readLogs := captureBindLogs(env)

	env.ctx.PrivilegeLevel = 100
	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"bind", "1001"}})
	prompt := waitGroupMessage(t, env)
	if !strings.Contains(prompt, "名下没有任何日志") {
		t.Fatalf("the start prompt should warn about an empty old group, got: %s", prompt)
	}

	challenge := groupBindChallengeForTest(t, env)
	reply := confirmGroupBindWithCode(t, env, challenge.Code)
	if !strings.Contains(reply, "没有查到任何日志") {
		t.Fatalf("the success reply should warn that the list stays empty, got: %s", reply)
	}
	if !strings.Contains(reply, ".log list") {
		t.Fatalf("the warning should point at .log list, got: %s", reply)
	}

	logged := readLogs()
	if !strings.Contains(logged, "旧群日志=0份") {
		t.Fatalf("the dice log should record the empty old group, got: %s", logged)
	}
}

// TestGroupBindSuggestsCandidateGroups 旧群查不到日志时，
// 要把「号码相同但群 ID 不同」的群列出来，让用户能立刻发现旧群号其实填错了。
func TestGroupBindSuggestsCandidateGroups(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()

	groupForTest(t, env, bindTestOldGroupID).InviteUserID = bindTestOldUserID
	// 历史数据落在「只写号码」的群 ID 下：绑 1001 时旧群名下查不到，但候选里能看到。
	row := &model.LogInfo{GroupID: "1001", Name: "旧团记录"}
	if err := env.operator.GetLogDB(constant.WRITE).Create(row).Error; err != nil {
		t.Fatalf("seed candidate log: %v", err)
	}

	env.ctx.PrivilegeLevel = 100
	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"bind", "1001"}})
	waitGroupMessage(t, env)

	challenge := groupBindChallengeForTest(t, env)
	reply := confirmGroupBindWithCode(t, env, challenge.Code)
	if !strings.Contains(reply, "可能与旧群号对不上") {
		t.Fatalf("the report should suggest candidate group ids, got: %s", reply)
	}
	if !strings.Contains(reply, "旧团记录") {
		t.Fatalf("the candidate line should list the logs on that group, got: %s", reply)
	}
}

// TestLogListExplainsEmptyBinding 列表为空时必须说清楚「读的是哪个群」。
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

// TestGroupStatusShowsLogInventory .group status 必须显示旧群日志现状。
func TestGroupStatusShowsLogInventory(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()

	env.addOldLog(t, "追书人")
	bindGroupForTest(t, env)

	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"status"}})
	reply := waitGroupMessage(t, env)
	if !strings.Contains(reply, "旧群日志") || !strings.Contains(reply, "追书人") {
		t.Fatalf("group status should show the old group log inventory, got: %s", reply)
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
