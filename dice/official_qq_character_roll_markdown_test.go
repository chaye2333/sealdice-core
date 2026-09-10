//nolint:testpackage
package dice

import (
	"strings"
	"testing"
	"time"

	ds "github.com/sealdice/dicescript"

	"sealdice-core/model"
	"sealdice-core/utils/constant"
)

// newOfficialQQBarTestCtx 构造一个可读取角色卡的官方 QQ 上下文。
func newOfficialQQBarTestCtx(t *testing.T, sheetType string, values map[string]*ds.VMValue) (*MsgContext, func()) {
	t.Helper()

	d, ep, _, cleanup := newExecuteNewTestDice(t)
	ep.Platform = "QQ"
	ep.ProtocolType = "official"

	// 属性查询需要 attrs 表存在，否则骰点引擎内部的断言会 panic。
	if operator, ok := d.DBOperator.(*mockDatabaseOperator); ok {
		if err := operator.GetDataDB(constant.WRITE).AutoMigrate(&model.AttributesItemModel{}); err != nil {
			cleanup()
			t.Fatalf("AutoMigrate attrs: %v", err)
		}
	}

	group := &GroupInfo{
		Active:          true,
		GroupID:         "OpenQQ-Group:100-group",
		GroupName:       "官方群",
		System:          sheetType,
		DiceIDActiveMap: new(SyncMap[string, bool]),
		DiceIDExistsMap: new(SyncMap[string, bool]),
		BotList:         new(SyncMap[string, bool]),
		Players:         new(SyncMap[string, *GroupPlayerInfo]),
		PlayerGroups:    new(SyncMap[string, []string]),
	}
	group.DiceIDActiveMap.Store(ep.UserID, true)
	group.DiceIDExistsMap.Store(ep.UserID, true)

	player := &GroupPlayerInfo{
		Name:         "调查员甲",
		UserID:       "OpenQQ:100-member",
		ValueMapTemp: &ds.ValueMap{},
	}
	group.Players.Store(player.UserID, player)
	d.ImSession.ServiceAtNew.Store(group.GroupID, group)

	ctx := &MsgContext{
		MessageType:     "group",
		Group:           group,
		Player:          player,
		EndPoint:        ep,
		Session:         d.ImSession,
		Dice:            d,
		IsCurGroupBotOn: true,
		PrivilegeLevel:  100,
	}
	ctx.SystemTemplate = group.GetCharTemplate(d)

	attrs := &AttributesItem{
		ID:        group.GroupID + "-" + player.UserID,
		valueMap:  &ds.ValueMap{},
		SheetType: sheetType,
		Name:      "调查员甲",
	}
	for key, value := range values {
		attrs.Store(key, value)
	}
	d.AttrsManager.m.Store(attrs.ID, attrs)

	return ctx, cleanup
}

func intVal(v int64) *ds.VMValue { return ds.NewIntVal(ds.IntType(v)) }

// ---------- 状态栏基础行为 ----------

func TestOfficialQQCharacterStatusBarRendersAttributes(t *testing.T) {
	ctx, cleanup := newOfficialQQBarTestCtx(t, "coc7", map[string]*ds.VMValue{
		"血条":  intVal(12),
		"血上限": intVal(12),
	})
	defer cleanup()
	ctx.Player.AutoSetNameTemplate = "{$t玩家_RAW} HP{血条}/{血上限}"

	bar := officialQQCharacterStatusBar(ctx)
	if bar == "" {
		t.Fatal("expected a status bar")
	}
	if !strings.Contains(bar, `\textcolor{#E5484D}`) {
		t.Fatalf("expected small red text color, got %q", bar)
	}
	if !strings.Contains(bar, `\scriptsize`) {
		t.Fatalf("expected small font size, got %q", bar)
	}
	if !strings.Contains(bar, "HP12/12") {
		t.Fatalf("expected evaluated attribute line HP12/12, got %q", bar)
	}
	if !strings.Contains(bar, "调查员甲") {
		t.Fatalf("expected character name, got %q", bar)
	}
	// 角色名与属性都必须在同一个数学片段内，这样才会整体渲染成小号红字
	if strings.Contains(bar, "\n") {
		t.Fatalf("expected a single-line status bar, got %q", bar)
	}
	if !strings.HasPrefix(bar, "$\\scriptsize\\textcolor{#E5484D}{\\text{") || !strings.HasSuffix(bar, "}}$") {
		t.Fatalf("expected everything inside one small-red math segment, got %q", bar)
	}
	// 片段内不能有落在 \text{...} 之外的散落文本
	inner := bar[strings.LastIndex(bar, `\text{`)+len(`\text{`) : strings.LastIndex(bar, "}}")]
	if !strings.Contains(inner, "调查员甲") {
		t.Fatalf("expected the character name inside the styled segment, got %q", bar)
	}
	if !strings.Contains(inner, "HP12/12") {
		t.Fatalf("expected the attributes inside the styled segment, got %q", bar)
	}
	// 角色名只出现一次
	if strings.Count(bar, "调查员甲") != 1 {
		t.Fatalf("expected the name exactly once, got %q", bar)
	}
}

func TestOfficialQQCharacterStatusBarTracksAttributeChanges(t *testing.T) {
	// 用自定义字段名，避免与 coc7 模板的内置别名（如 hp/HP 的显示映射）相互干扰
	ctx, cleanup := newOfficialQQBarTestCtx(t, "coc7", map[string]*ds.VMValue{
		"血条":  intVal(12),
		"血上限": intVal(12),
	})
	defer cleanup()
	ctx.Player.AutoSetNameTemplate = "{$t玩家_RAW} HP{血条}/{血上限}"

	first := officialQQCharacterStatusBar(ctx)
	if !strings.Contains(first, "HP12/12") {
		t.Fatalf("expected HP12/12, got %q", first)
	}

	// 改变属性后，下一次回复必须自动使用新值
	attrs, err := ctx.Dice.AttrsManager.LoadByCtx(ctx)
	if err != nil || attrs == nil {
		t.Fatalf("load attrs: %v", err)
	}
	attrs.Store("血条", intVal(5))

	second := officialQQCharacterStatusBar(ctx)
	if !strings.Contains(second, "HP5/12") {
		t.Fatalf("expected HP5/12 after the change, got %q", second)
	}
	if strings.Contains(second, "HP12/12") {
		t.Fatalf("stale attribute value leaked into %q", second)
	}
}

func TestOfficialQQCharacterStatusBarHiddenForNonOfficialPlatforms(t *testing.T) {
	for _, tc := range []struct {
		name     string
		platform string
		protocol string
	}{
		{"onebot", "QQ", "onebot"},
		{"telegram", "TG", ""},
		{"empty protocol", "QQ", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cleanup := newOfficialQQBarTestCtx(t, "coc7", map[string]*ds.VMValue{
				"血条": intVal(12),
			})
			defer cleanup()
			ctx.EndPoint.Platform = tc.platform
			ctx.EndPoint.ProtocolType = tc.protocol
			ctx.Player.AutoSetNameTemplate = "{$t玩家_RAW} HP{血条}"

			if bar := officialQQCharacterStatusBar(ctx); bar != "" {
				t.Fatalf("expected no status bar for %s/%s, got %q", tc.platform, tc.protocol, bar)
			}
			text := withOfficialQQCharacterStatusBar(ctx, "1d20=7")
			if text != "1d20=7" {
				t.Fatalf("expected reply to be untouched, got %q", text)
			}
		})
	}
}

func TestOfficialQQCharacterStatusBarHiddenForNameOnlyTemplate(t *testing.T) {
	ctx, cleanup := newOfficialQQBarTestCtx(t, "coc7", map[string]*ds.VMValue{"血条": intVal(12)})
	defer cleanup()

	// .sn none / .sn off 会清空模板
	ctx.Player.AutoSetNameTemplate = ""
	if bar := officialQQCharacterStatusBar(ctx); bar != "" {
		t.Fatalf("expected no status bar when .sn is off, got %q", bar)
	}

	// 只设了名字，没有属性：只有角色名，但仍然是小号红字
	ctx.Player.AutoSetNameTemplate = "{$t玩家_RAW}"
	bar := officialQQCharacterStatusBar(ctx)
	if !strings.Contains(bar, "调查员甲") {
		t.Fatalf("expected the character name, got %q", bar)
	}
	if !strings.Contains(bar, `\scriptsize`) || !strings.Contains(bar, `\textcolor{#E5484D}`) {
		t.Fatalf("expected the name to be styled small and red, got %q", bar)
	}
	inner := bar[strings.LastIndex(bar, `\text{`)+len(`\text{`) : strings.LastIndex(bar, "}}")]
	if inner != "调查员甲" {
		t.Fatalf("expected only the character name in the segment, got %q", inner)
	}
}

func TestOfficialQQCharacterStatusBarWorksWithUnknownTemplateFormat(t *testing.T) {
	// 不使用任何内置规则字段，验证实现不依赖硬编码的 coc/dnd 字段表
	ctx, cleanup := newOfficialQQBarTestCtx(t, "coc7", map[string]*ds.VMValue{
		"自定义甲": intVal(3),
	})
	defer cleanup()
	ctx.Player.AutoSetNameTemplate = "{$t玩家_RAW} 自定义{自定义甲}"

	bar := officialQQCharacterStatusBar(ctx)
	if !strings.Contains(bar, "自定义3") {
		t.Fatalf("expected custom attribute to be evaluated, got %q", bar)
	}
}

// ---------- 转义安全 ----------

func TestOfficialQQEscapeMathTextProtectsMarkdown(t *testing.T) {
	const dangerous = `a\b{c}d$e#f%g_h^i&j~k`

	escaped := officialQQEscapeMathText(dangerous)
	for _, ch := range []string{`\`, `{`, `}`, `$`, `#`, `%`, `_`, `^`, `&`, `~`} {
		if strings.Contains(escaped, ch) {
			t.Fatalf("escaped text still contains %q: %q", ch, escaped)
		}
	}
	if !strings.Contains(escaped, "ａ") && !strings.Contains(escaped, "a") {
		t.Fatalf("escaping dropped content: %q", escaped)
	}
}

func TestOfficialQQStatusBarSurvivesSpecialCharactersInAttributes(t *testing.T) {
	ctx, cleanup := newOfficialQQBarTestCtx(t, "coc7", map[string]*ds.VMValue{
		"备注": ds.NewStrVal(`a\b{c}$d_e`),
	})
	defer cleanup()
	ctx.Player.AutoSetNameTemplate = "{$t玩家_RAW} N{备注}"

	bar := officialQQCharacterStatusBar(ctx)
	// 数学片段内的 $ 已被转义成全角，所以整串里应该只剩一对定界符
	if strings.Count(bar, "$") != 2 {
		t.Fatalf("math segment was broken by special characters: %q", bar)
	}
	if strings.Count(bar, "{") != strings.Count(bar, "}") {
		t.Fatalf("braces became unbalanced: %q", bar)
	}
	if !strings.HasPrefix(bar, "$\\scriptsize\\textcolor{#E5484D}{\\text{") || !strings.HasSuffix(bar, "}}$") {
		t.Fatalf("math segment is not well-formed: %q", bar)
	}
	// 只取出「文本内容」那一层（\text{...} 里面），模板自带的 \scriptsize、颜色代码不算
	inner := bar[strings.LastIndex(bar, `\text{`)+len(`\text{`) : strings.LastIndex(bar, "}}")]
	// 这些半角符号会破坏片段，必须已被换成全角
	for _, token := range []string{"{c}", "_", "#", "%", "^", "&", `\b`} {
		if strings.Contains(inner, token) {
			t.Fatalf("text still contains %q, which can break the math segment: %q", token, bar)
		}
	}
	if !strings.Contains(inner, "＼") || !strings.Contains(inner, "｛") || !strings.Contains(inner, "｝") {
		t.Fatalf("expected full-width escapes in the text, got %q", inner)
	}
	if !strings.Contains(inner, "调查员甲") {
		t.Fatalf("expected the character name inside the segment, got %q", inner)
	}
}

func TestOfficialQQStatusBarNewlinesDoNotBreakMathSegment(t *testing.T) {
	ctx, cleanup := newOfficialQQBarTestCtx(t, "coc7", map[string]*ds.VMValue{
		"备注": ds.NewStrVal("第一行\n第二行"),
	})
	defer cleanup()
	ctx.Player.AutoSetNameTemplate = "{$t玩家_RAW} {备注}"

	bar := officialQQCharacterStatusBar(ctx)
	// 属性里的换行会被压成空格，状态栏整体必须仍是单行
	if lines := strings.Split(bar, "\n"); len(lines) != 1 {
		t.Fatalf("expected a single line, got %d: %q", len(lines), bar)
	}
}

// ---------- 附加行为 ----------

func TestWithOfficialQQCharacterStatusBarPrependsOnce(t *testing.T) {
	ctx, cleanup := newOfficialQQBarTestCtx(t, "coc7", map[string]*ds.VMValue{"血条": intVal(12)})
	defer cleanup()
	ctx.Player.AutoSetNameTemplate = "{$t玩家_RAW} HP{血条}"

	once := withOfficialQQCharacterStatusBar(ctx, "1d20=7")
	if !strings.HasSuffix(once, "1d20=7") {
		t.Fatalf("expected the original text to be kept at the end, got %q", once)
	}
	twice := withOfficialQQCharacterStatusBar(ctx, once)
	if twice != once {
		t.Fatalf("expected the status bar to be added only once\nfirst: %q\nsecond: %q", once, twice)
	}
}

func TestWithOfficialQQCharacterStatusBarEmptyTextUntouched(t *testing.T) {
	ctx, cleanup := newOfficialQQBarTestCtx(t, "coc7", map[string]*ds.VMValue{"血条": intVal(12)})
	defer cleanup()
	ctx.Player.AutoSetNameTemplate = "{$t玩家_RAW} HP{血条}"

	if got := withOfficialQQCharacterStatusBar(ctx, ""); got != "" {
		t.Fatalf("expected empty text to stay empty, got %q", got)
	}
}

// ---------- 端到端：真实掷骰指令 ----------

// newOfficialQQRollEnv 搭建一个「玩家已设置 .sn」的群，并返回官方 QQ 端点。
func newOfficialQQRollEnv(t *testing.T, groupID, userID, template string, attrs map[string]*ds.VMValue) (*Dice, *EndPointInfo, func()) {
	t.Helper()

	d, ep, _, cleanup := newExecuteNewTestDice(t)
	ep.Platform = "QQ"
	ep.ProtocolType = "official"

	if operator, ok := d.DBOperator.(*mockDatabaseOperator); ok {
		if err := operator.GetDataDB(constant.WRITE).AutoMigrate(&model.AttributesItemModel{}); err != nil {
			cleanup()
			t.Fatalf("AutoMigrate attrs: %v", err)
		}
	}

	group := &GroupInfo{
		Active:          true,
		GroupID:         groupID,
		GroupName:       "官方群",
		System:          "coc7",
		DiceIDActiveMap: new(SyncMap[string, bool]),
		DiceIDExistsMap: new(SyncMap[string, bool]),
		BotList:         new(SyncMap[string, bool]),
		Players:         new(SyncMap[string, *GroupPlayerInfo]),
		PlayerGroups:    new(SyncMap[string, []string]),
	}
	group.DiceIDActiveMap.Store(ep.UserID, true)
	group.DiceIDExistsMap.Store(ep.UserID, true)
	group.Players.Store(userID, &GroupPlayerInfo{
		Name:                "调查员甲",
		UserID:              userID,
		ValueMapTemp:        &ds.ValueMap{},
		AutoSetNameTemplate: template,
	})
	d.ImSession.ServiceAtNew.Store(groupID, group)

	item := &AttributesItem{
		ID:        groupID + "-" + userID,
		valueMap:  &ds.ValueMap{},
		SheetType: "coc7",
		Name:      "调查员甲",
	}
	for key, value := range attrs {
		item.Store(key, value)
	}
	d.AttrsManager.m.Store(item.ID, item)

	return d, ep, cleanup
}

func TestOfficialQQRollReplyCarriesStatusBar(t *testing.T) {
	d, ep, cleanup := newOfficialQQRollEnv(t, "OpenQQ-Group:200-group", "OpenQQ:200-member",
		"{$t玩家_RAW} HP{血条}",
		map[string]*ds.VMValue{"血条": intVal(12)},
	)
	defer cleanup()

	d.ImSession.ExecuteNew(ep, newGroupMsg("OpenQQ-Group:200-group", "OpenQQ:200-member", ".r 1d6"))

	adapter, ok := ep.Adapter.(*mockPlatformAdapter)
	if !ok {
		t.Fatalf("unexpected adapter type %T", ep.Adapter)
	}
	reply, got := adapter.waitForMsg(3 * time.Second)
	if !got {
		t.Fatal("timeout: expected a reply to '.r 1d6'")
	}
	if !strings.Contains(reply, `\textcolor{#E5484D}{\text{调查员甲 HP12}}`) {
		t.Fatalf("expected the virtual status bar on top of the roll reply, got %q", reply)
	}
	// 状态栏必须在最顶部，且整体是单个数学片段
	if !strings.HasPrefix(reply, "$\\scriptsize") {
		t.Fatalf("expected the status bar to be the first line, got %q", reply)
	}
	if !strings.Contains(reply, "调查员甲") {
		t.Fatalf("expected the character name in the reply, got %q", reply)
	}
}

func TestOfficialQQRollReplyHasNoStatusBarOnOneBot(t *testing.T) {
	d, ep, cleanup := newOfficialQQRollEnv(t, "QQ-Group:200", "QQ:200",
		"{$t玩家_RAW} HP{血条}",
		map[string]*ds.VMValue{"血条": intVal(12)},
	)
	defer cleanup()
	// 换成 OneBot 端点：行为必须与改动前一致
	ep.ProtocolType = "onebot"

	d.ImSession.ExecuteNew(ep, newGroupMsg("QQ-Group:200", "QQ:200", ".r 1d6"))

	adapter, ok := ep.Adapter.(*mockPlatformAdapter)
	if !ok {
		t.Fatalf("unexpected adapter type %T", ep.Adapter)
	}
	reply, got := adapter.waitForMsg(3 * time.Second)
	if !got {
		t.Fatal("timeout: expected a reply to '.r 1d6'")
	}
	if strings.Contains(reply, `\textcolor`) || strings.Contains(reply, `\scriptsize`) {
		t.Fatalf("OneBot reply must not contain the official QQ status bar, got %q", reply)
	}
}

func TestOfficialQQRollReplyWithoutSnTemplate(t *testing.T) {
	d, ep, cleanup := newOfficialQQRollEnv(t, "OpenQQ-Group:300-group", "OpenQQ:300-member",
		"",
		map[string]*ds.VMValue{"血条": intVal(12)},
	)
	defer cleanup()

	d.ImSession.ExecuteNew(ep, newGroupMsg("OpenQQ-Group:300-group", "OpenQQ:300-member", ".r 1d6"))

	adapter, ok := ep.Adapter.(*mockPlatformAdapter)
	if !ok {
		t.Fatalf("unexpected adapter type %T", ep.Adapter)
	}
	reply, got := adapter.waitForMsg(3 * time.Second)
	if !got {
		t.Fatal("timeout: expected a reply to '.r 1d6'")
	}
	if strings.Contains(reply, `\scriptsize`) {
		t.Fatalf("expected no status bar when .sn is not set, got %q", reply)
	}
}
