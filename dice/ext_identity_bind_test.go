//nolint:testpackage
package dice

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ds "github.com/sealdice/dicescript"

	"sealdice-core/dice/service"
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
	// 验证码挑战也是包级全局，必须在每个用例开始前清干净，
	// 否则上一个用例留下的挑战会污染下一个。
	globalIdentityBindCodes.Range(func(key string, _ *identityBindCodeChallenge) bool {
		globalIdentityBindCodes.Delete(key)
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
	d.Config.IdentityBindCooldownSec = 0
	// 这个环境专门用来测**答题路径**，所以显式关掉验证码。
	// （产品默认是开着验证码的，见 DefaultConfig；需要验证码的用例自己打开。）
	// 两边都显式声明，测试的意图才不会被默认值变化悄悄改掉。
	d.Config.IdentityBindUseVerificationCode = false
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

// ---------- 双向共享（阶段2）测试 ----------

// bindUserForTest 直接往存储里塞一条个人绑定，返回官方侧和旧侧的两个 ctx。
// 用于测试「绑定之后两侧看到的是同一份数据」。
func bindUserForTest(t *testing.T, env *bindTestEnv) (officialCtx, oldCtx *MsgContext) {
	t.Helper()

	record := &identityBindRecord{
		Action: identityBindActionUser,
		New:    identityBindEndpoint{Platform: "QQ", Protocol: "official", GroupID: bindTestNewGroupID, UserID: bindTestNewUserID},
		Old:    identityBindEndpoint{Platform: "QQ", Protocol: "onebot", GroupID: bindTestOldGroupID, UserID: bindTestOldUserID},
	}
	if err := identityBindStoreOf(env.d).put(env.d, record); err != nil {
		t.Fatalf("put user binding: %v", err)
	}

	// 官方侧 bot 的 ctx
	officialCtx, _ = newQuitCommandTestContext(t, env.d, env.ctx.EndPoint, bindTestNewUserID, bindTestNewGroupID, "新群")
	// 民间 bot 的 ctx（同一个海豹实例里的另一个连接方式）
	oldEP := &EndPointInfo{EndPointInfoBase: EndPointInfoBase{
		ID:           "ep-onebot",
		Platform:     "QQ",
		ProtocolType: "onebot",
		UserID:       "QQ:9999",
	}}
	oldCtx, _ = newQuitCommandTestContext(t, env.d, oldEP, bindTestOldUserID, bindTestOldGroupID, "旧群")
	return officialCtx, oldCtx
}

// TestIdentityBindSharesCharacterCardBothWays 双向共享的核心验收：
// 官方身份建的卡，旧号能看到；旧号建的卡，官方身份也能看到。
func TestIdentityBindSharesCharacterCardBothWays(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()
	officialCtx, oldCtx := bindUserForTest(t, env)

	am := env.d.AttrsManager

	// 官方身份建一张卡
	created, errCreate := am.CharNew(identityBindDataUserID(officialCtx), "官方侧建的卡", "coc7")
	if errCreate != nil {
		t.Fatalf("CharNew from official side: %v", errCreate)
	}
	if created.OwnerId != bindTestOldUserID {
		t.Fatalf("card owner = %q, want the canonical (old) id %q", created.OwnerId, bindTestOldUserID)
	}

	// 旧号侧必须能看到（owner_id 被归一到了旧 QQ 号）
	list, err := am.GetCharacterList(identityBindDataUserID(oldCtx))
	if err != nil {
		t.Fatalf("GetCharacterList from old side: %v", err)
	}
	if !containsIdentityBindName(list, "官方侧建的卡") {
		t.Fatalf("old side should see the card created by the official side, got %+v", list)
	}
	// 而且两边算出来的 owner 必须是同一个
	if got, want := identityBindDataUserID(officialCtx), identityBindDataUserID(oldCtx); got != want {
		t.Fatalf("both sides must resolve to the same owner id, got %q vs %q", got, want)
	}
	if got := identityBindDataUserID(officialCtx); got != bindTestOldUserID {
		t.Fatalf("official side data user id = %q, want the old id %q", got, bindTestOldUserID)
	}

	// 反过来：旧号侧建卡，官方侧能看到
	if _, errCreate2 := am.CharNew(identityBindDataUserID(oldCtx), "旧侧建的卡", "dnd5e"); errCreate2 != nil {
		t.Fatalf("CharNew from old side: %v", errCreate2)
	}
	list, err = am.GetCharacterList(identityBindDataUserID(officialCtx))
	if err != nil {
		t.Fatalf("GetCharacterList from official side: %v", err)
	}
	if !containsIdentityBindName(list, "旧侧建的卡") {
		t.Fatalf("official side should see the card created by the old side, got %+v", list)
	}
}

func containsIdentityBindName(items []*model.AttributesItemModel, name string) bool {
	for _, item := range items {
		if item != nil && item.Name == name {
			return true
		}
	}
	return false
}

// TestIdentityBindSharesAttributesAcrossBots 属性要两边共享：
// 一侧改完，另一侧读到的必须是改后的值。
//
// 注意需要**个人绑定 + 群绑定同时存在**：个人绑定只把用户维度指过去，
// 群维度由群绑定负责，两者合起来才会得到一模一样的 (群ID, 用户ID)。
func TestIdentityBindSharesAttributesAcrossBots(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()
	officialCtx, oldCtx := bindUserForTest(t, env)
	bindGroupForTest(t, env)

	am := env.d.AttrsManager

	// 官方侧写入
	attrs, err := am.LoadByCtx(officialCtx)
	if err != nil {
		t.Fatalf("LoadByCtx(official): %v", err)
	}
	attrs.Store("生命值", ds.NewIntVal(12))
	attrs.SaveToDB(am.db)

	// 旧号侧读到的应该是同一个对象/同一个值
	fromOld, err := am.LoadByCtx(oldCtx)
	if err != nil {
		t.Fatalf("LoadByCtx(old): %v", err)
	}
	if fromOld != attrs {
		t.Fatalf("both sides must load the very same attribute item, got %q vs %q", fromOld.ID, attrs.ID)
	}
	if got := fromOld.Load("生命值"); !ds.ValueEqual(got, ds.NewIntVal(12), false) {
		t.Fatalf("old side reads 生命值 = %+v, want 12", got)
	}

	// 旧号侧改，官方侧也要看到
	fromOld.Store("生命值", ds.NewIntVal(3))
	fromOld.SaveToDB(am.db)
	again, err := am.LoadByCtx(officialCtx)
	if err != nil {
		t.Fatalf("LoadByCtx(official, again): %v", err)
	}
	if got := again.Load("生命值"); !ds.ValueEqual(got, ds.NewIntVal(3), false) {
		t.Fatalf("official side reads 生命值 = %+v, want 3 after the old side changed it", got)
	}
}

// TestIdentityBindUserBindingAloneDoesNotBridgeGroups 只有个人绑定、没有群绑定时，
// 跨群不应该被"假装"打通——否则官方号在别的群里会读到不相干的旧群数据。
// 这是刻意的语义：用户维度归用户绑定管，群维度归群绑定管。
func TestIdentityBindUserBindingAloneDoesNotBridgeGroups(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()
	officialCtx, oldCtx := bindUserForTest(t, env)

	// 用户维度已归一
	if got := identityBindDataUserID(officialCtx); got != bindTestOldUserID {
		t.Fatalf("data user id = %q, want %q", got, bindTestOldUserID)
	}
	// 群维度保持各自真实群
	if got := identityBindDataGroupID(officialCtx); got != bindTestNewGroupID {
		t.Fatalf("official data group id = %q, want its own group %q", got, bindTestNewGroupID)
	}
	if got := identityBindDataGroupID(oldCtx); got != bindTestOldGroupID {
		t.Fatalf("old data group id = %q, want its own group %q", got, bindTestOldGroupID)
	}

	am := env.d.AttrsManager
	attrs, err := am.LoadByCtx(officialCtx)
	if err != nil {
		t.Fatalf("LoadByCtx(official): %v", err)
	}
	attrs.Store("生命值", ds.NewIntVal(12))
	attrs.SaveToDB(am.db)

	// 旧群那份不应该被写入（因为没有群绑定）
	fromOld, err := am.LoadByCtx(oldCtx)
	if err != nil {
		t.Fatalf("LoadByCtx(old): %v", err)
	}
	if got := fromOld.Load("生命值"); got != nil {
		t.Fatalf("without a group binding the two groups must stay separate, got %+v", got)
	}
}

// TestIdentityBindDataIDsFallBackWithoutBinding 没有绑定时，Data*ID 必须等于真实 ID，
// 也就是行为与改动前完全一致（防止改坏非绑定场景）。
func TestIdentityBindDataIDsFallBackWithoutBinding(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()

	ctx, _ := newQuitCommandTestContext(t, env.d, env.ctx.EndPoint, bindTestNewUserID, bindTestNewGroupID, "新群")
	// 刻意不设置 Data*ID，模拟绕过 GetPlayerInfoBySenderRaw 的路径
	if got := identityBindDataUserID(ctx); got != bindTestNewUserID {
		t.Fatalf("unbound data user id = %q, want %q", got, bindTestNewUserID)
	}
	if got := identityBindDataGroupID(ctx); got != bindTestNewGroupID {
		t.Fatalf("unbound data group id = %q, want %q", got, bindTestNewGroupID)
	}
	ctx.DataUserID = bindTestNewUserID
	ctx.DataGroupID = bindTestNewGroupID
	if got := identityBindDataUserID(ctx); got != bindTestNewUserID {
		t.Fatalf("data user id with explicit fill = %q, want %q", got, bindTestNewUserID)
	}
}

// TestIdentityBindDataIDsRecoverFromDelegation 代骰等逻辑会把 ctx.Player 换成别人，
// 这时 Data*ID 如果还是旧值就会造成错配，必须能按当前 Player 重新解析。
func TestIdentityBindDataIDsRecoverFromDelegation(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()
	officialCtx, _ := bindUserForTest(t, env)

	// 先按官方身份填好
	officialCtx.DataUserID = identityBindDataUserID(officialCtx)
	if officialCtx.DataUserID != bindTestOldUserID {
		t.Fatalf("setup: data user id = %q, want %q", officialCtx.DataUserID, bindTestOldUserID)
	}

	// 模拟代骰：Player 被换成另一个没有绑定的人，但 DataUserID 忘了更新
	officialCtx.Player = &GroupPlayerInfo{UserID: "OpenQQ:100-someone-else"}
	if got := identityBindDataUserID(officialCtx); got != "OpenQQ:100-someone-else" {
		t.Fatalf("stale DataUserID must be re-resolved, got %q", got)
	}
}

// bindGroupForTest 给「官方群 → 旧群」建立群绑定。
func bindGroupForTest(t *testing.T, env *bindTestEnv) {
	t.Helper()
	record := &identityBindRecord{
		Action: identityBindActionGroup,
		New:    identityBindEndpoint{Platform: "QQ", Protocol: "official", GroupID: bindTestNewGroupID},
		Old:    identityBindEndpoint{Platform: "QQ", Protocol: "onebot", GroupID: bindTestOldGroupID},
	}
	if err := identityBindStoreOf(env.d).put(env.d, record); err != nil {
		t.Fatalf("put group binding: %v", err)
	}
}

// TestIdentityBindGroupRedirectsAttributes 群绑定之后，属性读写的群维度也要归一。
func TestIdentityBindGroupRedirectsAttributes(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()
	bindGroupForTest(t, env)

	ctx, _ := newQuitCommandTestContext(t, env.d, env.ctx.EndPoint, bindTestNewUserID, bindTestNewGroupID, "新群")
	if got := identityBindDataGroupID(ctx); got != bindTestOldGroupID {
		t.Fatalf("data group id = %q, want the old group %q", got, bindTestOldGroupID)
	}
	if got := identityBindDataUserID(ctx); got != bindTestNewUserID {
		t.Fatalf("group binding must not change the user id, got %q", got)
	}

	// 在归一后的群维度上写属性，旧群的 ctx 必须读到
	am := env.d.AttrsManager
	attrs, err := am.LoadByCtx(ctx)
	if err != nil {
		t.Fatalf("LoadByCtx: %v", err)
	}
	attrs.Store("意志", ds.NewIntVal(55))
	attrs.SaveToDB(am.db)

	oldEP := &EndPointInfo{EndPointInfoBase: EndPointInfoBase{
		ID: "ep-onebot", Platform: "QQ", ProtocolType: "onebot", UserID: "QQ:9999",
	}}
	oldCtx, _ := newQuitCommandTestContext(t, env.d, oldEP, bindTestNewUserID, bindTestOldGroupID, "旧群")
	fromOld, err := am.LoadByCtx(oldCtx)
	if err != nil {
		t.Fatalf("LoadByCtx(old group): %v", err)
	}
	if got := fromOld.Load("意志"); !ds.ValueEqual(got, ds.NewIntVal(55), false) {
		t.Fatalf("old group reads 意志 = %+v, want 55", got)
	}
}

// TestIdentityBindSnTemplateFallsBackToOtherSide 方案 A：
// 自己没设 .sn 时读另一侧的模板；自己设过就以自己的为准。
func TestIdentityBindSnTemplateFallsBackToOtherSide(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()
	officialCtx, oldCtx := bindUserForTest(t, env)

	// 旧号设过模板，官方侧没设 → 官方侧应该读到旧号的模板
	oldCtx.Player.AutoSetNameTemplate = "{$t玩家_RAW} HP{生命值}"
	if got := identityBindPlayerNameTemplate(officialCtx); got != "{$t玩家_RAW} HP{生命值}" {
		t.Fatalf("official side should inherit the old template, got %q", got)
	}

	// 官方侧一旦自己设了，就以自己的为准（不会反过来被旧群覆盖）
	officialCtx.Player.AutoSetNameTemplate = "{$t玩家_RAW} SAN{理智}"
	if got := identityBindPlayerNameTemplate(officialCtx); got != "{$t玩家_RAW} SAN{理智}" {
		t.Fatalf("own template must win, got %q", got)
	}
	// 旧群侧的模板不受影响，即官方侧写入没有污染旧群
	if got := identityBindPlayerNameTemplate(oldCtx); got != "{$t玩家_RAW} HP{生命值}" {
		t.Fatalf("old side template must stay untouched, got %q", got)
	}
}

// TestIdentityBindSnTemplateOffIsHonoured 旧号显式 .sn off（空模板）不能被当成"没设置"而回退。
// 这里断言的是「两侧都空 → 结果为空」，不会凭空从别处继承出模板。
func TestIdentityBindSnTemplateEmptyWhenBothSidesEmpty(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()
	officialCtx, oldCtx := bindUserForTest(t, env)

	officialCtx.Player.AutoSetNameTemplate = ""
	oldCtx.Player.AutoSetNameTemplate = ""
	if got := identityBindPlayerNameTemplate(officialCtx); got != "" {
		t.Fatalf("expected an empty template, got %q", got)
	}
}

// ---------- 日志双向共通（阶段3）测试 ----------

// logBindTestEnv 建一个带日志表的绑定测试环境，并把官方群和旧群都放进内存。
//
// 注意：它已经建好了绑定和两边的 ctx，调用方用 officialCtxForTest/oldCtxForTest 取，
// 不要再次调用 bindUserForTest，否则会用空群对象覆盖掉内存里的 group（丢掉日志状态）。
func logBindTestEnv(t *testing.T) *bindTestEnv {
	t.Helper()
	env := newBindTestEnv(t)
	if err := env.operator.GetLogDB(constant.WRITE).AutoMigrate(&model.LogInfo{}, &model.LogOneItem{}); err != nil {
		t.Fatalf("AutoMigrate logs: %v", err)
	}
	return env
}

// groupForTest 从内存取群对象。
func groupForTest(t *testing.T, env *bindTestEnv, groupID string) *GroupInfo {
	t.Helper()
	group, ok := env.d.ImSession.ServiceAtNew.Load(groupID)
	if !ok || group == nil {
		t.Fatalf("group %q missing from ServiceAtNew", groupID)
	}
	return group
}

// logShareTestEnv 建好「个人绑定 + 群绑定」并返回两边的 ctx。
// 注意：绑定必须在建 ctx 之前放好，而且不能再重复调用 bindUserForTest，
// 否则会用新的空群对象覆盖内存里的 group，把日志状态弄丢。
func logShareTestEnv(t *testing.T) (env *bindTestEnv, officialCtx, oldCtx *MsgContext) {
	t.Helper()
	env = logBindTestEnv(t)
	officialCtx, oldCtx = bindUserForTest(t, env)
	bindGroupForTest(t, env)
	return env, officialCtx, oldCtx
}

// TestIdentityBindLogStateIsShared 群绑定之后，两边必须读到同一份日志状态。
//
// 这是「双向共通」最容易出错的一环：日志状态本来挂在各自的 GroupInfo 上，
// 官方群 .log on 之后旧群不知道，于是官方群记的日志在旧群一条都看不到。
func TestIdentityBindLogStateIsShared(t *testing.T) {
	env, officialCtx, oldCtx := logShareTestEnv(t)
	defer env.cleanup()

	// 官方群对象和旧群对象都必须已经从内存里拿到
	newGroup := groupForTest(t, env, bindTestNewGroupID)
	oldGroup := groupForTest(t, env, bindTestOldGroupID)

	// 在旧群上开启日志（模拟民间 bot 那边 .log on）
	logID, err := service.LogGetOrCreate(env.d.DBOperator, bindTestOldGroupID, "跑团记录")
	if err != nil {
		t.Fatalf("LogGetOrCreate: %v", err)
	}
	oldGroup.SetLogState(logID, "跑团记录", true)

	// 旧群自己的上下文：目标就是自己，不应该被"改道"
	if got, bound := identityBindReadGroup(oldCtx); bound || got != oldGroup {
		t.Fatalf("old group context should stay on itself, got %v / bound=%v", got, bound)
	}
	// 官方群的上下文：必须解析到旧群那份状态
	fromOfficial, bound := identityBindReadGroup(officialCtx)
	if !bound || fromOfficial != oldGroup {
		t.Fatalf("official group context should resolve to the old group, got %v / %v", fromOfficial, bound)
	}
	state := getGroupLogState(fromOfficial)
	if !state.On || state.Name != "跑团记录" || state.ID != logID {
		t.Fatalf("official side reads log state %+v, want on/跑团记录/%d", state, logID)
	}
	// 旧群对象上的状态同样可见——两边读的是同一个对象，这就是"共通"
	if oldState := getGroupLogState(oldGroup); oldState.Name != "跑团记录" || !oldState.On {
		t.Fatalf("old side reads log state %+v, want the same one", oldState)
	}
	// 官方群自己对象上是空的——状态只存在于归一后的群里
	if own := getGroupLogState(newGroup); own.On {
		t.Fatalf("official group object should not hold its own log state, got %+v", own)
	}
}

// TestIdentityBindLogWriteGoesToCanonicalGroup 玩家发言和骰子发言必须写进同一份日志。
//
// 玩家发言走 ctx.Group.GroupID（已归一），骰子发言走 msg.GroupID（真实群），
// 两条路径如果不统一，日志会裂成「玩家一条、骰子一条」两份文件。
func TestIdentityBindLogWriteGoesToCanonicalGroup(t *testing.T) {
	env, officialCtx, _ := logShareTestEnv(t)
	defer env.cleanup()

	// 玩家发言：归一后的群
	if got := identityBindDataGroupID(officialCtx); got != bindTestOldGroupID {
		t.Fatalf("player message group = %q, want %q", got, bindTestOldGroupID)
	}
	// 骰子发言：官方群发出来的消息，也必须落到旧群那份日志里
	if got := identityBindLogWriteGroupID(officialCtx, bindTestNewGroupID); got != bindTestOldGroupID {
		t.Fatalf("dice message group = %q, want %q", got, bindTestOldGroupID)
	}
	// 旧群自己发消息时不该被改道
	if got := identityBindLogWriteGroupID(officialCtx, bindTestOldGroupID); got != bindTestOldGroupID {
		t.Fatalf("old group message group = %q, want %q", got, bindTestOldGroupID)
	}

	// 完全没有群绑定时必须原样返回（防止改坏非绑定场景）
	unboundCtx := &MsgContext{Dice: env.d, Group: &GroupInfo{GroupID: "OpenQQ-Group:999-unbound"}}
	if got := identityBindLogWriteGroupID(unboundCtx, "OpenQQ-Group:999-unbound"); got != "OpenQQ-Group:999-unbound" {
		t.Fatalf("unbound group = %q, want unchanged", got)
	}
	// ctx 缺群时用 fallback；这里刻意用一个没绑定的群，验证不会被别的绑定串改
	unboundGroupID := "OpenQQ-Group:999-unbound"
	if got := identityBindLogWriteGroupID(&MsgContext{Dice: env.d}, unboundGroupID); got != unboundGroupID {
		t.Fatalf("nil group = %q, want fallback unchanged", got)
	}
}

// TestIdentityBindLogReadPrefersGroupWithHistory 读取时优先取「确实有日志」的那一侧。
// 官方向旧看（旧群有历史），旧群看自己；谁都不会因为归一而突然读不到东西。
func TestIdentityBindLogReadPrefersGroupWithHistory(t *testing.T) {
	env, officialCtx, oldCtx := logShareTestEnv(t)
	defer env.cleanup()

	// 一开始两边都没有日志：读取保持原样，不能强行改道
	if got := identityBindLogReadGroupID(officialCtx, bindTestNewGroupID); got != bindTestNewGroupID {
		t.Fatalf("with no logs anywhere, read group = %q, want unchanged", got)
	}

	// 给旧群写一条日志
	if ok := LogAppend(&MsgContext{Dice: env.d}, bindTestOldGroupID, 0, "旧群记录", &model.LogOneItem{
		Nickname: "tester", IMUserID: "user", Message: "line",
	}); !ok {
		t.Fatal("LogAppend to old group failed")
	}

	// 官方群读取时应该改道到旧群
	if got := identityBindLogReadGroupID(officialCtx, bindTestNewGroupID); got != bindTestOldGroupID {
		t.Fatalf("official read group = %q, want the old group %q", got, bindTestOldGroupID)
	}
	// 旧群读取时目标就是自己，不需要改道
	if got := identityBindLogReadGroupID(oldCtx, bindTestOldGroupID); got != bindTestOldGroupID {
		t.Fatalf("old read group = %q, want %q", got, bindTestOldGroupID)
	}
}

// ---------- 自检（阶段4）测试 ----------

// TestIdentityBindDoctorReportsHealthyBindings 正常绑定时自检应该报「正常」。
func TestIdentityBindDoctorReportsHealthyBindings(t *testing.T) {
	env, officialCtx, _ := logShareTestEnv(t)
	defer env.cleanup()

	report := identityBindDoctorReport(env.d, officialCtx)
	if !strings.Contains(report, "绑定记录数: 2") {
		t.Fatalf("expected 2 bindings in the report, got:\n%s", report)
	}
	if !strings.Contains(report, "[正常]") {
		t.Fatalf("expected a healthy marker, got:\n%s", report)
	}
	if !strings.Contains(report, "异常 0 条") {
		t.Fatalf("expected zero problems, got:\n%s", report)
	}
	if !strings.Contains(report, "所有绑定看起来都正常") {
		t.Fatalf("expected a healthy conclusion, got:\n%s", report)
	}
}

// TestIdentityBindDoctorDetectsMissingOldGroup 目标旧群不在内存里时必须被指出来。
// 这是最容易踩的坑：绑定成功但数据没变，因为读取静默回退了。
func TestIdentityBindDoctorDetectsMissingOldGroup(t *testing.T) {
	env, officialCtx, _ := logShareTestEnv(t)
	defer env.cleanup()

	// 把旧群从内存里摘掉，模拟「机器人很久没去过旧群」
	env.d.ImSession.ServiceAtNew.Delete(bindTestOldGroupID)

	report := identityBindDoctorReport(env.d, officialCtx)
	if !strings.Contains(report, "不在内存里") {
		t.Fatalf("expected the missing old group to be reported, got:\n%s", report)
	}
	if !strings.Contains(report, "异常 1 条") {
		t.Fatalf("expected exactly one problem, got:\n%s", report)
	}
	if !strings.Contains(report, "存在异常项") {
		t.Fatalf("expected a failure conclusion, got:\n%s", report)
	}
}

// TestIdentityBindDoctorHandlesEmptyStore 没有任何绑定时不能报错。
func TestIdentityBindDoctorHandlesEmptyStore(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()

	ctx, _ := newQuitCommandTestContext(t, env.d, env.ctx.EndPoint, bindTestNewUserID, bindTestNewGroupID, "新群")
	report := identityBindDoctorReport(env.d, ctx)
	if !strings.Contains(report, "绑定记录数: 0") {
		t.Fatalf("expected an empty report, got:\n%s", report)
	}
	if !strings.Contains(report, "没有任何绑定记录") {
		t.Fatalf("expected an empty-store note, got:\n%s", report)
	}
}

// TestIdentityBindDoctorFlagsCorruptRecord 缺 ID 的坏记录必须被标出来，而不是静默忽略。
func TestIdentityBindDoctorFlagsCorruptRecord(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()

	// 手工塞一条只有官方侧 ID 的记录（模拟文件被改坏）
	broken := &identityBindRecord{
		Action: identityBindActionUser,
		New:    identityBindEndpoint{UserID: bindTestNewUserID},
	}
	if err := identityBindStoreOf(env.d).put(env.d, broken); err != nil {
		t.Fatalf("put broken record: %v", err)
	}

	ctx, _ := newQuitCommandTestContext(t, env.d, env.ctx.EndPoint, bindTestNewUserID, bindTestNewGroupID, "新群")
	report := identityBindDoctorReport(env.d, ctx)
	if !strings.Contains(report, "缺少旧侧 ID") {
		t.Fatalf("expected the corrupt record to be flagged, got:\n%s", report)
	}
}

// TestLogDedupWindowClamps 同群多 bot 的日志去重窗口必须可配置且被收敛到合法范围。
func TestLogDedupWindowClamps(t *testing.T) {
	cases := []struct {
		name string
		d    *Dice
		want int64
	}{
		{"nil dice falls back to default", nil, logDedupDefaultWindowSec},
		{"zero falls back to default", &Dice{}, logDedupDefaultWindowSec},
		{"explicit value is honoured", &Dice{Config: Config{BaseConfig: BaseConfig{LogMultiBotDedupWindowSec: 30}}}, 30},
		{"absurd value is clamped", &Dice{Config: Config{BaseConfig: BaseConfig{LogMultiBotDedupWindowSec: 999999}}}, logDedupWindowMaxSec},
	}
	for _, c := range cases {
		if got := logDedupWindowSec(c.d); got != c.want {
			t.Fatalf("%s: logDedupWindowSec = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestLogCrossBotDedupIsOptIn 关键的安全性断言：
// 默认配置下「跨连接去重」必须是关闭的，行为与上游完全一致，
// 否则同一个人在 5 秒内发的两条相同消息会被并成一条永久日志。
func TestLogCrossBotDedupIsOptIn(t *testing.T) {
	if DefaultConfig.LogMultiBotDedupWindowSec != logDedupDefaultWindowSec {
		t.Fatalf("default window = %d, want %d (= upstream behaviour)",
			DefaultConfig.LogMultiBotDedupWindowSec, logDedupDefaultWindowSec)
	}
	if logCrossBotDedupEnabled(nil) {
		t.Fatal("cross-bot dedup must be OFF when there is no config")
	}
	if logCrossBotDedupEnabled(&Dice{}) {
		t.Fatal("cross-bot dedup must be OFF with an empty config")
	}
	if logCrossBotDedupEnabled(&Dice{Config: Config{BaseConfig: BaseConfig{LogMultiBotDedupWindowSec: 5}}}) {
		t.Fatal("cross-bot dedup must be OFF at the default window")
	}
	// 显式调大后才启用
	if !logCrossBotDedupEnabled(&Dice{Config: Config{BaseConfig: BaseConfig{LogMultiBotDedupWindowSec: 30}}}) {
		t.Fatal("cross-bot dedup should turn ON once the window is raised")
	}
}

// TestIdentityBindSnWriteDoesNotTouchOtherSide
// 方案 A 的写入隔离：官方侧写自己的 .sn，不能改到旧群那一份。
func TestIdentityBindSnWriteDoesNotTouchOtherSide(t *testing.T) {
	env, officialCtx, oldCtx := logShareTestEnv(t)
	defer env.cleanup()

	// 旧群原本设有模板
	oldCtx.Player.AutoSetNameTemplate = "{$t玩家_RAW} HP{生命值}"

	// 官方侧改成别的（模拟 .sn coc）
	officialCtx.Player.AutoSetNameTemplate = "{$t玩家_RAW} SAN{理智}"

	// 旧群那份必须原封不动
	if got := strings.TrimSpace(oldCtx.Player.AutoSetNameTemplate); got != "{$t玩家_RAW} HP{生命值}" {
		t.Fatalf("old side template was modified by the official side: %q", got)
	}
	// 官方侧生效的是自己那份（自己设过就以自己为准）
	if got := identityBindPlayerNameTemplate(officialCtx); got != "{$t玩家_RAW} SAN{理智}" {
		t.Fatalf("official side effective template = %q", got)
	}
	// 旧群侧生效的还是旧群那份
	if got := identityBindPlayerNameTemplate(oldCtx); got != "{$t玩家_RAW} HP{生命值}" {
		t.Fatalf("old side effective template = %q", got)
	}
}

// TestIdentityBindLogOffTogglesSharedState 官方群执行 .log off，必须关掉归一后的那份状态。
//
// 这是实现上的一个坑：状态挂在 GroupInfo 上，如果只把「读状态」改成读归一后的群，
// 却仍然在真实群对象上 SetLogOn/ClearLogState，就会出现「关不掉」——
// 官方群 .log off 之后旧群那边日志还在记。
func TestIdentityBindLogOffTogglesSharedState(t *testing.T) {
	env, officialCtx, _ := logShareTestEnv(t)
	defer env.cleanup()

	oldGroup := groupForTest(t, env, bindTestOldGroupID)
	newGroup := groupForTest(t, env, bindTestNewGroupID)

	// 旧群上开着日志
	logID, err := service.LogGetOrCreate(env.d.DBOperator, bindTestOldGroupID, "跑团记录")
	if err != nil {
		t.Fatalf("LogGetOrCreate: %v", err)
	}
	oldGroup.SetLogState(logID, "跑团记录", true)

	// 取已注册的 log 扩展（Dice.Init 时已经注册过，不能再注册一次）
	ext := env.d.ExtFind("log", false)
	if ext == nil {
		t.Fatal("log extension is not registered")
	}
	logCmd, ok := ext.CmdMap["log"]
	if !ok || logCmd == nil {
		t.Fatal(".log command is not registered")
	}

	// 通过官方群的上下文执行 .log off。
	// 注意 RawArgs 必须一起给：ChopPrefixToArgsWith 会用它切片，缺了会 panic。
	logCmd.Solve(officialCtx,
		&Message{MessageType: "group", GroupID: bindTestNewGroupID},
		&CmdArgs{Args: []string{"off"}, RawArgs: "off"})

	// 归一后的那份状态必须被关掉
	if state := getGroupLogState(oldGroup); state.On {
		t.Fatalf("official .log off did not turn off the shared state: %+v", state)
	}
	// 官方群自己的对象本来就不持有状态，不该被写上
	if own := getGroupLogState(newGroup); own.On || own.Name != "" {
		t.Fatalf("official group object must stay stateless, got %+v", own)
	}
}

// TestDefaultConfigValuesLandOnTheRightFields 锁定 DefaultConfig 的默认值。
//
// DefaultConfig 是**位置字面量**（没写字段名），所以字段顺序和字面量顺序必须严格对应。
// 一旦有人往 BaseConfig 中间插字段、或调整了字面量顺序，而错位后的类型恰好相同，
// Go 不会报错，只会把默认值静默赋给错误的字段——这类 bug 极难发现。
// 这个测试把 fork 新增字段和几个相邻的上游字段一起钉住，顺序一动就会红。
//
//nolint:staticcheck // QF1008：这里刻意写全嵌入字段名，就是为了直接验证「哪一段」有没有串位
func TestDefaultConfigValuesLandOnTheRightFields(t *testing.T) {
	c := DefaultConfig

	// fork 新增字段
	if c.OfficialQQRequestTimeoutSec != 60 {
		t.Fatalf("OfficialQQRequestTimeoutSec = %d, want 60", c.OfficialQQRequestTimeoutSec)
	}
	if c.OfficialQQChunkedUploadEnable {
		t.Fatal("OfficialQQChunkedUploadEnable should default to false")
	}
	if c.IdentityBindEnable {
		t.Fatal("IdentityBindEnable should default to false")
	}
	if c.IdentityBindCooldownSec != 60 {
		t.Fatalf("IdentityBindCooldownSec = %d, want 60", c.IdentityBindCooldownSec)
	}
	if c.LogMultiBotDedupWindowSec != logDedupDefaultWindowSec {
		t.Fatalf("LogMultiBotDedupWindowSec = %d, want %d (upstream behaviour)",
			c.LogMultiBotDedupWindowSec, logDedupDefaultWindowSec)
	}

	// 字面量里紧邻 fork 字段的上游字段，用来确认没有整体错位
	if !c.PlayerNameWrapEnable {
		t.Fatal("PlayerNameWrapEnable = false, want true (literal likely shifted)")
	}
	if !c.TextCmdTrustOnly {
		t.Fatal("TextCmdTrustOnly = false, want true (literal likely shifted)")
	}
	if c.DiceRandomMode != string(DiceRandomModePCG) {
		t.Fatalf("DiceRandomMode = %q, want %q", c.DiceRandomMode, DiceRandomModePCG)
	}
	if c.VMVersionForReply != "v1" || c.VMVersionForDeck != "v2" {
		t.Fatalf("VM versions shifted: reply=%q deck=%q", c.VMVersionForReply, c.VMVersionForDeck)
	}
	if c.BaseConfig.Name != "default" || c.BaseConfig.DataDir != "data/default" {
		t.Fatalf("Name/DataDir shifted: %q / %q", c.BaseConfig.Name, c.BaseConfig.DataDir)
	}
	if c.OfficialQQMigrationEnable {
		t.Fatal("OfficialQQMigrationEnable should default to false")
	}
	// 位置字面量最容易被忽略的相邻嵌入：确认后面几个 config 段没有整体串位
	if c.RateLimitConfig.PersonalReplenishRateStr != "@every 3s" {
		t.Fatalf("RateLimitConfig shifted: %q", c.RateLimitConfig.PersonalReplenishRateStr)
	}
	if c.PersonalBurst != 3 || c.GroupBurst != 3 {
		t.Fatalf("bursts shifted: personal=%d group=%d", c.PersonalBurst, c.GroupBurst)
	}
	if c.StoryLogConfig.LogSizeNoticeCount != 500 {
		t.Fatalf("StoryLogConfig shifted: %d", c.StoryLogConfig.LogSizeNoticeCount)
	}
	if len(c.DirtyConfig.DiceMasters) != 1 || c.DirtyConfig.DiceMasters[0] != "UI:1001" {
		t.Fatalf("DirtyConfig shifted: %+v", c.DirtyConfig.DiceMasters)
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
	cfg.IdentityBindCooldownSec = -5
	cfg.FixIdentityBindConfig()
	if cfg.IdentityBindCooldownSec != 0 {
		t.Fatalf("expected negative cooldown to clamp to 0, got %d", cfg.IdentityBindCooldownSec)
	}

	cfg.IdentityBindCooldownSec = 99999999
	cfg.IdentityBindCodeLength = 999
	cfg.IdentityBindCodeExpireSec = 999999
	cfg.FixIdentityBindConfig()
	if cfg.IdentityBindCooldownSec != identityBindMaxCooldownSec {
		t.Fatalf("expected cooldown clamp to %d, got %d",
			identityBindMaxCooldownSec, cfg.IdentityBindCooldownSec)
	}
	// 验证码相关字段也要被收敛
	if cfg.IdentityBindCodeLength != identityBindCodeMaxLen {
		t.Fatalf("expected code length clamp to %d, got %d",
			identityBindCodeMaxLen, cfg.IdentityBindCodeLength)
	}
	if cfg.IdentityBindCodeExpireSec != 3600 {
		t.Fatalf("expected code expiry clamp to 3600, got %d", cfg.IdentityBindCodeExpireSec)
	}
}

// ---------- 存储测试 ----------

func TestIdentityBindStorePersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	d := &Dice{BaseConfig: BaseConfig{DataDir: dir}}
	d.IdentityBindStore = &identityBindStore{}

	record := &identityBindRecord{
		Action:  identityBindActionUser,
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
	got, ok := identityBindStoreOf(fresh).find(fresh, bindTestNewUserID)
	if !ok {
		t.Fatal("expected binding to be reloaded from disk")
	}
	if got.Old.UserID != bindTestOldUserID || got.Old.GroupID != bindTestOldGroupID {
		t.Fatalf("reloaded record = %+v, want old %s / %s", got.Old, bindTestOldGroupID, bindTestOldUserID)
	}

	// 删除后不再可见
	toDelete, found := identityBindStoreOf(fresh).find(fresh, bindTestNewUserID)
	if !found {
		t.Fatal("expected to find the record before deleting")
	}
	deleted, err := identityBindStoreOf(fresh).delete(fresh, toDelete)
	if err != nil || !deleted {
		t.Fatalf("delete = (%v, %v), want (true, nil)", deleted, err)
	}
	reloaded := &Dice{BaseConfig: BaseConfig{DataDir: dir}}
	reloaded.IdentityBindStore = &identityBindStore{}
	if _, ok := identityBindStoreOf(reloaded).find(reloaded, bindTestNewUserID); ok {
		t.Fatal("expected binding to stay deleted after reload")
	}
	// 另一侧的索引也必须一起清掉，否则民间 bot 侧会指向一条已经不存在的绑定
	if _, ok := identityBindStoreOf(reloaded).find(reloaded, bindTestOldUserID); ok {
		t.Fatal("expected the reverse index entry to be removed as well")
	}
}

// TestIdentityBindSymmetricResolve 双向共享的核心断言：
// 同一对绑定，两侧解析出来的数据 key 必须完全一致，且规范 key 固定是旧身份。
func TestIdentityBindSymmetricResolve(t *testing.T) {
	dir := t.TempDir()
	d := &Dice{
		BaseConfig:        BaseConfig{DataDir: dir},
		IdentityBindStore: &identityBindStore{},
	}
	record := &identityBindRecord{
		Action:  identityBindActionUser,
		New:     identityBindEndpoint{GroupID: bindTestNewGroupID, UserID: bindTestNewUserID},
		Old:     identityBindEndpoint{GroupID: bindTestOldGroupID, UserID: bindTestOldUserID},
		Created: 123,
	}
	if err := identityBindStoreOf(d).put(d, record); err != nil {
		t.Fatalf("put: %v", err)
	}

	// 官方身份 → 旧身份
	if got := identityCanonicalUserID(d, bindTestNewUserID); got != bindTestOldUserID {
		t.Fatalf("canonical(official) = %q, want %q", got, bindTestOldUserID)
	}
	// 旧身份 → 还是旧身份（原地不动，旧账号的历史数据不需要搬家）
	if got := identityCanonicalUserID(d, bindTestOldUserID); got != bindTestOldUserID {
		t.Fatalf("canonical(old) = %q, want %q (must stay put)", got, bindTestOldUserID)
	}
	// 未绑定的 ID 原样返回
	if got := identityCanonicalUserID(d, "QQ:999999"); got != "QQ:999999" {
		t.Fatalf("canonical(unbound) = %q, want unchanged", got)
	}
	// 两侧必须收敛到同一个 key，否则数据还是两份
	if identityCanonicalUserID(d, bindTestNewUserID) != identityCanonicalUserID(d, bindTestOldUserID) {
		t.Fatal("both sides must converge to the same data key")
	}

	// 群也一样
	groupRecord := &identityBindRecord{
		Action:  identityBindActionGroup,
		New:     identityBindEndpoint{GroupID: bindTestNewGroupID},
		Old:     identityBindEndpoint{GroupID: bindTestOldGroupID},
		Created: 124,
	}
	if err := identityBindStoreOf(d).put(d, groupRecord); err != nil {
		t.Fatalf("put group: %v", err)
	}
	if got := identityCanonicalGroupID(d, bindTestNewGroupID); got != bindTestOldGroupID {
		t.Fatalf("canonical group(official) = %q, want %q", got, bindTestOldGroupID)
	}
	if got := identityCanonicalGroupID(d, bindTestOldGroupID); got != bindTestOldGroupID {
		t.Fatalf("canonical group(old) = %q, want %q (must stay put)", got, bindTestOldGroupID)
	}
}

// TestIdentityBindResolveWorksFromOldEndpoint 旧号侧的端点也必须能解析出绑定。
// 这条曾经是反的（只有官方端点才解析），导致双向共享不成立。
func TestIdentityBindResolveWorksFromOldEndpoint(t *testing.T) {
	dir := t.TempDir()
	d := &Dice{
		BaseConfig:        BaseConfig{DataDir: dir},
		IdentityBindStore: &identityBindStore{},
	}
	record := &identityBindRecord{
		Action:  identityBindActionUser,
		New:     identityBindEndpoint{GroupID: bindTestNewGroupID, UserID: bindTestNewUserID},
		Old:     identityBindEndpoint{GroupID: bindTestOldGroupID, UserID: bindTestOldUserID},
		Created: 123,
	}
	if err := identityBindStoreOf(d).put(d, record); err != nil {
		t.Fatalf("put: %v", err)
	}

	// 官方端点：归一成旧身份
	officialCtx := &MsgContext{
		Dice:     d,
		Group:    &GroupInfo{GroupID: bindTestNewGroupID},
		Player:   &GroupPlayerInfo{UserID: bindTestNewUserID},
		EndPoint: &EndPointInfo{EndPointInfoBase: EndPointInfoBase{Platform: "QQ", ProtocolType: "official"}},
	}
	if got := identityBindDataUserID(officialCtx); got != bindTestOldUserID {
		t.Fatalf("official data user id = %q, want %q", got, bindTestOldUserID)
	}

	// 旧端点：同样归一成旧身份，而且必须是同一个值（这就是「数据共通」）
	oldCtx := &MsgContext{
		Dice:     d,
		Group:    &GroupInfo{GroupID: bindTestOldGroupID},
		Player:   &GroupPlayerInfo{UserID: bindTestOldUserID},
		EndPoint: &EndPointInfo{EndPointInfoBase: EndPointInfoBase{Platform: "QQ", ProtocolType: "onebot"}},
	}
	if got := identityBindDataUserID(oldCtx); got != bindTestOldUserID {
		t.Fatalf("old data user id = %q, want %q", got, bindTestOldUserID)
	}
	if identityBindDataUserID(oldCtx) != identityBindDataUserID(officialCtx) {
		t.Fatal("both endpoints must resolve to the same data user id")
	}
}

// TestIdentityBindIsOfficialQQID 判方向只看 ID 前缀，不看端点。
func TestIdentityBindIsOfficialQQID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"OpenQQ:100-member", true},
		{"OpenQQ-Group:100-group", true},
		{"QQ:123456", false},
		{"QQ-Group:789", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isOfficialQQID(c.id); got != c.want {
			t.Fatalf("isOfficialQQID(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}

// TestIdentityBindUniquenessBlocksTakeover 同一个旧账号不能被第二个人绑定。
//
// 引入双向索引之后，旧号侧也指向官方 ID，所以「谁先绑谁得」必须显式拦住，
// 否则后来者一绑就把前面那个人的数据（属性、角色卡、日志）全部接管过去。
func TestIdentityBindUniquenessBlocksTakeover(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	env.addOldCard(t, "调查员甲")

	// 第一个人正常绑定成功（走验证码）
	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001", "1001"}})
	prompt := waitGroupMessage(t, env)
	if !strings.Contains(prompt, "验证码") {
		t.Fatalf("expected a code-flow prompt, got %q", prompt)
	}
	challenge, ok := identityBindLoadCode(identityBindActionUser, bindTestNewUserID)
	if !ok || challenge.Status != identityBindCodeDelivered {
		t.Fatalf("expected a delivered challenge for the first user, got %+v / %v", challenge, ok)
	}
	// 第一人通过民间 bot 私聊确认
	oldEP := codeOldEndPoint(t, env)
	firstCtx, firstMsg := newQuitCommandTestContext(t, env.d, oldEP, bindTestOldUserID, bindTestOldGroupID, "旧群")
	firstCtx.IsPrivate = true
	firstMsg.MessageType = "private"
	if !identityBindTryConsumeCode(firstCtx, firstMsg, challenge.Code) {
		t.Fatal("the first user's code reply should be consumed")
	}
	if _, found := identityBindStoreOf(env.d).find(env.d, bindTestOldUserID); !found {
		t.Fatal("expected the first bind to have created a record")
	}

	// 第二个人用不同的官方身份，试图绑定同一个旧号
	secondUserID := "OpenQQ:100-second-member"
	secondCtx, secondMsg := newQuitCommandTestContext(t, env.d, env.ctx.EndPoint, secondUserID, bindTestNewGroupID, "新群")
	before := env.recorder.messageCount()
	runIdentityBindCommand(secondCtx, secondMsg, &CmdArgs{Args: []string{"2001", "1001"}})
	reply := env.recorder.waitNextReply(t, before)
	if !strings.Contains(reply, "已经被绑定") {
		t.Fatalf("expected the second bind to be rejected as already claimed, got %q", reply)
	}

	// 关键：第一个人的绑定必须原封不动，没有被顶掉或改写
	record, ok := identityBindStoreOf(env.d).find(env.d, bindTestOldUserID)
	if !ok {
		t.Fatal("expected the original binding to still exist")
	}
	if record.New.UserID != bindTestNewUserID {
		t.Fatalf("binding owner changed to %s, want %s", record.New.UserID, bindTestNewUserID)
	}
	// 第二个人不应该拿到可用的挑战。
	// 注意：已完成（used）的挑战会保留一段时间供排查，所以"存在"不等于"可用"——
	// 这里检查的是归属：不能出现一条以**第二个人**为发起者的挑战。
	for _, owner := range []string{secondUserID, bindTestNewUserID} {
		if c, ok := identityBindLoadCode(identityBindActionUser, owner); ok {
			if c.Status == identityBindCodePending || c.Status == identityBindCodeDelivered {
				t.Fatalf("rejected user must not hold an active challenge, got owner=%q status=%q", owner, c.Status)
			}
		}
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
	if _, ok := identityBindStoreOf(env.d).find(env.d, bindTestNewUserID); ok {
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
	if reply := waitGroupMessage(t, env); !strings.Contains(reply, ".group bind <旧群号>") {
		t.Fatalf("expected a usage reply, got %q", reply)
	}
}

// ---------- .group 与 .bind 必须能同时使用 ----------

func TestIdentityBindGroupCommandAndUserBindAreIndependent(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	env.addOldCard(t, "调查员甲")
	env.addOldLog(t, "第一话")
	env.ctx.PrivilegeLevel = 100

	// 1) 先做个人身份绑定（验证码，私聊给旧号）
	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001", "1001"}})
	if reply := waitGroupMessage(t, env); !strings.Contains(reply, "验证码") {
		t.Fatalf("expected a user-bind code prompt, got %q", reply)
	}
	userChallenge, ok := identityBindLoadCode(identityBindActionUser, bindTestNewUserID)
	if !ok || userChallenge.Status != identityBindCodeDelivered {
		t.Fatalf("expected a delivered user challenge, got %+v / %v", userChallenge, ok)
	}
	oldEP := codeOldEndPoint(t, env)
	userCtx, userMsg := newQuitCommandTestContext(t, env.d, oldEP, bindTestOldUserID, bindTestOldGroupID, "旧群")
	userCtx.IsPrivate = true
	userMsg.MessageType = "private"
	if !identityBindTryConsumeCode(userCtx, userMsg, userChallenge.Code) {
		t.Fatal("the user code reply should be consumed")
	}
	if _, found := identityBindStoreOf(env.d).find(env.d, bindTestOldUserID); !found {
		t.Fatal("expected the user bind to have created a record")
	}

	// 2) 个人绑定已存在时，群绑定必须仍然可用（这是之前会互相阻断的地方）
	if _, exists := identityBindLoadCode(identityBindActionGroup, bindTestNewGroupID); exists {
		t.Fatal("group challenge should not exist yet")
	}
	oldGroup := groupForTest(t, env, bindTestOldGroupID)
	oldGroup.InviteUserID = bindTestOldUserID

	before := env.recorder.messageCount()
	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"bind", "1001"}})
	groupPrompt := env.recorder.waitNextReply(t, before)
	if !strings.Contains(groupPrompt, "验证码") {
		t.Fatalf("expected a group-bind code prompt even though the user is already bound, got %q", groupPrompt)
	}
	groupChallenge, ok := identityBindLoadCode(identityBindActionGroup, bindTestNewGroupID)
	if !ok || groupChallenge.Status != identityBindCodeDelivered {
		t.Fatalf("expected a delivered group challenge, got %+v / %v", groupChallenge, ok)
	}
	// 群绑定的确认人是旧群邀请人
	if groupChallenge.ConfirmBy != bindTestOldUserID {
		t.Fatalf("group challenge confirmer = %q, want the inviter %q", groupChallenge.ConfirmBy, bindTestOldUserID)
	}
	// 邀请人私聊确认
	invCtx, invMsg := newQuitCommandTestContext(t, env.d, oldEP, bindTestOldUserID, bindTestOldGroupID, "旧群")
	invCtx.IsPrivate = true
	invMsg.MessageType = "private"
	if !identityBindTryConsumeCode(invCtx, invMsg, groupChallenge.Code) {
		t.Fatal("the inviter's code reply should be consumed")
	}

	// 3) 两条绑定必须同时存在，且键互不覆盖
	store := identityBindStoreOf(env.d)
	userRecord, ok := store.find(env.d, bindTestNewUserID)
	if !ok {
		t.Fatal("user binding disappeared after group bind")
	}
	if userRecord.Action != identityBindActionUser || userRecord.Old.UserID != bindTestOldUserID {
		t.Fatalf("unexpected user binding record: %+v", userRecord)
	}
	groupRecord, ok := store.find(env.d, bindTestNewGroupID)
	if !ok {
		t.Fatal("group binding was not stored")
	}
	if groupRecord.Action != identityBindActionGroup || groupRecord.Old.GroupID != bindTestOldGroupID {
		t.Fatalf("unexpected group binding record: %+v", groupRecord)
	}
	if userRecord.Key == groupRecord.Key {
		t.Fatalf("user and group bindings must not share a store key: %q", userRecord.Key)
	}
}

func TestIdentityBindGroupUnbindKeepsUserBind(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()
	store := identityBindStoreOf(env.d)

	if err := store.put(env.d, &identityBindRecord{
		Action: identityBindActionUser,
		New:    identityBindEndpoint{GroupID: bindTestNewGroupID, UserID: bindTestNewUserID},
		Old:    identityBindEndpoint{GroupID: bindTestOldGroupID, UserID: bindTestOldUserID},
	}); err != nil {
		t.Fatalf("seed user bind: %v", err)
	}
	if err := store.put(env.d, &identityBindRecord{
		Action: identityBindActionGroup,
		New:    identityBindEndpoint{GroupID: bindTestNewGroupID, UserID: bindTestNewUserID},
		Old:    identityBindEndpoint{GroupID: bindTestOldGroupID},
	}); err != nil {
		t.Fatalf("seed group bind: %v", err)
	}

	// .group unbind 只应该删掉群绑定
	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"unbind"}})
	if reply := waitGroupMessage(t, env); !strings.Contains(reply, "已解除") {
		t.Fatalf("expected an unbind reply, got %q", reply)
	}
	if _, ok := store.find(env.d, bindTestNewGroupID); ok {
		t.Fatal("group binding should be gone")
	}
	if _, ok := store.find(env.d, bindTestNewUserID); !ok {
		t.Fatal("user binding must survive .group unbind")
	}
}

func TestIdentityBindGroupStatusShowsBinding(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()
	if err := identityBindStoreOf(env.d).put(env.d, &identityBindRecord{
		Action: identityBindActionGroup,
		New:    identityBindEndpoint{GroupID: bindTestNewGroupID, UserID: bindTestNewUserID},
		Old:    identityBindEndpoint{GroupID: bindTestOldGroupID, GroupName: "旧群"},
	}); err != nil {
		t.Fatalf("seed group bind: %v", err)
	}

	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"status"}})
	reply := waitGroupMessage(t, env)
	if !strings.Contains(reply, bindTestOldGroupID) || !strings.Contains(reply, "已绑定旧群") {
		t.Fatalf("expected the group binding status, got %q", reply)
	}
}

func TestIdentityBindLogAliasStillWorks(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	env.ctx.PrivilegeLevel = 100
	oldGroup := groupForTest(t, env, bindTestOldGroupID)
	oldGroup.InviteUserID = bindTestOldUserID

	// 旧的 .log bind 写法必须继续可用
	result, handled := runIdentityBindLogCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"bind", "1001"}})
	if !handled || !result.Solved {
		t.Fatalf("expected .log bind to keep working, got (%+v, %v)", result, handled)
	}
	if reply := waitGroupMessage(t, env); !strings.Contains(reply, "验证码") {
		t.Fatalf("expected a code prompt from the .log alias, got %q", reply)
	}
}

func TestIdentityBindGroupHelpMentionsNewCommand(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()

	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"help"}})
	reply := waitGroupMessage(t, env)
	if !strings.Contains(reply, ".group bind") || !strings.Contains(reply, ".group unbind") {
		t.Fatalf("expected help to document .group commands, got %q", reply)
	}
}

// ---------- 旧群号是可选的：个人绑定与群无关 ----------

func TestIdentityBindUserBindWithoutOldGroup(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	env.addOldCard(t, "调查员甲")

	// 只给旧 QQ 号，不给旧群号：验证码通道照样要能用
	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001"}})
	prompt := waitGroupMessage(t, env)
	if !strings.Contains(prompt, "验证码") {
		t.Fatalf("expected a code prompt without an old group id, got %q", prompt)
	}
	if !strings.Contains(prompt, bindTestOldUserID) {
		t.Fatalf("expected the prompt to mention the old qq id, got %q", prompt)
	}

	challenge, ok := identityBindLoadCode(identityBindActionUser, bindTestNewUserID)
	if !ok || challenge.Status != identityBindCodeDelivered {
		t.Fatalf("expected a delivered challenge, got %+v / %v", challenge, ok)
	}
	oldEP := codeOldEndPoint(t, env)
	oldCtx, oldMsg := newQuitCommandTestContext(t, env.d, oldEP, bindTestOldUserID, bindTestOldGroupID, "旧群")
	oldCtx.IsPrivate = true
	oldMsg.MessageType = "private"
	if !identityBindTryConsumeCode(oldCtx, oldMsg, challenge.Code) {
		t.Fatal("the code reply should be consumed")
	}

	record, ok := identityBindStoreOf(env.d).find(env.d, bindTestNewUserID)
	if !ok {
		t.Fatal("expected the binding to be stored")
	}
	if record.Old.UserID != bindTestOldUserID {
		t.Fatalf("stored old user = %q, want %q", record.Old.UserID, bindTestOldUserID)
	}
	// 没有指定旧群时不应该凭空写入旧群
	if record.Old.GroupID != "" {
		t.Fatalf("expected no old group id when it was not provided, got %q", record.Old.GroupID)
	}
}

func TestIdentityBindNoArgsShowsHelpAndStatus(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()

	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{}})
	reply := waitGroupMessage(t, env)
	if !strings.Contains(reply, ".bind") || !strings.Contains(reply, "尚未绑定旧QQ号") {
		t.Fatalf("expected help plus the current status, got %q", reply)
	}
	if !strings.Contains(reply, "全局") {
		t.Fatalf("expected the status to explain the binding is global, got %q", reply)
	}
}

// ---------- 取消进行中的绑定验证 ----------

func TestIdentityBindCancelStopsUserSession(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	env.addOldCard(t, "调查员甲")

	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001"}})
	if reply := waitGroupMessage(t, env); !strings.Contains(reply, "验证码") {
		t.Fatalf("expected a code prompt, got %q", reply)
	}
	if _, ok := identityBindLoadCode(identityBindActionUser, bindTestNewUserID); !ok {
		t.Fatal("expected a pending code challenge")
	}

	before := env.recorder.messageCount()
	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"cancel"}})
	if reply := env.recorder.waitNextReply(t, before); !strings.Contains(reply, "已取消") {
		t.Fatalf("expected a cancel confirmation, got %q", reply)
	}
	if _, ok := identityBindLoadCode(identityBindActionUser, bindTestNewUserID); ok {
		t.Fatal("expected the challenge to be gone after cancel")
	}

	// 取消之后可以重新发起
	before = env.recorder.messageCount()
	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001"}})
	if reply := env.recorder.waitNextReply(t, before); !strings.Contains(reply, "验证码") {
		t.Fatalf("expected to be able to start over after cancel, got %q", reply)
	}
}

func TestIdentityBindGroupCancelClearsUserSessionToo(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	env.addOldCard(t, "调查员甲")

	// 先用 .bind 挂起一条个人验证码挑战
	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"2001"}})
	if reply := waitGroupMessage(t, env); !strings.Contains(reply, "验证码") {
		t.Fatalf("expected a code prompt, got %q", reply)
	}
	if _, ok := identityBindLoadCode(identityBindActionUser, bindTestNewUserID); !ok {
		t.Fatal("expected a pending user challenge")
	}

	// .group cancel 应该把个人挑战也清掉，避免卡住无法重新绑定
	before := env.recorder.messageCount()
	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"cancel"}})
	if reply := env.recorder.waitNextReply(t, before); !strings.Contains(reply, "已取消") {
		t.Fatalf("expected a cancel confirmation, got %q", reply)
	}
	if _, ok := identityBindLoadCode(identityBindActionUser, bindTestNewUserID); ok {
		t.Fatal(".group cancel should also clear the user challenge")
	}
}

func TestIdentityBindCancelWithoutSessionIsHarmless(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()

	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"cancel"}})
	if reply := waitGroupMessage(t, env); !strings.Contains(reply, "没有进行中的绑定") {
		t.Fatalf("expected a friendly no-session reply, got %q", reply)
	}
}

func TestIdentityBindFormatDurationReadable(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{12 * time.Hour, "12 小时"},
		{90 * time.Minute, "1 小时 30 分"},
		{45 * time.Minute, "45 分"},
		{30 * time.Second, "30 秒"},
	}
	for _, tc := range cases {
		if got := identityBindFormatDuration(tc.in); got != tc.want {
			t.Fatalf("formatDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------- 两种绑定的状态必须分开显示 ----------

func TestIdentityBindStatusesAreSeparated(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()
	store := identityBindStoreOf(env.d)

	if err := store.put(env.d, &identityBindRecord{
		Action: identityBindActionUser,
		New:    identityBindEndpoint{GroupID: bindTestNewGroupID, UserID: bindTestNewUserID},
		Old:    identityBindEndpoint{GroupID: bindTestOldGroupID, UserID: bindTestOldUserID},
	}); err != nil {
		t.Fatalf("seed user bind: %v", err)
	}

	// 只有个人绑定时，群状态必须明确说「尚未绑定旧群」
	runIdentityBindGroupCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"status"}})
	groupStatus := waitGroupMessage(t, env)
	if !strings.Contains(groupStatus, "尚未绑定旧群") {
		t.Fatalf("group status must not report the user binding as a group binding, got %q", groupStatus)
	}
	if strings.Contains(groupStatus, "已绑定旧群") {
		t.Fatalf("group status wrongly claims a group binding: %q", groupStatus)
	}
	if !strings.Contains(groupStatus, ".bind status") {
		t.Fatalf("group status should point at .bind status for the personal binding, got %q", groupStatus)
	}

	// 个人状态必须显示旧 QQ 号
	before := env.recorder.messageCount()
	runIdentityBindCommand(env.ctx, env.msg, &CmdArgs{Args: []string{"status"}})
	userStatus := env.recorder.waitNextReply(t, before)
	if !strings.Contains(userStatus, bindTestOldUserID) {
		t.Fatalf("user status should show the bound old qq id, got %q", userStatus)
	}
	if !strings.Contains(userStatus, "全局") {
		t.Fatalf("user status should explain the binding is global, got %q", userStatus)
	}
}

// waitGroupMessage 等待骰娘发出回复并返回其内容。
func waitGroupMessage(t *testing.T, env *bindTestEnv) string {
	t.Helper()
	if env == nil || env.recorder == nil {
		t.Fatal("test env has no recorder")
	}
	return env.recorder.waitReply(t)
}
