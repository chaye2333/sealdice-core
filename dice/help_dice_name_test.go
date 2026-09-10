//nolint:testpackage
package dice

import (
	"strings"
	"testing"

	wr "github.com/mroth/weightedrand/v3"
)

// TestHelpFirstLineUsesDiceNameTemplate 锁定 .help 第一行的名字来源。
//
// 这一行以前是硬编码的 "海豹核心 " + 版本号，导致用户改了「核心:骰子名字」
// 之后 .help 里还是旧名字。现在改成读同一个文案模板，改名只需改一处。
func TestHelpFirstLineUsesDiceNameTemplate(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()

	const customName = "测试骰娘"
	chooser, err := wr.NewChooser(wr.NewChoice(customName, uint(1)))
	if err != nil {
		t.Fatalf("build chooser: %v", err)
	}
	env.d.TextMap["核心:骰子名字"] = chooser

	result := env.d.CmdMap["help"].Solve(env.ctx, env.msg, &CmdArgs{Args: []string{}})
	if !result.Matched || !result.Solved {
		t.Fatalf("expected .help to be handled, got %+v", result)
	}

	reply := waitGroupMessage(t, env)
	firstLine, _, _ := strings.Cut(reply, "\n")
	if !strings.HasPrefix(firstLine, customName+" ") {
		t.Fatalf(".help 第一行应使用「核心:骰子名字」的值，实际为 %q", firstLine)
	}
	if !strings.Contains(firstLine, VERSION.String()) {
		t.Fatalf(".help 第一行应包含版本号，实际为 %q", firstLine)
	}
	if strings.Contains(firstLine, "海豹核心") {
		t.Fatalf(".help 第一行不应再出现硬编码的旧名字，实际为 %q", firstLine)
	}
}

// TestHelpFirstLineFallsBackWhenTemplateMissing 模板缺失时不能输出空名字或占位符。
func TestHelpFirstLineFallsBackWhenTemplateMissing(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()

	delete(env.d.TextMap, "核心:骰子名字")

	env.d.CmdMap["help"].Solve(env.ctx, env.msg, &CmdArgs{Args: []string{}})
	reply := waitGroupMessage(t, env)
	firstLine, _, _ := strings.Cut(reply, "\n")
	if strings.HasPrefix(firstLine, " ") {
		t.Fatalf(".help 第一行以空格开头，说明名字取空了：%q", firstLine)
	}
	if strings.Contains(firstLine, "未知项") {
		t.Fatalf(".help 第一行泄漏了模板占位符：%q", firstLine)
	}
	if !strings.Contains(firstLine, VERSION.String()) {
		t.Fatalf(".help 第一行应包含版本号，实际为 %q", firstLine)
	}
}

// TestDefaultDiceNameIsConfigured 默认文案应已改为「鲸鱼娘与海豹娘的故事」。
func TestDefaultDiceNameIsConfigured(t *testing.T) {
	d, _, _, cleanup := newExecuteNewTestDice(t)
	defer cleanup()

	text := DiceFormatTmpl(&MsgContext{Dice: d}, "核心:骰子名字")
	if !strings.Contains(text, "鲸鱼娘与海豹娘的故事") {
		t.Fatalf("核心:骰子名字 的默认值应为「鲸鱼娘与海豹娘的故事」，实际 %q", text)
	}
}
