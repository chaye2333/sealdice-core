//nolint:testpackage
package dice

import (
	"strings"
	"testing"
	"time"

	ds "github.com/sealdice/dicescript"

	"sealdice-core/dice/service"
	"sealdice-core/model"
	"sealdice-core/utils/constant"
)

// newStatusBarLogCtx 造一个会挂状态栏的官方 QQ 上下文。
func newStatusBarLogCtx(t *testing.T) (*MsgContext, func()) {
	t.Helper()
	ctx, cleanup := newOfficialQQBarTestCtx(t, "coc7", map[string]*ds.VMValue{
		"血条":  intVal(12),
		"血上限": intVal(12),
	})
	ctx.Player.AutoSetNameTemplate = "{$t玩家_RAW} HP{血条}/{血上限}"
	ctx.OfficialQQStatusBarPending = false
	return ctx, cleanup
}

// TestOfficialQQStatusBarLineTrackedForDisplayOnly 状态栏前缀要记在上下文上，
// 且能原样剥掉；不加状态栏的回复要把上一次的记录清掉。
func TestOfficialQQStatusBarLineTrackedForDisplayOnly(t *testing.T) {
	ctx, cleanup := newStatusBarLogCtx(t)
	defer cleanup()

	const text = "1d20=7"
	ctx.OfficialQQStatusBarPending = true // 模板渲染最终回复时会打这个标记
	got := withOfficialQQCharacterStatusBar(ctx, text)
	if !strings.HasPrefix(got, "$\\scriptsize") {
		t.Fatalf("状态栏没挂上: %q", got)
	}

	line := officialQQStatusBarLineOf(ctx)
	if line == "" {
		t.Fatal("状态栏前缀没有记录到上下文")
	}
	if got != line+text {
		t.Fatalf("记录的前缀和实际拼上去的不一致: line=%q text=%q", line, got)
	}
	if stripped := stripOfficialQQStatusBar(line, got); stripped != text {
		t.Fatalf("剥离后 = %q, want %q", stripped, text)
	}

	// 不挂状态栏的回复：不能留着上一次的记录，否则日志钩子会去剥旧前缀
	if out := withOfficialQQCharacterStatusBar(ctx, "普通的一句话"); out != "普通的一句话" {
		t.Fatalf("普通回复不该挂状态栏: %q", out)
	}
	if line := officialQQStatusBarLineOf(ctx); line != "" {
		t.Fatalf("普通回复后应清空状态栏记录，实际 %q", line)
	}
}

func TestStripOfficialQQStatusBarNoop(t *testing.T) {
	if got := stripOfficialQQStatusBar("", "正文"); got != "正文" {
		t.Fatalf("空前缀应原样返回: %q", got)
	}
	if got := stripOfficialQQStatusBar("前缀\n", ""); got != "" {
		t.Fatalf("空正文应原样返回: %q", got)
	}
	// 正文里恰好也有同样的文字：只去掉第一处，不误伤后面的
	if got := stripOfficialQQStatusBar("A\n", "A\n正文A\n"); got != "正文A\n" {
		t.Fatalf("应只去掉首次出现的状态栏: %q", got)
	}
}

// TestLogHookDropsOfficialQQStatusBar 端到端：带状态栏的回复写进跑团日志时，
// 日志里只有干净正文（红字不进日志）。
func TestLogHookDropsOfficialQQStatusBar(t *testing.T) {
	ctx, cleanup := newStatusBarLogCtx(t)
	defer cleanup()

	logDB := ctx.Dice.DBOperator.GetLogDB(constant.WRITE)
	if err := logDB.AutoMigrate(&model.LogInfo{}, &model.LogOneItem{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}

	group := ctx.Group
	group.SetLogState(0, "测试", true)
	ctx.Group = group

	ext, ok := ctx.Dice.ExtRegistry.Load("log")
	if !ok || ext == nil || ext.OnMessageSend == nil {
		t.Fatal("日志扩展没有注册")
	}

	const body = "1d20=7"
	ctx.OfficialQQStatusBarPending = true
	text := withOfficialQQCharacterStatusBar(ctx, body)
	if !strings.HasPrefix(text, "$\\scriptsize") {
		t.Fatalf("状态栏没挂上: %q", text)
	}

	msg := &Message{
		Platform:    "QQ",
		MessageType: "group",
		GroupID:     group.GroupID,
		Time:        time.Now().Unix(),
		Message:     text,
		RawID:       "dice-1",
		Sender: SenderBase{
			UserID:   ctx.EndPoint.UserID,
			Nickname: ctx.EndPoint.Nickname,
		},
	}
	ext.OnMessageSend(ctx, msg, "")

	lines, err := service.LogGetAllLines(ctx.Dice.DBOperator, group.GroupID, "测试")
	if err != nil {
		t.Fatalf("LogGetAllLines: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("日志条数 = %d, want 1", len(lines))
	}
	if lines[0].Message != body {
		t.Fatalf("日志正文 = %q, want %q（状态栏不该进日志）", lines[0].Message, body)
	}
	if strings.Contains(lines[0].Message, "scriptsize") || strings.Contains(lines[0].Message, "textcolor") {
		t.Fatalf("日志里还带着状态栏红字: %q", lines[0].Message)
	}
}
