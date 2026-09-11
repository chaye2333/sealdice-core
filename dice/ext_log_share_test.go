//nolint:testpackage
package dice

import (
	"testing"

	"sealdice-core/dice/service"
	"sealdice-core/model"
	"sealdice-core/utils/constant"
)

// 本文件锁定「群绑定之后日志真能双向共用」。
//
// 这两条都是实际踩到的回归，且症状很容易被误判成"绑定没生效"：
//  1. .log 状态显示 0 条 —— 查条数用了当前群而不是归一后的群；
//  2. 官 bot 侧 .log new 之后写不进日志 —— 日志行建在了真实群 ID 上，
//     而写入用的是归一后的群 ID，同一个 logID 配了两个不同的 group_id。

// logCmdFor 取注册好的 .log 指令。
func logCmdFor(t *testing.T, env *bindTestEnv) *CmdItemInfo {
	t.Helper()
	ext := env.d.ExtFind("log", false)
	if ext == nil {
		t.Fatal("log extension is not registered")
	}
	cmd, ok := ext.CmdMap["log"]
	if !ok || cmd == nil {
		t.Fatal(".log command is not registered")
	}
	return cmd
}

// runLog 以指定 ctx 执行一条 .log 子指令。
func runLog(t *testing.T, env *bindTestEnv, ctx *MsgContext, args ...string) {
	t.Helper()
	logCmdFor(t, env).Solve(ctx,
		&Message{MessageType: "group", GroupID: ctx.Group.GroupID},
		&CmdArgs{Args: args, RawArgs: joinArgs(args)})
}

func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

// logShareEnv 准备一个「官方 bot + 民间 bot + 已群绑定」的环境。
func logShareEnv(t *testing.T) (env *bindTestEnv, oldEP *EndPointInfo, oldCtx *MsgContext) {
	t.Helper()
	env = newBindTestEnv(t)
	if err := env.operator.GetLogDB(constant.WRITE).AutoMigrate(&model.LogInfo{}, &model.LogOneItem{}); err != nil {
		t.Fatalf("AutoMigrate logs: %v", err)
	}

	oldEP = &EndPointInfo{
		EndPointInfoBase: EndPointInfoBase{
			ID: "ep-onebot", Platform: "QQ", ProtocolType: "onebot",
			UserID: "QQ:900000", Nickname: "OldBot", Enable: true,
		},
		Adapter: &recordingAdapter{},
	}
	oldEP.Session = env.d.ImSession
	env.d.ImSession.EndPoints = append(env.d.ImSession.EndPoints, oldEP)

	oldCtx, _ = newQuitCommandTestContext(t, env.d, oldEP, bindTestOldUserID, bindTestOldGroupID, "旧群")
	groupForTest(t, env, bindTestOldGroupID).Active = true
	bindGroupForTest(t, env)
	groupForTest(t, env, bindTestNewGroupID).Active = true
	return env, oldEP, oldCtx
}

// TestLogBoundStatusCountsOldGroupMessages 民间 bot 在旧群写 7 条，
// 官方 bot 绑群后看 .log 状态必须显示 7 条（曾错误显示 0 条）。
func TestLogBoundStatusCountsOldGroupMessages(t *testing.T) {
	env, _, oldCtx := logShareEnv(t)
	defer env.cleanup()

	oldGroup := groupForTest(t, env, bindTestOldGroupID)
	runLog(t, env, oldCtx, "new", "测试123")

	state := getGroupLogState(oldGroup)
	if !state.On || state.Name != "测试123" {
		t.Fatalf("civilian .log new did not take effect: %+v", state)
	}
	for i := range 7 {
		if ok := LogAppend(&MsgContext{Dice: env.d}, bindTestOldGroupID, state.ID, state.Name, &model.LogOneItem{
			Nickname: "tester", IMUserID: "user", Message: "line", Time: int64(i + 1),
		}); !ok {
			t.Fatalf("LogAppend #%d failed", i)
		}
	}

	officialCtx, _ := newQuitCommandTestContext(t, env.d, env.ctx.EndPoint, bindTestNewUserID, bindTestNewGroupID, "新群")

	// 读取必须改道到旧群（归一后的群）
	if got := identityBindLogReadGroupID(officialCtx, bindTestNewGroupID); got != bindTestOldGroupID {
		t.Fatalf("official read group = %q, want the old group %q", got, bindTestOldGroupID)
	}

	before := env.recorder.messageCount()
	runLog(t, env, officialCtx)
	out := env.recorder.waitNextReply(t, before)
	if !containsAny(out, "已记录文本7条") {
		t.Fatalf("official .log should report the old group's 7 lines, got:\n%s", out)
	}
	if !containsAny(out, "测试123") {
		t.Fatalf("official .log should report the shared story name, got:\n%s", out)
	}
}

// TestLogBoundOfficialSideCanStillRecord 官 bot 绑群后必须还能自己开日志并记录，
// 且日志行要建在归一后的群上（否则写入会找不到那一行）。
func TestLogBoundOfficialSideCanStillRecord(t *testing.T) {
	env, _, oldCtx := logShareEnv(t)
	defer env.cleanup()

	// 民间 bot 先开一份（模拟迁移前就在用的记录）
	oldGroup := groupForTest(t, env, bindTestOldGroupID)
	runLog(t, env, oldCtx, "new", "测试123")
	if st := getGroupLogState(oldGroup); !st.On || st.Name != "测试123" {
		t.Fatalf("civilian .log new failed: %+v", st)
	}

	officialCtx, _ := newQuitCommandTestContext(t, env.d, env.ctx.EndPoint, bindTestNewUserID, bindTestNewGroupID, "新群")

	// 官 bot 侧再新建一份
	before := env.recorder.messageCount()
	runLog(t, env, officialCtx, "new", "官方记录")
	out := env.recorder.waitNextReply(t, before)
	if !containsAny(out, "记录已经开启") {
		t.Fatalf("official .log new should succeed, got: %s", out)
	}

	st := getGroupLogState(oldGroup)
	if st.Name != "官方记录" || !st.On {
		t.Fatalf("state after official .log new = %+v, want on/官方记录", st)
	}

	// 关键回归点：日志行必须建在归一后的群（旧群）上
	rows := logInfoRowsFor(t, env, "官方记录")
	if len(rows) != 1 {
		t.Fatalf("expected exactly one log row named 官方记录, got %d", len(rows))
	}
	if rows[0].GroupID != bindTestOldGroupID {
		t.Fatalf("log row group_id = %q, want the canonical group %q", rows[0].GroupID, bindTestOldGroupID)
	}

	// 官方群里有人说话 → 必须真的落库
	ext := env.d.ExtFind("log", false)
	if ext == nil || ext.OnMessageReceived == nil {
		t.Fatal("log extension has no OnMessageReceived hook")
	}
	recvCtx, recvMsg := newQuitCommandTestContext(t, env.d, env.ctx.EndPoint, bindTestNewUserID, bindTestNewGroupID, "新群")
	recvMsg.Message = "官方群里说的第一句"
	recvMsg.RawID = "raw-1"
	ext.OnMessageReceived(recvCtx, recvMsg)

	lines, okCount := service.LogLinesCountGet(env.d.DBOperator, bindTestOldGroupID, st.Name)
	if !okCount || lines != 1 {
		t.Fatalf("official side recording did not land in the shared log: count=%d ok=%v", lines, okCount)
	}

	// 民间 bot 侧能列出这份记录（双方确实共用一份）
	logs, errList := service.LogGetList(env.d.DBOperator, bindTestOldGroupID)
	if errList != nil {
		t.Fatalf("LogGetList: %v", errList)
	}
	found := false
	for _, n := range logs {
		if n == st.Name {
			found = true
		}
	}
	if !found {
		t.Fatalf("the shared log %q should be listed under the canonical group, got %v", st.Name, logs)
	}
}

// TestGroupBindClearsStaleLogStateOnRealGroup 群绑定必须清掉真实群上残留的日志状态。
//
// 为什么重要：GroupInfo.LogCurName / LogOn 是会持久化的，而 LogCurID 不会。
// 如果官方群上留着一份"记录中"的状态，回退到官方主线之后，官方主线会拿它去
// LogGetOrCreate(官方群ID, 旧群的日志名)，凭空建出一份**空日志**，
// 于是「当前故事」名字对、条数却是 0 —— 看起来像数据丢了。
func TestGroupBindClearsStaleLogStateOnRealGroup(t *testing.T) {
	env := logShareEnvWithoutBinding(t)
	defer env.cleanup()

	// 官方群在绑群之前自己开着一份日志（真实会发生的：先用官 bot，后来才绑群）
	officialGroup := groupForTest(t, env, bindTestNewGroupID)
	officialGroup.SetLogState(12345, "官方群自己的记录", true)
	officialGroup.MarkDirty(env.d)
	if st := getGroupLogState(officialGroup); !st.On || st.Name == "" {
		t.Fatalf("setup failed: %+v", st)
	}

	identityBindResetRealGroupLogState(env.ctx, officialGroup)

	st := getGroupLogState(officialGroup)
	if st.On || st.Name != "" || st.ID != 0 {
		t.Fatalf("stale log state on the real group must be cleared, got %+v", st)
	}
}

// TestGroupBindViaCommandClearsRealGroupState 走完整的 .group bind 流程（验证码确认那一步），
// 确认真实群上的残留状态会被真实路径清掉，而不只是清理函数本身能用。
func TestGroupBindViaCommandClearsRealGroupState(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()

	// 官方群先有一份自己的日志状态
	officialGroup := groupForTest(t, env, bindTestNewGroupID)
	officialGroup.SetLogState(777, "官方群自己的记录", true)
	officialGroup.MarkDirty(env.d)

	// 旧群要有邀请人（群绑定的确认人）
	oldGroup := groupForTest(t, env, bindTestOldGroupID)
	oldGroup.InviteUserID = bindTestOldUserID
	env.addOldLog(t, "旧群记录")

	// 官 bot 侧发起群绑定 → 走验证码流程
	env.ctx.PrivilegeLevel = 100
	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"bind", "1001"}})
	waitGroupMessage(t, env)

	challenge, ok := identityBindLoadCode(identityBindActionGroup, bindTestOldGroupID)
	if !ok || challenge.Status != identityBindCodeDelivered {
		t.Fatalf("expected a delivered group challenge, got %+v / %v", challenge, ok)
	}

	// 邀请人在民间 bot 私聊里回复验证码 → 绑定成立
	oldEP := codeOldEndPoint(t, env)
	oldCtx, oldMsg := newQuitCommandTestContext(t, env.d, oldEP, bindTestOldUserID, bindTestOldGroupID, "旧群")
	oldCtx.IsPrivate = true
	oldMsg.MessageType = "private"
	if !identityBindTryConsumeCode(oldCtx, oldMsg, challenge.Code) {
		t.Fatal("the inviter's code reply should be consumed")
	}

	// 绑定成立后，官方群上的残留日志状态必须被清掉
	st := getGroupLogState(officialGroup)
	if st.On || st.Name != "" {
		t.Fatalf("real group log state must be cleared by the real bind path, got %+v", st)
	}
}

// logShareEnvWithoutBinding 建一个「官方 bot + 民间 bot」但**不**预建群绑定的环境。
func logShareEnvWithoutBinding(t *testing.T) *bindTestEnv {
	t.Helper()
	env := newBindTestEnv(t)
	if err := env.operator.GetLogDB(constant.WRITE).AutoMigrate(&model.LogInfo{}, &model.LogOneItem{}); err != nil {
		t.Fatalf("AutoMigrate logs: %v", err)
	}
	groupForTest(t, env, bindTestOldGroupID).Active = true
	groupForTest(t, env, bindTestNewGroupID).Active = true
	return env
}

// logInfoRowsFor 按名字取 logs 表的行，用于检查 group_id 有没有写错。
func logInfoRowsFor(t *testing.T, env *bindTestEnv, name string) []model.LogInfo {
	t.Helper()
	var rows []model.LogInfo
	if err := env.operator.GetLogDB(constant.WRITE).Where("name = ?", name).Find(&rows).Error; err != nil {
		t.Fatalf("query logs: %v", err)
	}
	return rows
}
