package dice

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

// 本文件实现「帮助文档图片化」：帮助正文**命中敏感词**或**超过设定长度**时，
// 把正文交给骰主自部署的渲染后端出图，再以 [CQ:image] 发送。
//
// 四条设计要点：
//  1. 只在需要时转图片：命中敏感词 / 超过 ImageRenderMinLength；平时仍是普通文本
//     —— 图片不能复制、不能搜索，而且每次渲染都有成本。
//  2. 出图**不缓存**：每次请求现场渲染，并用 base64 内联发送，图片不会留在任何
//     可被复用的 URL 上（"一次性临时用"）。
//  3. 任何失败（没配地址、超时、非 200、返回不是图片、图太大）都**回退成文本**，
//     渲染服务挂掉不会让帮助功能不可用。
//  4. 打码走纯匹配路径（CensorMaskOutgoing）：**不计数、不拉黑** ——
//     帮助文档是骰主主动公开的内容，不该把骰主自己送进黑名单。

// helpImageRenderMaxBytes 单张图片大小上限，防止后端返回一个巨大文件把消息撑爆。
const helpImageRenderMaxBytes = 8 << 20 // 8MB

// helpReplyPlan 决定这次帮助正文怎么发。
//
// 抽成纯函数是为了能单测：策略本身（什么时候转图片、图片里放原文还是打码后的文本）
// 不该埋在发送流程里。
//
//   - useImage：是否转图片。命中敏感词、或正文超过 ImageRenderMinLength 时为 true。
//   - maskInImage：图片里是否用打码后的文本（默认 false = 图片里保留原文，
//     因为"命中敏感词还转图片"本来就是为了让内容送达）。
//   - hit：是否命中敏感词（决定"渲染失败时"要不要用打码文本兜底 —— 命中时一定要打码）。
type helpReplyPlan struct {
	useImage    bool
	maskInImage bool
	hit         bool
	tooLong     bool
}

func planHelpReply(cfg *HelpConfig, hitWords []string, runeLen int) helpReplyPlan {
	plan := helpReplyPlan{hit: len(hitWords) > 0}
	if cfg == nil {
		return plan
	}
	plan.tooLong = cfg.ImageRenderMinLength > 0 && runeLen >= cfg.ImageRenderMinLength
	plan.useImage = cfg.ImageRenderEnable && (plan.hit || plan.tooLong)
	plan.maskInImage = plan.hit && cfg.ImageRenderMaskInImage
	return plan
}

// replyHelpText 发送帮助文档正文：需要时打码、需要时转图片，最后才回落成文本。
func replyHelpText(ctx *MsgContext, msg *Message, text string) {
	if ctx == nil || msg == nil || ctx.Dice == nil || text == "" {
		return
	}
	d := ctx.Dice

	masked, _ := d.CensorMaskOutgoing(text)
	plan := planHelpReply(helpConfigOf(d), d.CensorHitWords(text), utf8.RuneCountInString(text))
	if !plan.useImage {
		ReplyToSender(ctx, msg, masked)
		return
	}

	cfg := helpConfigOf(d)
	src := text
	if plan.maskInImage {
		src = masked
	}

	img, err := renderHelpDocImage(cfg, src, plan.tooLong && !plan.hit)
	if err != nil {
		d.Logger.Warnf("帮助文档图片渲染失败，已回退为文本：%v", err)
		ReplyToSender(ctx, msg, masked)
		return
	}
	d.Logger.Infof("帮助文档已渲染为图片：%d 字节，命中敏感词=%v，过长=%v", len(img), plan.hit, plan.tooLong)
	ReplyToSender(ctx, msg, "[CQ:image,file=base64://"+base64.StdEncoding.EncodeToString(img)+"]")
}

// helpConfigOf 取当前骰子的帮助文档配置（Help 管理器挂在 DiceManager 上）。
func helpConfigOf(d *Dice) *HelpConfig {
	if d == nil || d.Parent == nil || d.Parent.Help == nil {
		return nil
	}
	return d.Parent.Help.Config
}

// helpImageRenderRequest 渲染后端的请求体（契约见 tools/helpdoc-render/README.md）。
type helpImageRenderRequest struct {
	Text   string `json:"text"`
	Format string `json:"format"`
	Width  int    `json:"width"`
	Reason string `json:"reason,omitempty"` // "censor" | "toolong"，仅供后端记日志
}

// renderHelpDocImage 调自部署后端，把文本渲染成 PNG 字节。
func renderHelpDocImage(cfg *HelpConfig, text string, tooLong bool) ([]byte, error) {
	if cfg == nil || strings.TrimSpace(cfg.ImageRenderURL) == "" {
		return nil, errors.New("没有配置渲染后端地址")
	}

	reason := "censor"
	if tooLong {
		reason = "toolong"
	}
	body, err := json.Marshal(helpImageRenderRequest{
		Text:   text,
		Format: "markdown",
		Width:  cfg.imageRenderWidth(),
		Reason: reason,
	})
	if err != nil {
		return nil, err
	}

	reqCtx, cancel := context.WithTimeout(context.Background(), cfg.imageRenderTimeout())
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, cfg.ImageRenderURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token := strings.TrimSpace(cfg.ImageRenderToken); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := helpImageHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("渲染后端返回 %d：%s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "image/") {
		return nil, fmt.Errorf("渲染后端返回的不是图片（Content-Type: %s）", ct)
	}

	img, err := io.ReadAll(io.LimitReader(resp.Body, helpImageRenderMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(img) == 0 {
		return nil, errors.New("渲染后端返回了空响应")
	}
	if len(img) > helpImageRenderMaxBytes {
		return nil, fmt.Errorf("图片超过 %dMB 上限", helpImageRenderMaxBytes>>20)
	}
	return img, nil
}

// helpImageHTTPClient 渲染专用 client：不跟随重定向（避免把文本转发到别处）。
var helpImageHTTPClient = &http.Client{
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}
