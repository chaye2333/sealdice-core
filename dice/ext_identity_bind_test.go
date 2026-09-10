//nolint:testpackage
package dice

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"sealdice-core/model"
	"sealdice-core/utils/constant"
)

// ---------- 测试脚手架 ----------

const (
	bindTestOldGroupID = "QQ-Group:1001"
	bindTestNewGroupID = "OpenQQ-Group:100-x"
	bindTestOldUserID  = "QQ:2001"
	bindTestNewUserID  = "OpenQQ:100-member-openid"
)

// recordingAdapter 只记录骰娘真正想发出去的文本。
// 直接调用 Solve 时不会走完整发送链路，用它断言回复最稳妥。
type recordingAdapter struct {
	mockPlatformAdapter
	mu          sync.Mutex
	groupTexts  []string
	personTexts []string
}

func (a *recordingAdapter) SendToGroup(_ *MsgContext, _ string, text string, _ string) {
	a.mu.Lock()
	a.groupTexts = append(a.groupTexts, text)
	a.mu.Unlock()
}

func (a *recordingAdapter) SendToPerson(_ *MsgContext, _ string, text string, _ string) {
	a.mu.Lock()
	a.personTexts = append(a.personTexts, text)
	a.mu.Unlock()
}

func (a *recordingAdapter) last() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.groupTexts) > 0 {
		return a.groupTexts[len(a.groupTexts)-1]
	}
	if len(a.personTexts) > 0 {
		return a.personTexts[len(a.personTexts)-1]
	}
	return ""
}

// waitReply 等待回复出现。
// ReplyToSender 是异步的（panicHandler.Once 内部起 goroutine），必须轮询等待。
func (a *recordingAdapter) waitReply(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if text := a.last(); text != "" {
			return text
		}
		time.Sleep(10 * time.Millisecond)
	}
	return ""
}

// messageCount 当前已记录的消息条数，用于等待「下一条」回复。
func (a *recordingAdapter) messageCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.groupTexts) + len(a.personTexts)
}

// waitNextReply 等待并返回一条新的回复（相对于 before 条消息之后）。
func (a *recordingAdapter) waitNextReply(t *testing.T, before int) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a.messageCount() > before {
			return a.last()
		}
		time.Sleep(10 * time.Millisecond)
	}
	return ""
}

func resetIdentityBindGlobals() {
	globalIdentityBindSessions.Range(func(key string, _ *identityBindSession) bool {
		globalIdentityBindSessions.Delete(key)
		return true
	})
	globalIdentityBindLastAttempt.Range(func(key string, _ *identityBindLastAttempt) bool {
		globalIdentityBindLastAttempt.Delete(key)
		return true
	})
}

// bindTestEnv 构建一个可直接调用 .bind 指令的环境。
type bindTestEnv struct {
	d        *Dice
	ctx      *MsgContext
	msg      *Message
	cleanup  func()
	operator *mockDatabaseOperator
	recorder *recordingAdapter
}

func newBindTestEnv(t *testing.T) *bindTestEnv {
	t.Helper()
	resetIdentityBindGlobals()

	d, ep, _, cleanup := newExecuteNewTestDice(t)
	ep.Platform = "QQ"
	ep.ProtocolType = "official"
	d.Config.IdentityBindEnable = true
	d.Config.IdentityBindQuestionCount = 1
	d.Config.IdentityBindCooldownSec = 0
	// 关闭 QQ 发送延迟，否则每条回复都要等几百毫秒
	d.Config.MessageDelayRangeStart = 0
	d.Config.MessageDelayRangeEnd = 0

	recorder := &recordingAdapter{}
	ep.Adapter = recorder

	operator, okOperator := d.DBOperator.(*mockDatabaseOperator)
	if !okOperator {
		cleanup()
		t.Fatalf("unexpected DBOperator type %T", d.DBOperator)
	}
	if err := operator.GetDataDB(constant.WRITE).AutoMigrate(&model.AttributesItemModel{}); err != nil {
		cleanup()
		t.Fatalf("AutoMigrate attrs: %v", err)
	}
	if err := operator.GetLogDB(constant.WRITE).AutoMigrate(&model.LogInfo{}); err != nil {
		cleanup()
		t.Fatalf("AutoMigrate log_info: %v", err)
	}

	ctx, msg := newQuitCommandTestContext(t, d, ep, bindTestNewUserID, bindTestNewGroupID, "新群")
	// 旧群必须在内存里存在，验证题目才能从里面取到角色卡。
	newQuitCommandTestContext(t, d, ep, bindTestOldUserID, bindTestOldGroupID, "旧群")

	return &bindTestEnv{
		d:        d,
		ctx:      ctx,
		msg:      msg,
		operator: operator,
		recorder: recorder,
		cleanup: func() {
			cleanup()
		},
	}
}

// addOldCard 给旧身份添加一张角色卡（会出现在 GetCharacterList 里）。
func (env *bindTestEnv) addOldCard(t *testing.T, name string) {
	t.Helper()
	card := &model.AttributesItemModel{
		Id:        "card-" + name,
		Name:      name,
		OwnerId:   bindTestOldUserID,
		AttrsType: "character",
		SheetType: "coc7",
		Data:      []byte(`{}`),
	}
	if err := env.operator.GetDataDB(constant.WRITE).Create(card).Error; err != nil {
		t.Fatalf("create card %s: %v", name, err)
	}
}

// addOldLog 给旧群添加一条日志记录。
func (env *bindTestEnv) addOldLog(t *testing.T, name string) {
	t.Helper()
	row := &model.LogInfo{
		GroupID: bindTestOldGroupID,
		Name:    name,
	}
	if err := env.operator.GetLogDB(constant.WRITE).Create(row).Error; err != nil {
		t.Fatalf("create log %s: %v", name, err)
	}
}

// ---------- 纯函数测试 ----------

func TestIdentityBindNormalizeOldIDs(t *testing.T) {
	user, err := normalizeIdentityBindUser(" 2001 ")
	if err != nil || user != "QQ:2001" {
		t.Fatalf("normalize user = (%q, %v), want QQ:2001", user, err)
	}
	user, err = normalizeIdentityBindUser("QQ:2001")
	if err != nil || user != "QQ:2001" {
		t.Fatalf("normalize prefixed user = (%q, %v), want QQ:2001", user, err)
	}
	if _, err = normalizeIdentityBindUser("QQ-Group:1001"); err == nil {
		t.Fatal("expected group id to be rejected as a user id")
	}
	if _, err = normalizeIdentityBindUser("abc"); err == nil {
		t.Fatal("expected non-numeric user id to be rejected")
	}

	group, err := normalizeIdentityBindGroup("1001")
	if err != nil || group != "QQ-Group:1001" {
		t.Fatalf("normalize group = (%q, %v), want QQ-Group:1001", group, err)
	}
	if _, err = normalizeIdentityBindGroup(""); err == nil {
		t.Fatal("expected empty group id to be rejected")
	}
}

func TestIdentityBindParseAnswers(t *testing.T) {
	cases := []struct {
		text   string
		count  int
		want   []int
		wantOK bool
	}{
		{"1", 1, []int{1}, true},
		{"132", 3, []int{1, 3, 2}, true},
		{"1 3 2", 3, []int{1, 3, 2}, true},
		{"1,3,2", 3, []int{1, 3, 2}, true},
		{"１３２", 3, nil, false}, // 全角数字不接受
		{"13", 3, nil, false},
		{"1324", 3, nil, false},
		{"abc", 3, nil, false},
	}
	for _, tc := range cases {
		got, ok := identityBindParseAnswers(tc.text, tc.count)
		if ok != tc.wantOK {
			t.Fatalf("parse(%q) ok = %v, want %v", tc.text, ok, tc.wantOK)
		}
		if !ok {
			continue
		}
		if len(got) != len(tc.want) {
			t.Fatalf("parse(%q) = %v, want %v", tc.text, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("parse(%q) = %v, want %v", tc.text, got, tc.want)
			}
		}
	}
}

func TestIdentityBindBuildQuestionsClampsCountToAvailableNames(t *testing.T) {
	questions, ok := identityBindBuildQuestions("你的角色卡名是", []string{"甲"}, []string{"乙", "丙"}, 5)
	if !ok {
		t.Fatal("expected questions to be generated")
	}
	if len(questions) != 1 {
		t.Fatalf("expected question count to be clamped to 1, got %d", len(questions))
	}
	if len(questions[0].Options) < 2 {
		t.Fatalf("expected at least one distractor, got %v", questions[0].Options)
	}
	if questions[0].Options[questions[0].Answer] != "甲" {
		t.Fatalf("answer index points at %q, want 甲", questions[0].Options[questions[0].Answer])
	}
}

func TestIdentityBindBuildQuestionsRejectsEmptyCandidates(t *testing.T) {
	if _, ok := identityBindBuildQuestions("x", nil, []string{"y"}, 1); ok {
		t.Fatal("expected no questions when there are no candidates")
	}
}

func TestIdentityBindCheckAnswersRequiresAllCorrect(t *testing.T) {
	questions := []identityBindQuestion{
		{Options: []string{"a", "b"}, Answer: 1},
		{Options: []string{"c", "d"}, Answer: 0},
	}
	if !identityBindCheckAnswers(questions, []int{2, 1}) {
		t.Fatal("expected all-correct answers to pass")
	}
	if identityBindCheckAnswers(questions, []int{1, 1}) {
		t.Fatal("expected a wrong first answer to fail")
	}
	if identityBindCheckAnswers(questions, []int{2}) {
		t.Fatal("expected a length mismatch to fail")
	}
}

func TestIdentityBindPlatformIsolation(t *testing.T) {
	official := &EndPointInfo{EndPointInfoBase: EndPointInfoBase{Platform: "QQ", ProtocolType: "official"}}
	onebot := &EndPointInfo{EndPointInfoBase: EndPointInfoBase{Platform: "QQ", ProtocolType: "onebot"}}
	tg := &EndPointInfo{EndPointInfoBase: EndPointInfoBase{Platform: "TG"}}

	if !identityBindSupported(official) {
		t.Fatal("official QQ should support identity binding")
	}
	if identityBindSupported(onebot) {
		t.Fatal("OneBot must not use identity binding")
	}
	if identityBindSupported(tg) {
		t.Fatal("other platforms must not use identity binding")
	}
	if identityBindSupported(nil) {
		t.Fatal("nil endpoint must not support identity binding")
	}
}

func TestIdentityBindConfigClamping(t *testing.T) {
	cfg := Config{}
	cfg.IdentityBindQuestionCount = 0
	cfg.IdentityBindCooldownSec = -5
	cfg.FixIdentityBindConfig()
	if cfg.IdentityBindQuestionCount != DefaultConfig.IdentityBindQuestionCount {
		t.Fatalf("expected question count default %d, got %d",
			DefaultConfig.IdentityBindQuestionCount, cfg.IdentityBindQuestionCount)
	}
	if cfg.IdentityBindCooldownSec != 0 {
		t.Fatalf("expected negative cooldown to clamp to 0, got %d", cfg.IdentityBindCooldownSec)
	}

	cfg.IdentityBindQuestionCount = 999
	cfg.IdentityBindCooldownSec = 99999999
	cfg.FixIdentityBindConfig()
	if cfg.IdentityBindQuestionCount != identityBindMaxQuestionCount {
		t.Fatalf("expected question count clamp to %d, got %d",
			identityBindMaxQuestionCount, cfg.IdentityBindQuestionCount)
	}
	if cfg.IdentityBindCooldownSec != identityBindMaxCooldownSec {
		t.Fatalf("expected cooldown clamp to %d, got %d",
			identityBindMaxCooldownSec, cfg.IdentityBindCooldownSec)
	}
}

// ---------- 存储测试 ----------

func TestIdentityBindStorePersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	d := &Dice{BaseConfig: BaseConfig{DataDir: dir}}
	d.IdentityBindStore = &identityBindStore{}

	record := &identityBindRecord{
		Action:  identityBindActionUser,
		Key:     identityBindUserKey(bindTestNewUserID),
		New:     identityBindEndpoint{Platform: "QQ", Protocol: "official", GroupID: bindTestNewGroupID, UserID: bindTestNewUserID},
		Old:     identityBindEndpoint{Platform: "QQ", Protocol: "onebot", GroupID: bindTestOldGroupID, UserID: bindTestOldUserID},
		Created: 123,
		Creator: bindTestNewUserID,
	}
	if err := identityBindStoreOf(d).put(d, record); err != nil {
		t.Fatalf("put: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, identityBindStoreFilename)); err != nil {
		t.Fatalf("expected store file to exist: %v", err)
	}

	// 新实例重新加载
	fresh := &Dice{BaseConfig: BaseConfig{DataDir: dir}}
	fresh.IdentityBindStore = &identityBindStore{}
	got, ok := identityBindStoreOf(fresh).get(fresh, identityBindUserKey(bindTestNewUserID))
	if !ok {
		t.Fatal("expected binding to be reloaded from disk")
	}
	if got.Old.UserID != bindTestOldUserID || got.Old.GroupID != bindTestOldGroupID {
		t.Fatalf("reloaded record = %+v, want old %s / %s", got.Old, bindTestOldGroupID, bindTestOldUserID)
	}

	// 删除后不再可见
	deleted, err := identityBindStoreOf(fresh).delete(fresh, identityBindUserKey(bindTestNewUserID))
	if err != nil || !deleted {
		t.Fatalf("delete = (%v, %v), want (true, nil)", deleted, err)
	}
	reloaded := &Dice{BaseConfig: BaseConfig{DataDir: dir}}
	reloaded.IdentityBindStore = &identityBindStore{}
	if _, ok := identityBindStoreOf(reloaded).get(reloaded, identityBindUserKey(bindTestNewUserID)); ok {
		t.Fatal("expected binding to stay deleted after reload")
	}
}

func TestIdentityBindResolveIgnoresNonOfficialPlatforms(t *testing.T) {
	dir := t.TempDir()
	d := &Dice{
		BaseConfig:        BaseConfig{DataDir: dir},
		IdentityBindStore: &identityBindStore{},
	}
	record := &identityBindRecord{
		Action:  identityBindActionUser,
		Key:     identityBindUserKey(bindTestNewUserID),
		New:     identityBindEndpoint{GroupID: bindTestNewGroupID, UserID: bindTestNewUserID},
		Old:     identityBindEndpoint{GroupID: bindTestOldGroupID, UserID: bindTestOldUserID},
		Created: 123,
	}
	if err := identityBindStoreOf(d).put(d, record); err != nil {
		t.Fatalf("put: %v", err)
	}

	ctx := &MsgContext{
		Dice: d,
		Group: &GroupInfo{
			GroupID: bindTestNewGroupID,
		},
		Player:   &GroupPlayerInfo{UserID: bindTestNewUserID},
		EndPoint: &EndPointInfo{EndPointInfoBase: EndPointInfoBase{Platform: "QQ", ProtocolType: "onebot"}},
	}
	if _, bound := identityBindResolveUserID(ctx); bound {
		t.Fatal("OneBot endpoint must not resolve to a bound identity")
	}
	if _, _, bound := identityBindAttrTarget(ctx); bound {
		t.Fatal("OneBot endpoint must not redirect attribute reads")
	}

	ctx.EndPoint.ProtocolType = "official"
	oldUserID, bound := identityBindResolveUserID(ctx)
	if !bound || oldUserID != bindTestOldUserID {
		t.Fatalf("official endpoint resolve = (%q, %v), want %s", oldUserID, bound, bindTestOldUserID)
	}
}

func TestIdentityBindCooldownTracking(t *testing.T) {
	d := &Dice{}
	d.Config.IdentityBindCooldownSec = 60
	epID := "ep-cooldown"
	userID := "QQ:9001"

	if remaining := identityBindCooldownRemaining(d, epID, userID, identityBindActionUser); remaining != 0 {
		t.Fatalf("expected no cooldown before an attempt, got %v", remaining)
	}
	identityBindMarkAttempt(epID, userID, identityBindActionUser)
	remaining := identityBindCooldownRemaining(d, epID, userID, identityBindActionUser)
	if remaining <= 0 || remaining > 60*1e9 {
		t.Fatalf("expected an active cooldown, got %v", remaining)
	}
	// 冷却关闭时不限制
	d.Config.IdentityBindCooldownSec = 0
	if remaining := identityBindCooldownRemaining(d, epID, userID, identityBindActionUser); remaining != 0 {
		t.Fatalf("expected cooldown to be disabled, got %v", remaining)
	}
}

// ---------- 指令端到端 ----------

func TestIdentityBindCommandDisabledByDefault(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()
	env.d.Config.IdentityBindEnable = false

	result := runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001", "1001"}})
	if !result.Matched || !result.Solved {
		t.Fatalf("expected command to be handled, got %+v", result)
	}
	if reply := waitGroupMessage(t, env); !strings.Contains(reply, "未开启") {
		t.Fatalf("expected a disabled-feature reply, got %q", reply)
	}
}

func TestIdentityBindCommandRejectsOneBot(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()
	env.ctx.EndPoint.ProtocolType = "onebot"

	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001", "1001"}})
	if reply := waitGroupMessage(t, env); !strings.Contains(reply, "仅用于 QQ 官方机器人") {
		t.Fatalf("expected a platform rejection reply, got %q", reply)
	}
}

func TestIdentityBindCommandWithoutOldDataExplainsWhy(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()

	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001", "1001"}})
	if reply := waitGroupMessage(t, env); !strings.Contains(reply, "找不到") {
		t.Fatalf("expected a missing-data reply, got %q", reply)
	}
}

func TestIdentityBindCommandFullFlowBindsOnCorrectAnswer(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()
	env.addOldCard(t, "调查员甲")

	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001", "1001"}})
	questionReply := waitGroupMessage(t, env)
	if !strings.Contains(questionReply, "共 1 题") {
		t.Fatalf("expected a question prompt, got %q", questionReply)
	}

	session, ok := identityBindLoadSession(identityBindSessionKey(env.ctx.EndPoint.ID, bindTestNewUserID, identityBindActionUser))
	if !ok {
		t.Fatal("expected a pending bind session")
	}
	if len(session.Questions) != 1 {
		t.Fatalf("expected 1 question, got %d", len(session.Questions))
	}
	answer := session.Questions[0].Answer + 1

	before := env.recorder.messageCount()
	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{itoa(answer)}})
	if reply := env.recorder.waitNextReply(t, before); !strings.Contains(reply, "绑定成功") {
		t.Fatalf("expected a success reply, got %q", reply)
	}

	record, ok := identityBindStoreOf(env.d).get(env.d, identityBindUserKey(bindTestNewUserID))
	if !ok {
		t.Fatal("expected the binding to be stored")
	}
	if record.Old.UserID != bindTestOldUserID || record.Old.GroupID != bindTestOldGroupID {
		t.Fatalf("stored record = %+v, want old %s / %s", record.Old, bindTestOldGroupID, bindTestOldUserID)
	}
	// 会话必须被消费掉，避免重复提交
	if _, ok := identityBindLoadSession(identityBindSessionKey(env.ctx.EndPoint.ID, bindTestNewUserID, identityBindActionUser)); ok {
		t.Fatal("expected the session to be cleared after success")
	}
}

func TestIdentityBindCommandWrongAnswerFailsAndClearsSession(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()
	env.addOldCard(t, "调查员甲")
	env.addOldCard(t, "调查员乙")

	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001", "1001"}})
	sessionKey := identityBindSessionKey(env.ctx.EndPoint.ID, bindTestNewUserID, identityBindActionUser)
	// 先等到出题消息落地，否则消息计数会算错
	if reply := waitGroupMessage(t, env); !strings.Contains(reply, "共 1 题") {
		t.Fatalf("expected a question prompt, got %q", reply)
	}
	session, ok := identityBindLoadSession(sessionKey)
	if !ok {
		t.Fatal("expected a pending bind session")
	}
	correct := session.Questions[0].Answer + 1
	wrong := 0
	for i := 1; i <= len(session.Questions[0].Options); i++ {
		if i != correct {
			wrong = i
			break
		}
	}
	if wrong == 0 {
		t.Fatal("test setup could not produce a wrong answer")
	}

	before := env.recorder.messageCount()
	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{itoa(wrong)}})
	if reply := env.recorder.waitNextReply(t, before); !strings.Contains(reply, "答案不正确") {
		t.Fatalf("expected a failure reply, got %q", reply)
	}
	if _, ok := identityBindStoreOf(env.d).get(env.d, identityBindUserKey(bindTestNewUserID)); ok {
		t.Fatal("a failed verification must not create a binding")
	}
	if _, ok := identityBindLoadSession(sessionKey); ok {
		t.Fatal("expected the session to be cleared after a wrong answer")
	}
}

func TestIdentityBindCommandCooldownBlocksSecondAttempt(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()
	env.addOldCard(t, "调查员甲")
	env.d.Config.IdentityBindCooldownSec = 3600

	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001", "1001"}})
	first := waitGroupMessage(t, env)
	if strings.Contains(first, "操作过于频繁") {
		t.Fatalf("unexpected cooldown on the first attempt: %q", first)
	}

	// 清掉会话，模拟用户重新发起
	identityBindClearSession(identityBindSessionKey(env.ctx.EndPoint.ID, bindTestNewUserID, identityBindActionUser))

	before := env.recorder.messageCount()
	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001", "1001"}})
	if reply := env.recorder.waitNextReply(t, before); !strings.Contains(reply, "操作过于频繁") {
		t.Fatalf("expected a cooldown reply, got %q", reply)
	}
}

func TestIdentityBindUnbindRemovesBinding(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()

	record := &identityBindRecord{
		Action:  identityBindActionUser,
		Key:     identityBindUserKey(bindTestNewUserID),
		New:     identityBindEndpoint{GroupID: bindTestNewGroupID, UserID: bindTestNewUserID},
		Old:     identityBindEndpoint{GroupID: bindTestOldGroupID, UserID: bindTestOldUserID},
		Created: 1,
	}
	if err := identityBindStoreOf(env.d).put(env.d, record); err != nil {
		t.Fatalf("put: %v", err)
	}

	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"unbind"}})
	if reply := waitGroupMessage(t, env); !strings.Contains(reply, "已解除绑定") {
		t.Fatalf("expected an unbind reply, got %q", reply)
	}
	if _, ok := identityBindStoreOf(env.d).get(env.d, identityBindUserKey(bindTestNewUserID)); ok {
		t.Fatal("expected the binding to be removed")
	}
}

func TestIdentityBindLogCommandRequiresPrivilege(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()
	env.ctx.PrivilegeLevel = 0

	result, handled := runIdentityBindLogCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"bind", "1001"}})
	if !handled {
		t.Fatal("expected .log bind to be handled")
	}
	if !result.Solved {
		t.Fatal("expected .log bind to be solved")
	}
	if reply := waitGroupMessage(t, env); !strings.Contains(reply, "管理权限") {
		t.Fatalf("expected a permission reply, got %q", reply)
	}
}

func TestIdentityBindLogCommandRejectsMissingOldGroup(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()

	runIdentityBindLogCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"bind"}})
	if reply := waitGroupMessage(t, env); !strings.Contains(reply, ".log bind <旧群号>") {
		t.Fatalf("expected a usage reply, got %q", reply)
	}
}

func TestIdentityBindLogCommandBindsOnCorrectAnswer(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()
	env.addOldLog(t, "第一话")

	result, handled := runIdentityBindLogCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"bind", "1001"}})
	if !handled || !result.Solved {
		t.Fatalf("expected .log bind to be handled, got (%+v, %v)", result, handled)
	}
	sessionKey := identityBindSessionKey(env.ctx.EndPoint.ID, bindTestNewUserID, identityBindActionGroup)
	// 先等到出题消息落地，否则消息计数会算错
	if reply := waitGroupMessage(t, env); !strings.Contains(reply, "共 1 题") {
		t.Fatalf("expected a question prompt, got %q", reply)
	}
	session, ok := identityBindLoadSession(sessionKey)
	if !ok {
		t.Fatal("expected a pending log bind session")
	}
	answer := session.Questions[0].Answer + 1

	before := env.recorder.messageCount()
	runIdentityBindLogCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"bind", itoa(answer)}})
	if reply := env.recorder.waitNextReply(t, before); !strings.Contains(reply, "日志绑定成功") {
		t.Fatalf("expected a success reply, got %q", reply)
	}
	record, ok := identityBindStoreOf(env.d).get(env.d, identityBindGroupKey(bindTestNewGroupID))
	if !ok {
		t.Fatal("expected the log binding to be stored")
	}
	if record.Old.GroupID != bindTestOldGroupID {
		t.Fatalf("stored log binding old group = %q, want %q", record.Old.GroupID, bindTestOldGroupID)
	}
}

// itoa 避免测试文件再引入 strconv。
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	negative := v < 0
	if negative {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// waitGroupMessage 等待骰娘发出回复并返回其内容。
func waitGroupMessage(t *testing.T, env *bindTestEnv) string {
	t.Helper()
	if env == nil || env.recorder == nil {
		t.Fatal("test env has no recorder")
	}
	return env.recorder.waitReply(t)
}
