// helpdoc-render —— 帮助文档图片化的**自部署渲染后端**。
//
// 契约（与 dice/help_image_render.go 一一对应）：
//
//	POST /render   {"text": "...", "format": "markdown", "width": 820, "reason": "censor"}
//	  → 200 image/png，响应体是图片字节
//	  → 4xx/5xx + 纯文本错误说明（骰子会把这段写进日志，然后回退成文本发送）
//	GET  /health   → 200 "ok"
//
// 设计取向是**低内存、零外部依赖**：
//   - 纯 Go 渲染（golang.org/x/image 的 font/opentype 直接画到 RGBA），
//     不需要浏览器、不需要无头 Chrome、不需要 node；
//   - 每次请求现场渲染，**不做任何缓存**（配合"一次性临时图片"的要求，
//     图片只存在于这一次响应里）；
//   - 常驻内存十几 MB 量级，渲染一张 820×1000 的图约需 3~5MB 临时内存。
//
// 用法见同目录 README.md。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// renderRequest 渲染请求体。字段与骰子侧 helpImageRenderRequest 对应。
type renderRequest struct {
	Text   string `json:"text"`
	Format string `json:"format"`
	Width  int    `json:"width"`
	Reason string `json:"reason"`
}

func main() {
	var (
		addr        = flag.String("addr", "127.0.0.1:3212", "监听地址")
		fontPath    = flag.String("font", "", "字体文件路径（.ttf/.otf/.ttc）；留空则自动探测常见中日韩字体")
		token       = flag.String("token", "", "可选鉴权令牌；设置后要求 Authorization: Bearer <token>")
		fontSize    = flag.Float64("font-size", 17, "正文字号（像素）")
		lineHeight  = flag.Float64("line-height", 1.65, "行高倍数")
		padding     = flag.Float64("padding", 26, "页面内边距（像素）")
		maxHeight   = flag.Int("max-height", 20000, "单张图片最大高度（像素），超过则截断并提示")
		maxTextLen  = flag.Int("max-text-chars", 20000, "正文最大字符数，超过则截断（防止一次渲染吃掉太多内存）")
		concurrency = flag.Int("concurrency", 2, "并发渲染上限（限制峰值内存）")
		showVersion = flag.Bool("version", false, "打印版本并退出")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("helpdoc-render 0.1.0")
		return
	}

	renderer, err := NewRenderer(RenderOptions{
		FontPath:    *fontPath,
		FontSize:    *fontSize,
		LineHeight:  *lineHeight,
		Padding:     *padding,
		MaxHeight:   *maxHeight,
		MaxTextRune: *maxTextLen,
	})
	if err != nil {
		log.Fatalf("初始化渲染器失败：%v", err)
	}
	log.Printf("字体：%s（%.1fpx，行高 %.2f）", renderer.FontName(), *fontSize, *lineHeight)

	sem := make(chan struct{}, max(1, *concurrency))

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("/render", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "只接受 POST /render", http.StatusMethodNotAllowed)
			return
		}
		if *token != "" && r.Header.Get("Authorization") != "Bearer "+*token {
			http.Error(w, "鉴权失败", http.StatusUnauthorized)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			http.Error(w, "读取请求体失败: "+err.Error(), http.StatusBadRequest)
			return
		}
		var req renderRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "请求体不是合法 JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.Text) == "" {
			http.Error(w, "text 为空", http.StatusBadRequest)
			return
		}

		sem <- struct{}{}
		png, err := renderer.RenderPNG(req.Text, req.Width)
		<-sem
		if err != nil {
			if errors.Is(err, errTextTooLong) {
				http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
				return
			}
			log.Printf("渲染失败（reason=%s, %d 字）：%v", req.Reason, utf8.RuneCountInString(req.Text), err)
			http.Error(w, "渲染失败: "+err.Error(), http.StatusInternalServerError)
			return
		}

		log.Printf("已渲染：%d 字 → %d 字节（reason=%s, width=%d）",
			utf8.RuneCountInString(req.Text), len(png), req.Reason, req.Width)

		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store") // 明确告诉任何中间层：别缓存
		w.Header().Set("Content-Length", fmt.Sprint(len(png)))
		_, _ = w.Write(png)
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("helpdoc-render 监听 http://%s（POST /render）", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("服务退出：%v", err)
	}
	_ = os.Stdout.Sync()
}
