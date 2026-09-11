package dice

import (
	"fmt"
	"regexp"
	"strings"
)

// 本文件实现「QQ 官方机器人虚拟角色状态栏」。
//
// 背景：QQ 官方机器人没有修改群成员名片的接口，所以 .sn 设置的名片模板
// 在官方 QQ 上完全没有效果。这里改为在掷骰 / 检定回复的顶部渲染一条
// “虚拟状态栏”：属性行用 QQ Markdown 的小号红字，角色名与正文保持正常字号。
//
// 设计要点：
//  1. 不硬编码 COC / DND / 任何规则系统的属性字段，只读取玩家当前保存的
//     AutoSetNameTemplate 并实时求值，因此能兼容已有和未来的所有 .sn 写法。
//  2. 只对 QQ 官方机器人端点生效，OneBot 等平台完全不受影响。
//  3. 不写入、不改写任何用户文案模板。
//  4. .sn none / .sn off 时 AutoSetNameTemplate 为空，自动不显示状态栏。

// officialQQCharacterStatusBarMarkdownColor 状态栏属性行的颜色（小号红字）。
const officialQQCharacterStatusBarMarkdownColor = "#E5484D"

// officialQQPlayerPlaceholderRe 匹配 .sn 模板里的玩家名占位符。
// 常见写法有 {$t玩家_RAW} 与 {$t玩家}，也允许中间出现空格。
var officialQQPlayerPlaceholderRe = regexp.MustCompile(`\{\s*\$t玩家(?:_RAW)?\s*\}`)

// officialQQMathEscapeReplacer 把会破坏 QQ Markdown 数学片段的字符换成全角字符。
// 这些字符出现在属性值里会让 LaTeX 片段提前结束或被解析成别的公式。
var officialQQMathEscapeReplacer = strings.NewReplacer(
	`\`, `＼`, // 反斜杠 -> 全角，同时规避 LaTeX 命令
	`{`, `｛`,
	`}`, `｝`,
	`$`, `＄`, // 避免提前结束数学片段
	`#`, `＃`,
	`%`, `％`,
	`_`, `＿`,
	`^`, `＾`,
	`&`, `＆`, // LaTeX 对齐符
	`~`, `～`,
	"\r\n", " ",
	"\n", " ",
	"\r", " ",
	"\t", " ",
)

// isOfficialQQEndpoint 是否为 QQ 官方机器人端点。
// 状态栏只在这里启用，其它平台（含 OneBot）行为不变。
func isOfficialQQEndpoint(ep *EndPointInfo) bool {
	if ep == nil {
		return false
	}
	return ep.Platform == "QQ" && ep.ProtocolType == "official"
}

// officialQQEscapeMathText 转义要放进 QQ Markdown 数学片段里的文本。
func officialQQEscapeMathText(text string) string {
	return officialQQMathEscapeReplacer.Replace(text)
}

// officialQQEvalStatusBarTemplate 安全求值状态栏模板。
//
// 状态栏会在每一条掷骰 / 检定回复上渲染，属于热路径：
// 骰点引擎内部有若干 lo.Must 之类的断言，模板写错或数据库暂时不可用都可能 panic。
// 这里兜底恢复，宁可少显示一行状态栏，也不能让整条掷骰指令崩掉。
func officialQQEvalStatusBarTemplate(ctx *MsgContext, tmpl string) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			if ctx != nil && ctx.Dice != nil && ctx.Dice.Logger != nil {
				ctx.Dice.Logger.Warnf("渲染 QQ 官方角色状态栏失败，已跳过属性行: %v", r)
			}
			text = ""
			err = fmt.Errorf("渲染角色状态栏时发生异常: %v", r)
		}
	}()
	return EvalPlayerGroupCardTemplate(ctx, tmpl)
}

// officialQQCharacterAttributeLine 从 .sn 模板里取出「属性行」并实时求值。
//
// .sn 模板的通用形态是「玩家名占位符 + 属性表达式」，例如：
//
//	{$t玩家_RAW} HP{hp}/{hpmax} AC{ac}
//	{$t玩家_RAW} SAN{理智} HP{生命值}/{生命值上限} DEX{敏捷}
//	{$t玩家_RAW}                     <- 只设置了名字，没有属性
//
// 这里把玩家名占位符删掉，只对剩下的部分求值，因此不需要为每种规则维护字段表。
// 返回空字符串表示该模板没有属性部分（或求值失败），调用方应跳过属性行。
func officialQQCharacterAttributeLine(ctx *MsgContext) string {
	if ctx == nil || ctx.Player == nil {
		return ""
	}
	tmpl := identityBindPlayerNameTemplate(ctx)
	if tmpl == "" {
		return ""
	}
	attributePart := officialQQPlayerPlaceholderRe.ReplaceAllString(tmpl, "")
	attributePart = strings.TrimSpace(attributePart)
	if attributePart == "" {
		// 模板里只有玩家名，没有属性
		return ""
	}

	text, err := officialQQEvalStatusBarTemplate(ctx, attributePart)
	if err != nil {
		return ""
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return text
}

// officialQQCharacterStatusBar 生成完整的状态栏文本（角色名 + 属性，同一行）。
//
// 输出形如：
//
//	$\scriptsize\textcolor{#E5484D}{\text{调查员甲 HP12/12 AC16}}$
//
// 角色名与属性都在同一个数学片段内，因此客户端会整体渲染成小号红字。
func officialQQCharacterStatusBar(ctx *MsgContext) string {
	if ctx == nil || ctx.Player == nil {
		return ""
	}
	if !isOfficialQQEndpoint(ctx.EndPoint) {
		return ""
	}
	// 做过身份绑定时，角色名也取绑定另一侧（旧QQ号）的，这样状态栏和角色卡数据一致。
	name := strings.TrimSpace(ctx.Player.Name)
	if bound := identityBindReadPlayer(ctx); bound != nil && bound != ctx.Player {
		if boundName := strings.TrimSpace(bound.Name); boundName != "" {
			name = boundName
		}
	}
	if name == "" {
		return ""
	}
	// .sn none / .sn off 会把模板清空，此时不显示状态栏。
	// 这里用 identityBindPlayerNameTemplate：官方侧自己没设模板时会回退到绑定另一侧
	// （旧群旧号）的模板，从而直接沿用迁移前设好的名片格式。
	if identityBindPlayerNameTemplate(ctx) == "" {
		return ""
	}

	// 角色名放在最前，便于一眼看到是谁在骰
	parts := []string{officialQQEscapeMathText(name)}
	if attrLine := officialQQCharacterAttributeLine(ctx); attrLine != "" {
		parts = append(parts, officialQQEscapeMathText(attrLine))
	}

	return fmt.Sprintf(
		`$\scriptsize\textcolor{%s}{\text{%s}}$`,
		officialQQCharacterStatusBarMarkdownColor,
		strings.Join(parts, " "),
	)
}

// withOfficialQQCharacterStatusBar 在掷骰 / 鉴定回复顶部加上虚拟角色状态栏。
//
// 由发送层（ReplyToSender / ReplyGroup / ReplyPerson）统一调用，因此对所有规则系统
// 生效，包括 FU、SH 以及第三方扩展。
//
// 是否需要加状态栏由 ctx.OfficialQQStatusBarPending 决定——它由 DiceFormatTmpl
// 在渲染「最终回复」模板时置位，这样帮助文本、错误提示等不会被误加。
// 无论是否真的加了，这里都会消费掉标记，避免同一条回复被处理两次。
func withOfficialQQCharacterStatusBar(ctx *MsgContext, text string) string {
	if ctx == nil {
		return text
	}
	pending := ctx.OfficialQQStatusBarPending
	ctx.OfficialQQStatusBarPending = false
	if !pending || text == "" {
		return text
	}

	bar := officialQQCharacterStatusBar(ctx)
	if bar == "" {
		return text
	}
	// 状态栏已在顶部时不重复添加
	if strings.HasPrefix(text, bar) {
		return text
	}
	return bar + "\n" + text
}
