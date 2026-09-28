//nolint:testpackage
package dice

import (
	"sort"
	"strings"
	"testing"

	"sealdice-core/dice/censor"
)

// newMaskTestDice 造一个只带敏感词引擎的 Dice（DB 为 nil —— 掩码走纯匹配路径，
// 本来就不该碰数据库，所以这里能跑通本身就说明没写库）。
func newMaskTestDice(words ...string) *Dice {
	c := &censor.Censor{SensitiveKeys: make(map[string]censor.WordInfo)}
	for _, w := range words {
		c.SensitiveKeys[strings.ToLower(w)] = censor.WordInfo{Level: censor.Danger, Origin: w}
	}
	_ = c.Load()

	d := &Dice{CensorManager: &CensorManager{Censor: c}}
	d.Config.CensorMaskEnable = true
	d.Config.CensorMaskChar = DefaultCensorMaskChar
	return d
}

// TestCensorMaskWordsEqualLength 掩码要「按命中词的字数」等长替换。
func TestCensorMaskWordsEqualLength(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		words []string
		mask  string
		want  string
	}{
		{"二字词", "这里有天主", []string{"天主"}, "口", "这里有口口"},
		{"四字词", "信仰天主教义", []string{"天主教义"}, "口", "信仰口口口口"},
		{"长词优先（乱序传入也不出错）", "天主教", []string{"天主", "天主教"}, "口", "口口口"},
		{"自定义掩码字符", "abc", []string{"abc"}, "*", "***"},
		{"掩码为空时回退默认值", "abc", []string{"abc"}, "", "口口口"},
		{"多处命中", "天主与圣母", []string{"天主", "圣母"}, "口", "口口与口口"},
		{"没命中就不动", "正常文本", []string{"天主"}, "口", "正常文本"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := censorMaskWords(tt.in, tt.words, tt.mask)
			if got != tt.want {
				t.Fatalf("censorMaskWords(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestCensorMaskWordsSkipsCQCodes CQ 码/海豹码内部绝不能动：
// 改坏一个参数轻则图发不出去，重则把整个码截断。
func TestCensorMaskWordsSkipsCQCodes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"CQ 参数内不替换", "[CQ:image,file=天主.png]天主", "[CQ:image,file=天主.png]口口"},
		{"海豹码内不替换", "[img:天主.png]天主", "[img:天主.png]口口"},
		{"CQ 码在中间", "前[CQ:at,qq=天主]后天主", "前[CQ:at,qq=天主]后口口"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := censorMaskWords(tt.in, []string{"天主"}, "口")
			if got != tt.want {
				t.Fatalf("censorMaskWords(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestCensorMaskOutgoingRequiresSwitch 开关关着时一个字符都不该改。
func TestCensorMaskOutgoingRequiresSwitch(t *testing.T) {
	d := newMaskTestDice("天主")
	d.Config.CensorMaskEnable = false

	got, words := d.CensorMaskOutgoing("信仰天主")
	if got != "信仰天主" {
		t.Fatalf("开关关闭时不该修改文本，got %q", got)
	}
	if len(words) != 0 {
		t.Fatalf("开关关闭时不该报告命中，got %v", words)
	}
}

// TestCensorMaskOutgoingUsesEngine 开关打开时走引擎匹配并等长打码。
func TestCensorMaskOutgoingUsesEngine(t *testing.T) {
	d := newMaskTestDice("天主", "圣母")

	got, words := d.CensorMaskOutgoing("信仰天主与圣母")
	if got != "信仰口口与口口" {
		t.Fatalf("got %q, want %q", got, "信仰口口与口口")
	}
	sort.Strings(words)
	if strings.Join(words, ",") != "圣母,天主" {
		t.Fatalf("命中的词不对：%v", words)
	}
}

// TestCensorHitWordsSortedLongFirst 命中词按长词优先返回。
//
// 注意：引擎对**同一位置**上互相重叠的多个词只报一个（"天主教" 只报 "天主"），
// 所以这里用两个不重叠、长度不同的词来验证排序。
func TestCensorHitWordsSortedLongFirst(t *testing.T) {
	d := newMaskTestDice("天主", "圣母院")

	words := d.CensorHitWords("天主与圣母院")
	if len(words) != 2 {
		t.Fatalf("应命中两个词，got %v", words)
	}
	if words[0] != "圣母院" || words[1] != "天主" {
		t.Fatalf("长词应排在前面，got %v", words)
	}
}

// TestCensorMaskOutgoingToleratesEmptyDice 没有引擎 / nil 接收者都不能 panic。
func TestCensorMaskOutgoingToleratesEmptyDice(t *testing.T) {
	var nilDice *Dice
	if got, _ := nilDice.CensorMaskOutgoing("天主"); got != "天主" {
		t.Fatalf("nil Dice 应原样返回，got %q", got)
	}

	empty := &Dice{}
	empty.Config.CensorMaskEnable = true
	if got, _ := empty.CensorMaskOutgoing("天主"); got != "天主" {
		t.Fatalf("没有引擎时应原样返回，got %q", got)
	}
}

// TestPlanHelpReply 帮助正文的发送策略：
// 只有「命中敏感词」或「超过长度阈值」才转图片，其余仍是文本。
func TestPlanHelpReply(t *testing.T) {
	hit := []string{"天主"}
	noHit := []string(nil)

	tests := []struct {
		name       string
		cfg        *HelpConfig
		hitWords   []string
		runeLen    int
		wantImage  bool
		wantMasked bool
	}{
		{"没有配置：永远发文本", nil, hit, 99999, false, false},
		{"开关关闭：命中也不转", &HelpConfig{ImageRenderEnable: false, ImageRenderMinLength: 10}, hit, 999, false, false},
		{"命中敏感词：转图片（图片里默认保留原文）", &HelpConfig{ImageRenderEnable: true}, hit, 20, true, false},
		{"命中敏感词 + 图片里也打码", &HelpConfig{ImageRenderEnable: true, ImageRenderMaskInImage: true}, hit, 20, true, true},
		{"未命中但过长：转图片", &HelpConfig{ImageRenderEnable: true, ImageRenderMinLength: 100}, noHit, 100, true, false},
		{"未命中且不够长：发文本", &HelpConfig{ImageRenderEnable: true, ImageRenderMinLength: 100}, noHit, 99, false, false},
		{"长度阈值为 0：不按长度转", &HelpConfig{ImageRenderEnable: true, ImageRenderMinLength: 0}, noHit, 100000, false, false},
		{"命中时即使长度为 0 也转", &HelpConfig{ImageRenderEnable: true}, hit, 0, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := planHelpReply(tt.cfg, tt.hitWords, tt.runeLen)
			if plan.useImage != tt.wantImage {
				t.Fatalf("useImage = %v, want %v", plan.useImage, tt.wantImage)
			}
			if plan.maskInImage != tt.wantMasked {
				t.Fatalf("maskInImage = %v, want %v", plan.maskInImage, tt.wantMasked)
			}
			if plan.hit != (len(tt.hitWords) > 0) {
				t.Fatalf("hit = %v, want %v", plan.hit, len(tt.hitWords) > 0)
			}
		})
	}
}

// TestRenderHelpDocImageValidatesConfig 渲染后端地址没配 / 不合法时要明确报错，
// 而不是静默发出一个坏链接。
func TestRenderHelpDocImageValidatesConfig(t *testing.T) {
	if _, err := renderHelpDocImage(nil, "x", false); err == nil {
		t.Fatal("nil 配置应报错")
	}
	if _, err := renderHelpDocImage(&HelpConfig{}, "x", false); err == nil {
		t.Fatal("没配地址应报错")
	}
	if _, err := renderHelpDocImage(&HelpConfig{ImageRenderURL: "http://127.0.0.1:1/render", ImageRenderTimeoutSec: 1}, "x", false); err == nil {
		t.Fatal("连不上应报错（用于触发回退文本）")
	}
}

// TestHelpConfigImageRenderDefaults 渲染参数的默认值。
func TestHelpConfigImageRenderDefaults(t *testing.T) {
	cfg := &HelpConfig{}
	if cfg.imageRenderWidth() != helpImageRenderDefaultWidth {
		t.Fatalf("宽度默认值不对：%d", cfg.imageRenderWidth())
	}
	if cfg.imageRenderTimeout().Seconds() != helpImageRenderDefaultTimeoutSec {
		t.Fatalf("超时默认值不对：%v", cfg.imageRenderTimeout())
	}

	cfg.ImageRenderWidth = 1200
	cfg.ImageRenderTimeoutSec = 30
	if cfg.imageRenderWidth() != 1200 || cfg.imageRenderTimeout().Seconds() != 30 {
		t.Fatal("显式配置应覆盖默认值")
	}
}
