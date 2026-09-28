package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// errTextTooLong 正文超过渲染上限：骰子侧会回退成文本发送。
var errTextTooLong = errors.New("正文超过渲染上限")

// ---------- 配色（浅色主题，够用不花哨） ----------

var (
	colBg     = color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
	colFg     = color.RGBA{R: 0x21, G: 0x25, B: 0x2B, A: 0xFF}
	colMuted  = color.RGBA{R: 0x6B, G: 0x72, B: 0x80, A: 0xFF}
	colCodeBg = color.RGBA{R: 0xF2, G: 0xF3, B: 0xF5, A: 0xFF}
	colRule   = color.RGBA{R: 0xE3, G: 0xE6, B: 0xEA, A: 0xFF}
	colQuote  = color.RGBA{R: 0xC9, G: 0xD1, B: 0xD9, A: 0xFF}
	colLink   = color.RGBA{R: 0x1A, G: 0x56, B: 0xB0, A: 0xFF}
)

// RenderOptions 渲染参数。
type RenderOptions struct {
	FontPath    string
	FontSize    float64
	LineHeight  float64
	Padding     float64
	MaxHeight   int
	MaxTextRune int
}

// Renderer 一个渲染器 = 一份字体 + 一套排版参数。
// 只做「文本 → PNG」，无状态、不缓存。
type Renderer struct {
	fnt      *opentype.Font
	fontName string
	opts     RenderOptions

	mu    sync.Mutex
	faces map[float64]font.Face
	rw    map[float64]map[rune]float64
}

// 常见中日韩字体：Linux 服务器装 fonts-noto-cjk / fonts-wqy-microhei 即可命中。
var fontCandidates = []string{
	"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
	"/usr/share/fonts/opentype/noto/NotoSansCJKsc-Regular.otf",
	"/usr/share/fonts/truetype/noto/NotoSansCJK-Regular.ttc",
	"/usr/share/fonts/truetype/wqy/wqy-microhei.ttc",
	"/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc",
	"/usr/share/fonts/truetype/arphic/uming.ttc",
	"/System/Library/Fonts/PingFang.ttc",
	"/Library/Fonts/Arial Unicode.ttf",
	`C:\Windows\Fonts\msyh.ttc`,
	`C:\Windows\Fonts\msyh.ttf`,
	`C:\Windows\Fonts\simhei.ttf`,
}

// NewRenderer 载入字体并返回渲染器。
func NewRenderer(opts RenderOptions) (*Renderer, error) {
	path := strings.TrimSpace(opts.FontPath)
	if path == "" {
		path = detectFont()
	}
	if path == "" {
		return nil, errors.New("没找到可用字体，请用 -font 指定，或安装中文字体" +
			"（Debian/Ubuntu: apt install fonts-noto-cjk；Alpine: apk add font-noto-cjk）")
	}

	data, err := os.ReadFile(path) //nolint:gosec // 字体路径由部署者通过命令行指定
	if err != nil {
		return nil, fmt.Errorf("读取字体失败: %w", err)
	}

	var fnt *opentype.Font
	if strings.EqualFold(filepath.Ext(path), ".ttc") {
		coll, collErr := opentype.ParseCollection(data)
		if collErr != nil {
			return nil, fmt.Errorf("解析字体集合失败: %w", collErr)
		}
		fnt, err = coll.Font(0)
		if err != nil {
			return nil, fmt.Errorf("从字体集合取第一个字体失败: %w", err)
		}
	} else {
		fnt, err = opentype.Parse(data)
		if err != nil {
			return nil, fmt.Errorf("解析字体失败: %w", err)
		}
	}

	if opts.FontSize <= 0 {
		opts.FontSize = 17
	}
	if opts.LineHeight <= 0 {
		opts.LineHeight = 1.65
	}
	if opts.Padding < 0 {
		opts.Padding = 0
	}
	if opts.Padding == 0 {
		opts.Padding = 26
	}
	if opts.MaxHeight <= 0 {
		opts.MaxHeight = 20000
	}
	if opts.MaxTextRune <= 0 {
		opts.MaxTextRune = 20000
	}

	return &Renderer{
		fnt:      fnt,
		fontName: path,
		opts:     opts,
		faces:    make(map[float64]font.Face),
		rw:       make(map[float64]map[rune]float64),
	}, nil
}

func detectFont() string {
	if env := strings.TrimSpace(os.Getenv("HELPDOC_RENDER_FONT")); env != "" {
		if _, err := os.Stat(env); err == nil {
			return env
		}
	}
	for _, p := range fontCandidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// FontName 返回实际使用的字体文件路径（启动日志里打出来，便于排查乱码）。
func (r *Renderer) FontName() string { return r.fontName }

func (r *Renderer) face(size float64) (font.Face, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.faces[size]; ok {
		return f, nil
	}
	f, err := opentype.NewFace(r.fnt, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, err
	}
	r.faces[size] = f
	return f, nil
}

// runeWidth 单个字符的宽度（按字号缓存；CJK 的换行判断全靠它）。
func (r *Renderer) runeWidth(size float64, ch rune) float64 {
	r.mu.Lock()
	cache, ok := r.rw[size]
	if !ok {
		cache = make(map[rune]float64)
		r.rw[size] = cache
	}
	if w, ok := cache[ch]; ok {
		r.mu.Unlock()
		return w
	}
	f, ok := r.faces[size]
	r.mu.Unlock()
	if !ok || f == nil {
		return size
	}
	adv, _ := f.GlyphAdvance(ch)
	w := float64(adv) / 64
	cache[ch] = w
	return w
}

func (r *Renderer) textWidth(size float64, s string) float64 {
	total := 0.0
	for _, ch := range s {
		total += r.runeWidth(size, ch)
	}
	return total
}

// ---------- 排版中间结构 ----------

type span struct {
	text string
	bold bool
	code bool
	link bool
}

type vline struct {
	spans       []span
	scale       float64 // 相对基准字号的倍数
	indent      float64 // 额外左缩进
	quote       bool
	codeBlock   bool
	rule        bool
	spaceBefore float64
}

// RenderPNG 把 Markdown 子集渲染成 PNG 字节。
func (r *Renderer) RenderPNG(text string, width int) ([]byte, error) {
	if r == nil || r.fnt == nil {
		return nil, errors.New("渲染器未初始化")
	}
	if n := utf8.RuneCountInString(text); n > r.opts.MaxTextRune {
		return nil, fmt.Errorf("%w：%d 字 > 上限 %d 字", errTextTooLong, n, r.opts.MaxTextRune)
	}
	if width <= 0 {
		width = 820
	}
	if width < 320 {
		width = 320
	}
	if width > 2000 {
		width = 2000
	}

	contentWidth := float64(width) - r.opts.Padding*2
	lines := r.layout(text, contentWidth)

	total := r.opts.Padding * 2
	for _, ln := range lines {
		total += ln.spaceBefore + r.lineHeight(ln)
	}
	height := int(math.Ceil(total))
	if height < 1 {
		height = 1
	}
	if height > r.opts.MaxHeight {
		height = r.opts.MaxHeight
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), image.NewUniform(colBg), image.Point{}, draw.Src)

	y := r.opts.Padding
	for _, ln := range lines {
		y += ln.spaceBefore
		lineH := r.lineHeight(ln)
		if y+lineH > float64(height)-r.opts.Padding*0.5 {
			// 放不下了：画一行提示，避免"图片突然断掉"让人以为内容就这么多
			r.drawNotice(img, width, y, "…（内容过长，已截断）")
			break
		}
		r.drawLine(img, width, y, lineH, ln)
		y += lineH
	}

	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (r *Renderer) lineHeight(ln vline) float64 {
	if ln.rule {
		return 16
	}
	return r.opts.FontSize * ln.scale * r.opts.LineHeight
}

func (r *Renderer) fontFor(ln vline) (font.Face, float64) {
	size := r.opts.FontSize * ln.scale
	face, err := r.face(size)
	if err != nil {
		return nil, size
	}
	return face, size
}

func (r *Renderer) drawNotice(img *image.RGBA, width int, y float64, text string) {
	face, size := r.fontFor(vline{scale: 1})
	if face == nil {
		return
	}
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(colMuted),
		Face: face,
		Dot:  fixed.Point26_6{X: fixed.I(int(r.opts.Padding)), Y: fixed.I(int(y + size))},
	}
	d.DrawString(text)
	_ = width
}

func (r *Renderer) drawLine(img *image.RGBA, width int, y, lineH float64, ln vline) {
	left := r.opts.Padding + ln.indent
	right := float64(width) - r.opts.Padding

	if ln.rule {
		rect := image.Rect(int(left), int(y+lineH/2), int(right), int(y+lineH/2)+1)
		draw.Draw(img, rect, image.NewUniform(colRule), image.Point{}, draw.Src)
		return
	}
	if ln.codeBlock {
		rect := image.Rect(int(r.opts.Padding), int(y), int(right), int(y+lineH))
		draw.Draw(img, rect, image.NewUniform(colCodeBg), image.Point{}, draw.Src)
	}
	if ln.quote {
		rect := image.Rect(int(r.opts.Padding)+2, int(y), int(r.opts.Padding)+6, int(y+lineH))
		draw.Draw(img, rect, image.NewUniform(colQuote), image.Point{}, draw.Src)
		left += 14
	}

	face, _ := r.fontFor(ln)
	if face == nil {
		return
	}
	baseline := y + lineH*0.75
	x := left
	for _, sp := range ln.spans {
		if sp.text == "" {
			continue
		}
		size := r.opts.FontSize * ln.scale
		w := r.textWidth(size, sp.text)
		if sp.code && !ln.codeBlock {
			rect := image.Rect(int(x)-2, int(y+lineH*0.12), int(x+w)+2, int(y+lineH*0.92))
			draw.Draw(img, rect, image.NewUniform(colCodeBg), image.Point{}, draw.Src)
		}
		col := colFg
		if sp.link {
			col = colLink
		}
		dot := fixed.Point26_6{X: fixed.Int26_6(math.Round(x * 64)), Y: fixed.Int26_6(math.Round(baseline * 64))}
		d := &font.Drawer{Dst: img, Src: image.NewUniform(col), Face: face, Dot: dot}
		d.DrawString(sp.text)
		if sp.bold {
			// 没有单独的粗体字重时用"错位重描一次"模拟加粗。
			// 注意必须把 Dot 复位再画：DrawString 会把 Dot 推进到文本末尾，
			// 不复位就会把这段字重复画到**后面文字的位置**上（实测把整行糊掉）。
			d.Dot = dot
			d.Dot.X += fixed.Int26_6(45)
			d.DrawString(sp.text)
		}
		x += w
	}
}

// ---------- Markdown 子集 → 视觉行 ----------

const (
	codeIndent = 12.0
	listIndent = 18.0
)

func (r *Renderer) layout(text string, contentWidth float64) []vline {
	var out []vline
	pending := 0.0

	srcLines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	inCode := false
	var codeLines []string

	flushCode := func() {
		if len(codeLines) == 0 {
			return
		}
		for _, cl := range codeLines {
			wrapped := r.wrapSpans([]span{{text: cl, code: true}}, 0.92, contentWidth-codeIndent)
			if len(wrapped) == 0 {
				wrapped = [][]span{{{text: " "}}}
			}
			for i, ws := range wrapped {
				ln := vline{spans: ws, scale: 0.92, codeBlock: true, indent: codeIndent}
				if i == 0 {
					ln.spaceBefore = pending + r.opts.FontSize*0.35
					pending = 0
				}
				out = append(out, ln)
			}
		}
		pending = r.opts.FontSize * 0.5
		codeLines = nil
	}

	for _, raw := range srcLines {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") {
			if inCode {
				flushCode()
				inCode = false
			} else {
				inCode = true
				codeLines = nil
			}
			continue
		}
		if inCode {
			codeLines = append(codeLines, line)
			continue
		}

		if trimmed == "" {
			if pending < r.opts.FontSize*0.7 {
				pending = r.opts.FontSize * 0.7
			}
			continue
		}

		if isHRule(trimmed) {
			out = append(out, vline{rule: true, spaceBefore: pending + r.opts.FontSize*0.4})
			pending = r.opts.FontSize * 0.4
			continue
		}

		if lvl, title, ok := parseHeading(trimmed); ok {
			scale := headingScale(lvl)
			wrapped := r.wrapSpans(parseInline(title), scale, contentWidth)
			for i, ws := range wrapped {
				ln := vline{spans: ws, scale: scale}
				if i == 0 {
					ln.spaceBefore = pending + r.opts.FontSize*0.5
					pending = 0
				}
				out = append(out, ln)
			}
			pending = r.opts.FontSize * 0.3
			continue
		}

		if strings.HasPrefix(trimmed, ">") {
			body := strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))
			wrapped := r.wrapSpans(parseInline(body), 1, contentWidth-listIndent)
			for i, ws := range wrapped {
				ln := vline{spans: ws, scale: 1, quote: true, indent: listIndent}
				if i == 0 {
					ln.spaceBefore = pending
					pending = 0
				}
				out = append(out, ln)
			}
			continue
		}

		if marker, rest, depth, ok := parseList(trimmed); ok {
			indent := listIndent * float64(depth+1)
			wrapped := r.wrapSpans(parseInline(rest), 1, contentWidth-indent)
			if len(wrapped) == 0 {
				wrapped = [][]span{{{text: ""}}}
			}
			for i, ws := range wrapped {
				spans := ws
				if i == 0 {
					spans = append([]span{{text: marker}}, ws...)
				}
				ln := vline{spans: spans, scale: 1, indent: indent}
				if i == 0 {
					ln.spaceBefore = pending
					pending = 0
				}
				out = append(out, ln)
			}
			continue
		}

		wrapped := r.wrapSpans(parseInline(trimmed), 1, contentWidth)
		for i, ws := range wrapped {
			ln := vline{spans: ws, scale: 1}
			if i == 0 {
				ln.spaceBefore = pending
				pending = 0
			}
			out = append(out, ln)
		}
	}

	if inCode {
		flushCode()
	}
	return out
}

func headingScale(level int) float64 {
	switch level {
	case 1:
		return 1.65
	case 2:
		return 1.42
	case 3:
		return 1.24
	case 4:
		return 1.12
	case 5:
		return 1.04
	default:
		return 1.0
	}
}

func isHRule(s string) bool {
	if len(s) < 3 {
		return false
	}
	for _, ch := range []string{"-", "*", "_"} {
		if strings.Trim(s, ch) == "" && strings.Count(s, ch) >= 3 {
			return true
		}
	}
	return false
}

func parseHeading(s string) (int, string, bool) {
	level := 0
	for level < len(s) && level < 6 && s[level] == '#' {
		level++
	}
	if level == 0 || level >= len(s) || s[level] != ' ' {
		return 0, "", false
	}
	return level, strings.TrimSpace(s[level:]), true
}

// parseList 识别 "- / * / + / 1." 这类列表，返回标记、正文与缩进层级。
func parseList(s string) (string, string, int, bool) {
	depth := 0
	rest := s
	for {
		if strings.HasPrefix(rest, "  ") {
			depth++
			rest = rest[2:]
			continue
		}
		break
	}
	for _, prefix := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(rest, prefix) {
			return "• ", strings.TrimSpace(rest[len(prefix):]), depth, true
		}
	}
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	if i > 0 && i+1 < len(rest) && (rest[i] == '.' || rest[i] == ')') && rest[i+1] == ' ' {
		return rest[:i+1] + " ", strings.TrimSpace(rest[i+2:]), depth, true
	}
	return "", "", 0, false
}

// parseInline 处理 **粗体**、`行内代码`、[文字](链接)。
func parseInline(s string) []span {
	var spans []span
	var buf strings.Builder
	flush := func(bold, code bool) {
		if buf.Len() > 0 {
			spans = append(spans, span{text: buf.String(), bold: bold, code: code})
			buf.Reset()
		}
	}
	bold := false
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "**"):
			flush(bold, false)
			bold = !bold
			i += 2
		case s[i] == '`':
			flush(bold, false)
			end := strings.IndexByte(s[i+1:], '`')
			if end < 0 {
				buf.WriteByte(s[i])
				i++
				continue
			}
			spans = append(spans, span{text: s[i+1 : i+1+end], code: true})
			i += end + 2
		case s[i] == '[':
			closeIdx := strings.IndexByte(s[i:], ']')
			if closeIdx > 0 && i+closeIdx+1 < len(s) && s[i+closeIdx+1] == '(' {
				end := strings.IndexByte(s[i+closeIdx+1:], ')')
				if end > 0 {
					flush(bold, false)
					spans = append(spans, span{text: s[i+1 : i+closeIdx], link: true})
					i += closeIdx + 1 + end + 1
					continue
				}
			}
			buf.WriteByte(s[i])
			i++
		default:
			// 逐字符推进（按 rune 边界，避免切坏多字节字符）
			_, size := utf8.DecodeRuneInString(s[i:])
			buf.WriteString(s[i : i+size])
			i += size
		}
	}
	flush(bold, false)
	return spans
}

// styledRune 折行时用的"带样式的单字符"。
type styledRune struct {
	ch               rune
	bold, code, link bool
}

// wrapSpans 按宽度折行；CJK 逐字可断，西文尽量在空格处断。
func (r *Renderer) wrapSpans(spans []span, scale, maxWidth float64) [][]span {
	size := r.opts.FontSize * scale
	var runes []styledRune
	for _, sp := range spans {
		for _, ch := range sp.text {
			runes = append(runes, styledRune{ch: ch, bold: sp.bold, code: sp.code, link: sp.link})
		}
	}
	if len(runes) == 0 {
		return nil
	}

	var lines [][]span
	var cur []styledRune
	width := 0.0
	lastBreak := -1

	emit := func() {
		if len(cur) == 0 {
			return
		}
		// 行尾空格去掉
		for len(cur) > 0 && cur[len(cur)-1].ch == ' ' {
			cur = cur[:len(cur)-1]
		}
		if len(cur) > 0 {
			lines = append(lines, toSpans(cur))
		}
		cur = nil
		width = 0
		lastBreak = -1
	}

	for _, item := range runes {
		w := r.runeWidth(size, item.ch)
		if width+w > maxWidth && len(cur) > 0 {
			if lastBreak > 0 && lastBreak < len(cur) {
				rest := append([]styledRune(nil), cur[lastBreak:]...)
				cur = cur[:lastBreak]
				emit()
				for len(rest) > 0 && rest[0].ch == ' ' {
					rest = rest[1:]
				}
				cur = rest
				for _, x := range cur {
					width += r.runeWidth(size, x.ch)
				}
			} else {
				emit()
			}
		}
		cur = append(cur, item)
		width += w
		if item.ch == ' ' || isCJKBreakable(item.ch) {
			lastBreak = len(cur)
		}
	}
	emit()
	return lines
}

// toSpans 把带样式的字符序列合并成尽量少的 span（相邻同样式合并，少画几次）。
func toSpans(items []styledRune) []span {
	var out []span
	for _, it := range items {
		if n := len(out); n > 0 && out[n-1].bold == it.bold && out[n-1].code == it.code && out[n-1].link == it.link {
			out[n-1].text += string(it.ch)
			continue
		}
		out = append(out, span{text: string(it.ch), bold: it.bold, code: it.code, link: it.link})
	}
	return out
}

func isCJKBreakable(ch rune) bool {
	return unicode.Is(unicode.Han, ch) ||
		unicode.Is(unicode.Hiragana, ch) ||
		unicode.Is(unicode.Katakana, ch) ||
		unicode.Is(unicode.Hangul, ch) ||
		(ch >= 0x3000 && ch <= 0x303F) || // CJK 标点
		(ch >= 0xFF00 && ch <= 0xFFEF) // 全角
}
