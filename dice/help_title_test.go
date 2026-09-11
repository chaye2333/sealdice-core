//nolint:testpackage
package dice

import (
	"strings"
	"testing"

	wr "github.com/mroth/weightedrand/v3"
)

// TestHelpFirstLineIsHardcodedAndIndependentFromDiceName 锁定 .help 第一行的语义。
//
// 这一行是**程序自身的标题**，刻意保持硬编码，不走「核心:骰子名字」文案模板：
// 那个模板是玩家侧的骰娘名字（会被掷骰文案等引用，用户常改成像"罗兰"这样的名字），
// 如果 .help 跟着它变，标题就会变成"罗兰 1.6.2-dev+..."这种混搭。
func TestHelpFirstLineIsHardcodedAndIndependentFromDiceName(t *testing.T) {
	env := newBindTestEnv(t)
	defer env.cleanup()

	const customDiceName = "罗兰"
	chooser, err := wr.NewChooser(wr.NewChoice(customDiceName, uint(1)))
	if err != nil {
		t.Fatalf("build chooser: %v", err)
	}
	env.d.TextMap["核心:骰子名字"] = chooser

	result := env.d.CmdMap["help"].Solve(env.ctx, env.msg, &CmdArgs{Args: []string{}})
	if !result.Matched || !result.Solved {
		t.Fatalf("expected .help to be handled, got %+v", result)
	}

	reply := waitGroupMessage(t, env)
	firstLine, rest, _ := strings.Cut(reply, "\n")
	if !strings.HasPrefix(firstLine, "鲸娘与豹 ") {
		t.Fatalf(".help 第一行应为固定的标题，实际为 %q", firstLine)
	}
	if !strings.Contains(firstLine, VERSION.String()) {
		t.Fatalf(".help 第一行应包含版本号，实际为 %q", firstLine)
	}
	// 关键：不能被「核心:骰子名字」影响
	if strings.Contains(firstLine, customDiceName) {
		t.Fatalf(".help 第一行不应跟随「核心:骰子名字」变化，实际为 %q", firstLine)
	}
	// fork 说明必须留在帮助里，方便使用者知道自己在跑哪个版本
	if !strings.Contains(rest, "该fork版本主要是适配官bot的功能，代码鲸鱼写的。") {
		t.Fatalf(".help 应包含 fork 说明，实际为:\n%s", reply)
	}
}
