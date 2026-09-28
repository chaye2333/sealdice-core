package main

import (
	"bytes"
	"image/png"
	"strings"
	"testing"
)

// newTestRenderer 拿本机字体建一个渲染器；没有任何中日韩字体时跳过
// （CI 的 ubuntu runner 默认没装中文字体，不该因此判失败）。
func newTestRenderer(t *testing.T, opts RenderOptions) *Renderer {
	t.Helper()
	r, err := NewRenderer(opts)
	if err != nil {
		t.Skipf("本机没有可用字体，跳过渲染测试：%v", err)
	}
	return r
}

func TestRenderPNGProducesValidImage(t *testing.T) {
	r := newTestRenderer(t, RenderOptions{})

	md := "# 标题\n\n正文**加粗**、`行内代码`与中文折行测试。\n\n- 列表项一\n- 列表项二\n\n> 引用\n\n```text\n.ra 侦查\n```\n\n---\n"
	out, err := r.RenderPNG(md, 820)
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("输出的不是合法 PNG：%v", err)
	}
	if b := img.Bounds(); b.Dx() != 820 {
		t.Fatalf("宽度应为 820，实际 %d", b.Dx())
	} else if b.Dy() < 80 {
		t.Fatalf("高度过小（%d），内容可能没画上去", b.Dy())
	}
	if len(out) > 400*1024 {
		t.Fatalf("图片过大：%d 字节（聊天图片不该超过几百 KB）", len(out))
	}
}

func TestRenderPNGRejectsTooLongText(t *testing.T) {
	r := newTestRenderer(t, RenderOptions{MaxTextRune: 10})

	if _, err := r.RenderPNG(strings.Repeat("字", 11), 820); err == nil {
		t.Fatal("超过 max-text-chars 应报错（骰子侧据此回退文本）")
	}
}

func TestRenderPNGClampsWidth(t *testing.T) {
	r := newTestRenderer(t, RenderOptions{})

	out, err := r.RenderPNG("窄", 100)
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("输出的不是合法 PNG：%v", err)
	}
	if img.Bounds().Dx() != 320 {
		t.Fatalf("过窄的宽度应被抬到 320，实际 %d", img.Bounds().Dx())
	}
}

func TestRenderPNGEmptyTextStillRenders(t *testing.T) {
	r := newTestRenderer(t, RenderOptions{})

	out, err := r.RenderPNG("   \n\n", 600)
	if err != nil {
		t.Fatalf("空白文本不该报错：%v", err)
	}
	if len(out) == 0 {
		t.Fatal("空白文本也应产出一张图（骰子侧已保证 text 非空，这里是兜底）")
	}
}
