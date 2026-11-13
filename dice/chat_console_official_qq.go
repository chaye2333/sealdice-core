package dice

// 官方 QQ 侧的聊天控制台钩子：入站采集、出站采集、群信息补全、骰主发送。
// 平台适配器只在这里跟控制台耦合，避免把采集逻辑散落到 3000 行的适配器里。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	qqconstant "github.com/sealdice/botgo/constant"
	"github.com/sealdice/botgo/dto"

	"sealdice-core/message"
)

const (
	// 聊天控制台群信息拉取节流：官方接口 30 QPM，且群名基本不变，没必要频繁查。
	chatConsoleInfoThrottle = 10 * time.Minute
	// 控制台单条消息长度上限（官方文本消息限制在 1500 字以内比较安全）。
	chatConsoleSendMaxLen  = 1500
	chatConsoleSendTimeout = 35 * time.Second
	// 控制台上传富媒体的大小上限与超时（大文件上传比文本慢得多）。
	chatConsoleMediaMaxBytes = 30 << 20
	chatConsoleMediaTimeout  = 180 * time.Second
	chatConsoleRecallTimeout = 20 * time.Second
)

// 官方富媒体 file_type 取值（1=图片、2=视频、3=语音、4=文件）。
const (
	officialQQFileTypeImage  = 1
	officialQQFileTypeVideo  = 2
	officialQQFileTypeRecord = 3
)

func chatConsoleGroupKey(groupOpenID string) string {
	return chatConsoleKindGroup + ":" + groupOpenID
}

// chatConsoleStore 返回所属 Dice 的聊天控制台存储；测试或未初始化时返回 nil。
func (pa *PlatformAdapterOfficialQQ) chatConsoleStore() *ChatConsoleStore {
	if pa == nil || pa.EndPoint == nil || pa.EndPoint.Session == nil || pa.EndPoint.Session.Parent == nil {
		return nil
	}
	return pa.EndPoint.Session.Parent.GetChatConsole()
}

func (pa *PlatformAdapterOfficialQQ) chatConsoleLogf(format string, args ...interface{}) {
	if pa != nil && pa.EndPoint != nil && pa.EndPoint.Session != nil && pa.EndPoint.Session.Parent != nil {
		if log := pa.EndPoint.Session.Parent.Logger; log != nil {
			log.Debugf(format, args...)
		}
	}
}

// recordChatConsoleInbound 记录一条收到的官方消息。full 表示该群已授权全量消息。
//
// payload 是官方事件体（*dto.Message 的别名类型），用来补 CQ 码拿不到的信息：
// message_type（卡片 / 引用 / 聊天记录）、ark_data、msg_elements 以及附件元数据。
func (pa *PlatformAdapterOfficialQQ) recordChatConsoleInbound(
	msg *Message, payload *dto.Message, kind, targetID, senderOpenID string, full bool, event *dto.WSPayload,
) {
	store := pa.chatConsoleStore()
	if store == nil || msg == nil || targetID == "" {
		return
	}
	rawID, _ := msg.RawID.(string)
	_, err := store.Append(ChatConsoleRecord{
		Kind:         kind,
		TargetID:     targetID,
		UIN:          pa.UIN,
		EndpointID:   pa.EndPoint.ID,
		AppID:        pa.AppID,
		SenderID:     msg.Sender.UserID,
		SenderName:   msg.Sender.Nickname,
		SenderOpenID: senderOpenID,
		Dir:          chatConsoleDirIn,
		Source:       chatConsoleSourceUser,
		Text:         chatConsoleInboundText(msg.Message, payload, event),
		Media:        chatConsoleInboundMedia(payload, event),
		Quote:        chatConsoleInboundQuote(payload),
		MessageID:    rawID,
		TS:           msg.Time,
		FullMessage:  full,
	})
	if err != nil {
		pa.chatConsoleLogf("official qq 聊天控制台记录消息失败: %v", err)
		return
	}
	if kind == chatConsoleKindGroup {
		// 群名未知时顺带补一下，失败了也不影响消息记录。
		pa.refreshChatConsoleGroupInfo(targetID)
	}
}

// 官方消息的 message_type 取值（见开放平台 C2C / GROUP_MESSAGE_CREATE 文档）。
const (
	chatConsoleMsgTypeText   = 0
	chatConsoleMsgTypeArk    = 3
	chatConsoleMsgTypeParts  = 101
	chatConsoleMsgTypeRecord = 102
	chatConsoleMsgTypeQuote  = 103
)

// chatConsoleInboundText 把官方消息补成"人能看懂"的样子：
// 纯文本原样；ARK 卡片补标题/来源/链接；聊天记录把 msg_elements 拼进来；
// 语音有 ASR 结果时补一行转写。
//
// base 里可能带着附件 CQ 码（dice 侧需要），这里只追加，不删改。
func chatConsoleInboundText(base string, payload *dto.Message, event *dto.WSPayload) string {
	text := strings.TrimSpace(base)
	if payload == nil {
		return text
	}

	var blocks []string
	if payload.MessageType == chatConsoleMsgTypeArk {
		if card := chatConsoleArkText(payload.ARKData, text); card != "" {
			blocks = append(blocks, card)
		}
	}
	if len(payload.MsgElements) > 0 && payload.MessageType != chatConsoleMsgTypeQuote {
		var parts []string
		for _, el := range payload.MsgElements {
			if el == nil {
				continue
			}
			if content := strings.TrimSpace(el.Content); content != "" {
				parts = append(parts, content)
			}
		}
		if len(parts) > 0 {
			head := "[聊天记录]"
			if payload.MessageType == chatConsoleMsgTypeParts {
				head = "[合并消息]"
			}
			blocks = append(blocks, head+"\n"+strings.Join(parts, "\n\n"))
		}
	}
	if asr := chatConsoleASRText(event); asr != "" && !strings.Contains(text, asr) {
		blocks = append(blocks, "[语音转文字] "+asr)
	}

	if len(blocks) == 0 {
		return text
	}
	if text == "" {
		return strings.Join(blocks, "\n")
	}
	return text + "\n" + strings.Join(blocks, "\n")
}

// chatConsoleArkText 把结构化卡片渲染成几行可读文本；已出现在正文里的值不重复追加。
func chatConsoleArkText(ark *dto.ARKData, existing string) string {
	if ark == nil {
		return ""
	}
	name := strings.TrimSpace(ark.ArkName)
	if name == "" {
		name = strings.TrimSpace(ark.ArkType)
	}
	if name == "" {
		name = "卡片"
	}

	lines := []string{"[卡片消息] " + name}
	appendLine := func(label, value string) {
		value = strings.TrimSpace(value)
		if value == "" || strings.Contains(existing, value) {
			return
		}
		for _, line := range lines {
			if strings.Contains(line, value) {
				return
			}
		}
		lines = append(lines, label+value)
	}

	prompt := strings.TrimSpace(ark.Prompt)
	appendLine("摘要: ", prompt)
	if title := strings.TrimSpace(ark.Fields["title"]); title != prompt {
		appendLine("标题: ", title)
	}
	appendLine("说明: ", ark.Fields["desc"])
	appendLine("来源: ", ark.Fields["source"])
	appendLine("链接: ", ark.Fields["jump_url"])

	if len(lines) == 1 {
		return "" // 只有类型名，没有实际内容就别刷屏
	}
	return strings.Join(lines, "\n")
}

// chatConsoleInboundQuote 还原引用消息（message_type=103）的被引用内容。
func chatConsoleInboundQuote(payload *dto.Message) *ChatConsoleQuote {
	if payload == nil || payload.MessageType != chatConsoleMsgTypeQuote {
		return nil
	}
	el := payload.QuotedElement()
	if el == nil {
		return nil
	}
	text := strings.TrimSpace(el.Content)
	if text == "" && len(el.Attachments) > 0 {
		text = "（附件）"
	}
	if text == "" {
		return nil
	}
	return &ChatConsoleQuote{
		Sender: "被引用消息",
		Text:   text,
		MsgID:  payload.MessageScene.RefMsgIdx(),
	}
}

// chatConsoleInboundMedia 把附件映射成控制台展示用的媒体列表。
// 语音优先用官方转码后的 WAV 地址（原始 file 是 SILK，浏览器放不出来）。
func chatConsoleInboundMedia(payload *dto.Message, event *dto.WSPayload) []ChatConsoleMedia {
	if payload == nil || len(payload.Attachments) == 0 {
		return nil
	}
	raw := chatConsoleRawAttachments(event)
	rawByName := make(map[string]chatConsoleRawAttachment, len(raw))
	for _, item := range raw {
		if item.FileName != "" {
			rawByName[item.FileName] = item
		}
	}
	rawByIndex := len(raw) == len(payload.Attachments)
	out := make([]ChatConsoleMedia, 0, len(payload.Attachments))
	for i, item := range payload.Attachments {
		if item == nil || item.URL == "" {
			continue
		}
		kind := chatConsoleAttachmentKind(item.ContentType)
		url := item.URL
		if kind == "record" {
			// 优先按文件名对齐（事件体里可能只带语音那一条），其次按下标
			if voice, ok := rawByName[item.FileName]; ok && voice.VoiceWAVURL != "" {
				url = voice.VoiceWAVURL
			} else if rawByIndex && raw[i].VoiceWAVURL != "" {
				url = raw[i].VoiceWAVURL
			}
		}
		out = append(out, ChatConsoleMedia{Kind: kind, URL: url, Name: item.FileName})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func chatConsoleAttachmentKind(contentType string) string {
	switch {
	case strings.HasPrefix(contentType, "image/"):
		return "image"
	case strings.HasPrefix(contentType, "voice"):
		return "record"
	case strings.HasPrefix(contentType, "video/"):
		return "video"
	default:
		return "file"
	}
}

// chatConsoleRawAttachment 是 SDK 没暴露的两个字段：转码后的语音地址与 ASR 结果。
type chatConsoleRawAttachment struct {
	FileName     string `json:"filename"`
	VoiceWAVURL  string `json:"voice_wav_url"`
	ASRReferText string `json:"asr_refer_text"`
}

// chatConsoleRawAttachments 从原始事件体里取附件扩展字段（webhook 用 data，ws 用 d）。
func chatConsoleRawAttachments(event *dto.WSPayload) []chatConsoleRawAttachment {
	if event == nil || len(event.RawMessage) == 0 {
		return nil
	}
	type attachmentsHolder struct {
		Attachments []chatConsoleRawAttachment `json:"attachments"`
	}
	var wrapper struct {
		D    attachmentsHolder `json:"d"`
		Data attachmentsHolder `json:"data"`
	}
	if err := json.Unmarshal(event.RawMessage, &wrapper); err != nil {
		return nil
	}
	if len(wrapper.Data.Attachments) > 0 {
		return wrapper.Data.Attachments
	}
	return wrapper.D.Attachments
}

func chatConsoleASRText(event *dto.WSPayload) string {
	for _, item := range chatConsoleRawAttachments(event) {
		if text := strings.TrimSpace(item.ASRReferText); text != "" {
			return text
		}
	}
	return ""
}

// recordChatConsoleOutbound 记录一条骰子实际发出的消息（在 API 发送成功之后调用）。
func (pa *PlatformAdapterOfficialQQ) recordChatConsoleOutbound(kind, targetID string, toCreate *dto.MessageToCreate, res *dto.Message) {
	store := pa.chatConsoleStore()
	if store == nil || targetID == "" {
		return
	}
	text := chatConsoleOutboundText(toCreate)
	if text == "" {
		return
	}
	msgID := ""
	if res != nil {
		msgID = res.ID
	}
	_, err := store.Append(ChatConsoleRecord{
		Kind:       kind,
		TargetID:   targetID,
		UIN:        pa.UIN,
		EndpointID: pa.EndPoint.ID,
		AppID:      pa.AppID,
		SenderID:   pa.EndPoint.UserID,
		SenderName: pa.EndPoint.Nickname,
		Dir:        chatConsoleDirOut,
		Source:     chatConsoleSourceBot,
		Text:       text,
		MessageID:  msgID,
		TS:         time.Now().Unix(),
	})
	if err != nil {
		pa.chatConsoleLogf("official qq 聊天控制台记录发送消息失败: %v", err)
	}
}

func chatConsoleOutboundText(toCreate *dto.MessageToCreate) string {
	if toCreate == nil {
		return ""
	}
	if toCreate.Content != "" {
		return toCreate.Content
	}
	if toCreate.Markdown != nil && toCreate.Markdown.Content != "" {
		return toCreate.Markdown.Content
	}
	if toCreate.Media != nil {
		// 富媒体消息没有正文，只记一个占位，避免历史里出现空条目。
		if toCreate.MsgType == dto.RichMediaMsg {
			return "[富媒体消息]"
		}
	}
	return ""
}

// refreshChatConsoleGroupInfo 在后台拉取群名 / 成员数（受节流保护）。
func (pa *PlatformAdapterOfficialQQ) refreshChatConsoleGroupInfo(groupOpenID string) {
	store := pa.chatConsoleStore()
	if store == nil || groupOpenID == "" {
		return
	}
	key := chatConsoleGroupKey(groupOpenID)
	conv := store.Conversation(key)
	if conv != nil {
		if conv.NameManual || conv.Name != chatConsoleDefaultName(chatConsoleKindGroup, groupOpenID) {
			return
		}
	}
	if !pa.allowChatConsoleInfoFetch(groupOpenID) {
		return
	}
	go func() {
		info, err := pa.fetchChatConsoleGroupInfo(groupOpenID)
		if err != nil {
			pa.chatConsoleLogf("official qq 获取群信息失败(%s): %v", groupOpenID, err)
			return
		}
		store.SetGroupInfo(key, *info)
		// 顺手把 dice 层的群名也补上，群组管理页就不会显示「未获取到」
		if info.Name != "" && pa.EndPoint != nil && pa.EndPoint.Session != nil {
			setOfficialQQGroupName(pa.EndPoint.Session.Parent,
				formatDiceIDOfficialQQGroupOpenID(pa.UIN, groupOpenID), info.Name)
		}
	}()
}

func (pa *PlatformAdapterOfficialQQ) allowChatConsoleInfoFetch(groupOpenID string) bool {
	pa.consoleInfoMu.Lock()
	defer pa.consoleInfoMu.Unlock()
	if pa.consoleInfoAt == nil {
		pa.consoleInfoAt = map[string]time.Time{}
	}
	now := time.Now()
	if at, ok := pa.consoleInfoAt[groupOpenID]; ok && now.Sub(at) < chatConsoleInfoThrottle {
		return false
	}
	pa.consoleInfoAt[groupOpenID] = now
	return true
}

// fetchChatConsoleGroupInfo 通过 OpenAPI 透传接口查询群信息（SDK 没封装 /v2/groups/{id}/info）。
// 该接口是白名单机器人才能用的（错误码 11253），拿不到时不报错、只是没有简介/标签这些字段。
func (pa *PlatformAdapterOfficialQQ) fetchChatConsoleGroupInfo(groupOpenID string) (*ChatConsoleGroupInfo, error) {
	if pa.Api == nil {
		return nil, errors.New("官方 QQ 机器人尚未连接")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	body, err := pa.Api.Transport(ctx, http.MethodGet,
		qqconstant.APIDomain+"/v2/groups/"+url.PathEscape(groupOpenID)+"/info", nil)
	if err != nil {
		return nil, err
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	info := &ChatConsoleGroupInfo{
		Name:        chatConsoleFirstString(payload, "group_name", "name"),
		MemberCount: chatConsoleFirstInt(payload, "group_member_num", "member_count", "group_member_count"),
		FingerMemo:  chatConsoleFirstString(payload, "group_finger_memo", "finger_memo"),
		ClassText:   chatConsoleFirstString(payload, "group_class_text", "class_text"),
	}
	if rawTags, ok := payload["group_tags"].([]interface{}); ok {
		for _, item := range rawTags {
			if tag, ok := item.(string); ok && strings.TrimSpace(tag) != "" {
				info.Tags = append(info.Tags, strings.TrimSpace(tag))
			}
		}
	}
	if info.Name == "" && info.MemberCount == 0 && info.FingerMemo == "" && info.ClassText == "" && len(info.Tags) == 0 {
		return nil, errors.New("群信息接口没有返回可用字段")
	}
	return info, nil
}

// chatConsoleManualInfoInterval 手动刷新的最小间隔：前端连点 / 多人同时点时不至于打满官方配额（30 QPM）。
const chatConsoleManualInfoInterval = 3 * time.Second

// RefreshChatConsoleGroupInfo 立刻向官方接口拉一次群资料（骰主在控制台点「刷新」时用）。
// 和后台的节流版本不同：这是用户显式触发，不吃 chatConsoleInfoThrottle 的 10 分钟限制，
// 只挡 3 秒内的重复请求，避免前端连点把官方配额打满。
func RefreshChatConsoleGroupInfo(d *Dice, key string) (*ChatConsoleConversation, error) {
	if d == nil {
		return nil, errors.New("骰子还没初始化")
	}
	store := d.GetChatConsole()
	if store == nil {
		return nil, errors.New("聊天控制台未启用")
	}
	conv := store.Conversation(key)
	if conv == nil {
		return nil, errors.New("会话不存在")
	}
	if conv.Kind != chatConsoleKindGroup {
		return nil, errors.New("只有群聊才能查群资料")
	}
	var pa *PlatformAdapterOfficialQQ
	if d.ImSession != nil {
		for _, ep := range d.ImSession.EndPoints {
			if ep == nil || ep.Adapter == nil {
				continue
			}
			p, ok := ep.Adapter.(*PlatformAdapterOfficialQQ)
			if !ok || p == nil {
				continue
			}
			if conv.EndpointID != "" && ep.ID == conv.EndpointID {
				pa = p
				break
			}
			if conv.UIN != "" && p.UIN == conv.UIN {
				pa = p
			}
		}
	}
	if pa == nil {
		return nil, errors.New("找不到对应的官方 QQ 机器人，只有官方 QQ 群能查群资料")
	}
	// 3 秒内刚查过就直接返回缓存，别把官方配额打满
	pa.consoleInfoMu.Lock()
	last, hasLast := pa.consoleInfoAt[conv.TargetID]
	tooSoon := hasLast && time.Since(last) < chatConsoleManualInfoInterval
	pa.consoleInfoMu.Unlock()
	if tooSoon {
		return conv, nil
	}
	info, err := pa.fetchChatConsoleGroupInfo(conv.TargetID)
	if err != nil {
		return nil, err
	}
	pa.consoleInfoMu.Lock()
	if pa.consoleInfoAt == nil {
		pa.consoleInfoAt = map[string]time.Time{}
	}
	pa.consoleInfoAt[conv.TargetID] = time.Now()
	pa.consoleInfoMu.Unlock()

	store.SetGroupInfo(key, *info)
	if info.Name != "" {
		setOfficialQQGroupName(d, formatDiceIDOfficialQQGroupOpenID(pa.UIN, conv.TargetID), info.Name)
	}
	if updated := store.Conversation(key); updated != nil {
		return updated, nil
	}
	return conv, nil
}

func chatConsoleFirstString(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func chatConsoleFirstInt(m map[string]interface{}, keys ...string) int {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			if v > 0 {
				return int(v)
			}
		case int:
			if v > 0 {
				return v
			}
		}
	}
	return 0
}

// ChatConsoleMention 控制台发送时的 @ 目标。
type ChatConsoleMention struct {
	OpenID string `json:"openid"`
	Name   string `json:"name"`
}

// ChatConsoleSendOptions 控制台发送选项：伪引用 + @ 群成员。
type ChatConsoleSendOptions struct {
	Quote    *ChatConsoleQuote
	Mentions []ChatConsoleMention
}

// platformText 是真正发到 QQ 上的正文：引用前缀 + @ 标签 + 用户输入。
func (opts ChatConsoleSendOptions) platformText(kind, text string) string {
	var b strings.Builder
	if opts.Quote != nil {
		b.WriteString(chatConsoleQuotePrefix(*opts.Quote))
	}
	if kind == chatConsoleKindGroup {
		for _, m := range opts.Mentions {
			if m.OpenID == "" {
				continue
			}
			fmt.Fprintf(&b, `<qqbot-at-user id="%s" />`, m.OpenID)
		}
	}
	b.WriteString(text)
	return b.String()
}

// displayText 是控制台历史里展示的正文：引用块单独渲染，正文只带 @昵称。
func (opts ChatConsoleSendOptions) displayText(kind, text string) string {
	var b strings.Builder
	if kind == chatConsoleKindGroup {
		for _, m := range opts.Mentions {
			if m.Name == "" {
				continue
			}
			b.WriteString("@" + m.Name + " ")
		}
	}
	b.WriteString(text)
	return strings.TrimSpace(b.String())
}

// chatConsoleQuotePrefix 官方主动消息挂不了 msg_id，只能在正文里带上引用片段。
func chatConsoleQuotePrefix(q ChatConsoleQuote) string {
	quoted := []rune(strings.TrimSpace(q.Text))
	if len(quoted) > 40 {
		quoted = append(quoted[:40], '…')
	}
	sender := strings.TrimSpace(q.Sender)
	if sender == "" {
		sender = "对方"
	}
	return fmt.Sprintf("「回复 %s：%s」\n", sender, string(quoted))
}

// chatConsoleKey 会话在控制台存储里的 key。
func chatConsoleKey(kind, targetID string) string {
	return kind + ":" + targetID
}

// chatConsoleQuotePassiveWindow 官方被动回复窗口。
// 只有这个时长以内收到的消息才能挂 msg_id 做"真引用"；更早的消息只能退回正文前缀那种伪引用，
// 因为官方不允许主动消息带 message_reference。
const chatConsoleQuotePassiveWindow = 4 * time.Minute

// chatConsoleMediaCQRe 控制台文本里不能直接塞富媒体：官方富媒体要单独上传接口，
// 而且 message.ConvertStringMessage 会把无效的媒体码静默丢掉，所以这里先拦一次给个明确的提示。
var chatConsoleMediaCQRe = regexp.MustCompile(`\[CQ:(?:image|record|video|file)[,\]]`)

// chatConsoleParseCQ 把控制台输入里的 CQ 码翻成官方 QQ 认的形态：
//
//	[CQ:at,qq=<openid>]      → <qqbot-at-user id="<openid>" />（群聊）
//	[CQ:at,qq=all]           → <qqbot-at-everyone />（群聊）
//	[CQ:reply,id=<msg_id>]   → 记为被动回复目标，正文里去掉
//
// 图片 / 语音 / 文件这类 CQ 码在控制台没有对应上传通道，直接报错，
// 免得骰主以为已经发出去了。返回正文、被动回复的 msg_id。
func (pa *PlatformAdapterOfficialQQ) chatConsoleParseCQ(kind, text string) (string, string, error) {
	if chatConsoleMediaCQRe.MatchString(text) {
		return "", "", errors.New("控制台输入不支持图片 / 语音 / 文件的 CQ 码，请用输入框旁的上传按钮发送")
	}
	var (
		b       strings.Builder
		replyID string
	)
	for _, element := range message.ConvertStringMessage(text) {
		switch elem := element.(type) {
		case *message.TextElement:
			b.WriteString(elem.Content)
		case *message.ReplyElement:
			if replyID == "" {
				replyID = strings.TrimSpace(elem.ReplySeq)
			}
		case *message.AtElement:
			if kind != chatConsoleKindGroup {
				continue // 私聊没有 @
			}
			target := normalizeOfficialQQGroupAtTarget(pa.UIN, elem.Target)
			switch target {
			case "":
				continue
			case "all":
				b.WriteString("<qqbot-at-everyone />")
			default:
				b.WriteString(formatOfficialQQAtUser(target))
			}
		case *message.ImageElement, *message.RecordElement, *message.FileElement:
			return "", "", errors.New("控制台输入不支持图片 / 语音 / 文件的 CQ 码，请用输入框旁的上传按钮发送")
		}
	}
	return b.String(), replyID, nil
}

// chatConsoleQuoteFromMsgID 用 msg_id 反查一条引用信息（查不到就只留 id）。
func (pa *PlatformAdapterOfficialQQ) chatConsoleQuoteFromMsgID(kind, targetID, msgID string) *ChatConsoleQuote {
	quote := &ChatConsoleQuote{MsgID: msgID}
	store := pa.chatConsoleStore()
	if store == nil {
		return quote
	}
	if msg := store.Find(chatConsoleKey(kind, targetID), msgID); msg != nil {
		quote.Sender = msg.SenderName
		quote.Text = msg.Text
	}
	return quote
}

// chatConsoleResolveQuote 决定 opts.Quote 能不能做成真引用。
// 返回（展示 / 记录用的引用信息, 可用的被动回复 msg_id）。
func (pa *PlatformAdapterOfficialQQ) chatConsoleResolveQuote(kind, targetID string, quote *ChatConsoleQuote) (*ChatConsoleQuote, string) {
	if quote == nil {
		return nil, ""
	}
	want := strings.TrimSpace(quote.MsgID)
	if want == "" {
		return quote, ""
	}
	store := pa.chatConsoleStore()
	if store == nil {
		return quote, ""
	}
	msg := store.Find(chatConsoleKey(kind, targetID), want)
	if msg == nil || msg.Dir != chatConsoleDirIn || msg.MsgID == "" {
		return quote, ""
	}
	if time.Since(time.Unix(msg.TS, 0)) > chatConsoleQuotePassiveWindow {
		return quote, ""
	}
	out := *quote
	out.MsgID = msg.MsgID
	if strings.TrimSpace(out.Sender) == "" {
		out.Sender = msg.SenderName
	}
	if strings.TrimSpace(out.Text) == "" {
		out.Text = msg.Text
	}
	return &out, msg.MsgID
}

// chatConsoleAtDisplayName 把 openid 换回昵称，取最近发言过的那个人。
func (pa *PlatformAdapterOfficialQQ) chatConsoleAtDisplayName(kind, targetID, openid string) string {
	store := pa.chatConsoleStore()
	if store != nil {
		if msgs, _, err := store.Messages(chatConsoleKey(kind, targetID), 0, 200); err == nil {
			for i := len(msgs) - 1; i >= 0; i-- {
				if msgs[i].SenderOpenID == openid && msgs[i].SenderName != "" {
					return msgs[i].SenderName
				}
			}
		}
	}
	return openid
}

// chatConsoleDisplayText 把正文里的官方 @ 标签换回 `@昵称`，历史里不该出现标签原文。
func (pa *PlatformAdapterOfficialQQ) chatConsoleDisplayText(kind, targetID, text string) string {
	if !strings.Contains(text, "qqbot-at") {
		return text
	}
	text = strings.ReplaceAll(text, "<qqbot-at-everyone />", "@全体成员")
	return officialQQAtRegex.ReplaceAllStringFunc(text, func(tag string) string {
		target := officialQQMentionTarget(officialQQAtRegex.FindStringSubmatch(tag))
		if target == "" {
			return tag
		}
		return "@" + pa.chatConsoleAtDisplayName(kind, targetID, target)
	})
}

// chatConsoleSendPayload 组装好的待发消息 + 历史展示文本 + 不支持 markdown 时的退化正文。
type chatConsoleSendPayload struct {
	toCreate    *dto.MessageToCreate
	displayText string // 历史里展示的正文（@ 标签已还原成 @昵称）
	plainText   string // markdown 发不出去时的退化正文（@ 变纯文本 @昵称）
	quote       *ChatConsoleQuote
}

// content 返回真正发出去的正文（markdown 模式下正文在 Markdown.Content 里）。
func (p *chatConsoleSendPayload) content() string {
	if p == nil || p.toCreate == nil {
		return ""
	}
	if p.toCreate.Markdown != nil {
		return p.toCreate.Markdown.Content
	}
	return p.toCreate.Content
}

// chatConsoleUseMarkdown 控制台消息是否走 markdown（msg_type=2）。
// `<qqbot-at-user>` / `<qqbot-at-everyone>` 只有在 markdown 消息里才会被 QQ 解析，
// 纯文本会把它原样显示出来（“<qqbot-at-user id="..." />耶耶耶”就是这么来的），
// 所以带 @ 的消息必须走 markdown；其余情况跟随「官方QQ使用Markdown」设置，和骰子本体保持一致。
func (pa *PlatformAdapterOfficialQQ) chatConsoleUseMarkdown(content string) bool {
	if strings.Contains(content, "qqbot-at-") {
		return true
	}
	if pa == nil || pa.EndPoint == nil || pa.EndPoint.Session == nil || pa.EndPoint.Session.Parent == nil {
		return false
	}
	return pa.EndPoint.Session.Parent.Config.OfficialQQUseMarkdown
}

// chatConsoleBuildSend 组装一条待发送的官方消息（不触发网络请求，便于单测）。
func (pa *PlatformAdapterOfficialQQ) chatConsoleBuildSend(kind, targetID, text string, opts ChatConsoleSendOptions) (*chatConsoleSendPayload, error) {
	rendered, cqReplyID, err := pa.chatConsoleParseCQ(kind, text)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(rendered) == "" && len(opts.Mentions) == 0 {
		return nil, errors.New("消息不能为空")
	}
	if runes := []rune(rendered); len(runes) > chatConsoleSendMaxLen {
		return nil, fmt.Errorf("消息过长（%d 字），单条请控制在 %d 字以内", len(runes), chatConsoleSendMaxLen)
	}

	// 界面上的「回复」：只有最近收到、还在被动回复窗口内的消息才能挂 msg_id
	quote, replyID := pa.chatConsoleResolveQuote(kind, targetID, opts.Quote)
	if cqReplyID != "" {
		replyID = cqReplyID
		quote = pa.chatConsoleQuoteFromMsgID(kind, targetID, cqReplyID)
	}

	sendOpts := opts
	if replyID != "" {
		sendOpts.Quote = nil // 真引用由 message_reference 表达，正文里不再加「回复 某人」前缀
	}

	content := sendOpts.platformText(kind, rendered)
	displayText := pa.chatConsoleDisplayText(kind, targetID, sendOpts.displayText(kind, rendered))

	toCreate := &dto.MessageToCreate{
		MsgType: dto.TextMsg,
		MsgSeq:  rand.Uint32()%10000000 + 1, //nolint:gosec // 仅用于消息去重序号
	}
	if pa.chatConsoleUseMarkdown(content) {
		toCreate.MsgType = 2 // markdown 消息
		toCreate.Markdown = &dto.Markdown{Content: content}
	} else {
		toCreate.Content = content
	}
	if replyID != "" {
		toCreate.MsgID = replyID
		toCreate.MessageReference = &dto.MessageReference{
			MessageID:             replyID,
			IgnoreGetMessageError: true,
		}
	}

	return &chatConsoleSendPayload{
		toCreate:    toCreate,
		displayText: displayText,
		plainText:   displayText,
		quote:       quote,
	}, nil
}

// SendChatConsoleMessage 以机器人身份向会话发送一条文字消息，并记入历史。
// 正文里可以带 CQ 码：`[CQ:at,qq=<openid>]` 会被翻成官方 @，`[CQ:reply,id=<msg_id>]`
// 会走被动回复（真引用）；其它富媒体 CQ 码请用上传接口。
func (pa *PlatformAdapterOfficialQQ) SendChatConsoleMessage(kind, targetID, text string, opts ChatConsoleSendOptions) (*ChatConsoleMessage, error) {
	if pa == nil || pa.Api == nil {
		return nil, errors.New("官方 QQ 机器人尚未连接")
	}
	if pa.EndPoint == nil {
		return nil, errors.New("官方 QQ 端点信息缺失")
	}
	if targetID == "" {
		return nil, errors.New("会话目标为空")
	}
	if kind != chatConsoleKindGroup && kind != chatConsoleKindC2C {
		return nil, fmt.Errorf("未知的会话类型: %s", kind)
	}

	payload, err := pa.chatConsoleBuildSend(kind, targetID, strings.TrimSpace(text), opts)
	if err != nil {
		return nil, err
	}
	toCreate := payload.toCreate

	ctx, cancel := context.WithTimeout(context.Background(), chatConsoleSendTimeout)
	defer cancel()

	// 主动消息才排队限流；被动回复（带 msg_id）不占主动消息额度
	if toCreate.MsgID == "" {
		switch kind {
		case chatConsoleKindGroup:
			if quotaErr := pa.waitGroupActiveQuota(ctx, targetID); quotaErr != nil {
				return nil, chatConsoleQuotaError(quotaErr)
			}
		case chatConsoleKindC2C:
			if quotaErr := pa.waitC2CActiveQuota(ctx, targetID); quotaErr != nil {
				return nil, chatConsoleQuotaError(quotaErr)
			}
		}
	}

	send := func(msg *dto.MessageToCreate) (*dto.Message, error) {
		switch kind {
		case chatConsoleKindGroup:
			return pa.Api.PostGroupMessage(ctx, targetID, msg)
		default:
			return pa.Api.PostC2CMessage(ctx, targetID, msg)
		}
	}

	var (
		res *dto.Message
	)
	res, err = send(toCreate)
	if err != nil && toCreate.Markdown != nil {
		// 这个 bot 发不了 markdown：退化成纯文本（@ 变成 @昵称），至少消息能发出去
		pa.chatConsoleLogf("official qq 控制台 markdown 发送失败，退化为纯文本: %v", err)
		fallback := *toCreate
		fallback.MsgType = dto.TextMsg
		fallback.Markdown = nil
		fallback.Content = payload.plainText
		res, err = send(&fallback)
	}
	if err != nil {
		pa.chatConsoleLogf("official qq 控制台发送失败: %v", err)
		return nil, errors.New(translateOfficialQQSendError(err))
	}

	msgID := ""
	if res != nil {
		msgID = res.ID
	}

	msg, appendErr := pa.chatConsoleAppendOutgoing(kind, targetID, payload.displayText, msgID, payload.quote)
	if appendErr != nil {
		pa.chatConsoleLogf("official qq 聊天控制台记录控制台消息失败: %v", appendErr)
	}
	if msg != nil {
		return msg, nil
	}
	return &ChatConsoleMessage{
		ID:         msgID,
		TS:         time.Now().Unix(),
		Dir:        chatConsoleDirOut,
		Source:     chatConsoleSourceConsole,
		SenderID:   pa.EndPoint.UserID,
		SenderName: pa.EndPoint.Nickname,
		Text:       payload.displayText,
		MsgID:      msgID,
		Quote:      payload.quote,
	}, nil
}

// chatConsoleAppendOutgoing 记录一条"骰主从控制台发出"的消息。
func (pa *PlatformAdapterOfficialQQ) chatConsoleAppendOutgoing(kind, targetID, text, msgID string, quote *ChatConsoleQuote) (*ChatConsoleMessage, error) {
	store := pa.chatConsoleStore()
	if store == nil {
		return nil, errors.New("聊天控制台未初始化")
	}
	return store.Append(ChatConsoleRecord{
		Kind:       kind,
		TargetID:   targetID,
		UIN:        pa.UIN,
		EndpointID: pa.EndPoint.ID,
		AppID:      pa.AppID,
		SenderID:   pa.EndPoint.UserID,
		SenderName: pa.EndPoint.Nickname,
		Dir:        chatConsoleDirOut,
		Source:     chatConsoleSourceConsole,
		Text:       text,
		MessageID:  msgID,
		TS:         time.Now().Unix(),
		Quote:      quote,
	})
}

// SendChatConsoleMedia 以机器人身份发送图片 / 语音 / 视频 / 文件。
// fileName 只用于判断类型和展示，真正的文件名由官方富媒体上传决定。
func (pa *PlatformAdapterOfficialQQ) SendChatConsoleMedia(kind, targetID, fileName string, data []byte) (*ChatConsoleMessage, error) {
	if pa == nil || pa.Api == nil {
		return nil, errors.New("官方 QQ 机器人尚未连接")
	}
	if pa.EndPoint == nil {
		return nil, errors.New("官方 QQ 端点信息缺失")
	}
	if len(data) == 0 {
		return nil, errors.New("文件内容为空")
	}
	if len(data) > chatConsoleMediaMaxBytes {
		return nil, fmt.Errorf("文件过大（%d MB），请控制在 %d MB 以内",
			len(data)>>20, chatConsoleMediaMaxBytes>>20)
	}

	fileType, label := chatConsoleMediaType(fileName, data)

	ctx, cancel := context.WithTimeout(context.Background(), chatConsoleMediaTimeout)
	defer cancel()

	switch kind {
	case chatConsoleKindGroup:
		if err := pa.waitGroupActiveQuota(ctx, targetID); err != nil {
			return nil, chatConsoleQuotaError(err)
		}
	case chatConsoleKindC2C:
		if err := pa.waitC2CActiveQuota(ctx, targetID); err != nil {
			return nil, chatConsoleQuotaError(err)
		}
	default:
		return nil, fmt.Errorf("未知的会话类型: %s", kind)
	}

	// 落到临时文件：走分片上传时能保留文件名，不走分片时读 Stream 转 base64。
	ext := filepath.Ext(fileName)
	if ext == "" {
		ext = ".bin"
	}
	tmp, err := os.CreateTemp("", "sealdice-chat-console-*"+ext)
	if err != nil {
		return nil, errors.New("创建临时文件失败: " + err.Error())
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, writeErr := tmp.Write(data); writeErr != nil {
		_ = tmp.Close()
		return nil, errors.New("写入临时文件失败: " + writeErr.Error())
	}
	_ = tmp.Close()

	elem := &message.FileElement{
		File:        tmpPath,
		URL:         "file://" + tmpPath,
		ContentType: http.DetectContentType(data),
		Stream:      bytes.NewReader(data),
	}

	var media *dto.MediaInfo
	switch kind {
	case chatConsoleKindGroup:
		media, err = pa.uploadGroupMedia(ctx, targetID, elem, fileType)
	case chatConsoleKindC2C:
		media, err = pa.uploadC2CMedia(ctx, targetID, elem, fileType)
	}
	if err != nil {
		pa.chatConsoleLogf("official qq 控制台上传富媒体失败: %v", err)
		return nil, errors.New(translateOfficialQQSendError(err))
	}

	toCreate := &dto.MessageToCreate{
		MsgType: dto.RichMediaMsg,
		Media:   media,
		MsgSeq:  rand.Uint32()%10000000 + 1, //nolint:gosec // 仅用于消息去重序号
	}
	var (
		res *dto.Message
	)
	switch kind {
	case chatConsoleKindGroup:
		res, err = pa.Api.PostGroupMessage(ctx, targetID, toCreate)
	case chatConsoleKindC2C:
		res, err = pa.Api.PostC2CMessage(ctx, targetID, toCreate)
	}
	if err != nil {
		pa.chatConsoleLogf("official qq 控制台发送富媒体失败: %v", err)
		return nil, errors.New(translateOfficialQQSendError(err))
	}

	msgID := ""
	if res != nil {
		msgID = res.ID
	}
	displayText := label
	if fileName != "" && fileType == officialQQFileTypeFile {
		displayText = fmt.Sprintf("[文件 %s]", fileName)
	}
	msg, appendErr := pa.chatConsoleAppendOutgoing(kind, targetID, displayText, msgID, nil)
	if appendErr != nil {
		pa.chatConsoleLogf("official qq 聊天控制台记录媒体消息失败: %v", appendErr)
	}
	if msg != nil {
		return msg, nil
	}
	return &ChatConsoleMessage{
		ID:         msgID,
		TS:         time.Now().Unix(),
		Dir:        chatConsoleDirOut,
		Source:     chatConsoleSourceConsole,
		SenderID:   pa.EndPoint.UserID,
		SenderName: pa.EndPoint.Nickname,
		Text:       displayText,
		MsgID:      msgID,
	}, nil
}

func chatConsoleMediaType(fileName string, data []byte) (int, string) {
	mime := http.DetectContentType(data)
	name := strings.ToLower(fileName)
	switch {
	case strings.HasPrefix(mime, "image/"):
		return officialQQFileTypeImage, "[图片]"
	case strings.HasPrefix(mime, "video/"):
		return officialQQFileTypeVideo, "[视频]"
	case strings.HasPrefix(mime, "audio/"), strings.HasSuffix(name, ".silk"), strings.HasSuffix(name, ".amr"):
		return officialQQFileTypeRecord, "[语音]"
	}
	if strings.HasSuffix(name, ".jpg") || strings.HasSuffix(name, ".jpeg") ||
		strings.HasSuffix(name, ".png") || strings.HasSuffix(name, ".gif") ||
		strings.HasSuffix(name, ".webp") || strings.HasSuffix(name, ".bmp") {
		return officialQQFileTypeImage, "[图片]"
	}
	return officialQQFileTypeFile, "[文件]"
}

// RecallChatConsoleMessage 撤回机器人自己发出的消息（官方只允许撤回自己的）。
func (pa *PlatformAdapterOfficialQQ) RecallChatConsoleMessage(kind, targetID, msgID string) error {
	if pa == nil || pa.Api == nil {
		return errors.New("官方 QQ 机器人尚未连接")
	}
	if targetID == "" || msgID == "" {
		return errors.New("缺少会话或消息 ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), chatConsoleRecallTimeout)
	defer cancel()

	var err error
	switch kind {
	case chatConsoleKindGroup:
		err = pa.Api.RetractGroupMessage(ctx, targetID, msgID)
	case chatConsoleKindC2C:
		err = pa.Api.RetractC2CMessage(ctx, targetID, msgID)
	default:
		return fmt.Errorf("未知的会话类型: %s", kind)
	}
	if err != nil {
		pa.chatConsoleLogf("official qq 控制台撤回失败: %v", err)
		return errors.New(translateOfficialQQRecallError(err))
	}
	return nil
}

// MuteChatConsoleMember 禁言 / 解禁群成员（seconds<=0 表示解除禁言）。
// 官方要求机器人是群管理员，单次最长 30 天，接口 60 QPM。
func (pa *PlatformAdapterOfficialQQ) MuteChatConsoleMember(groupOpenID, memberOpenID string, seconds int) error {
	if pa == nil || pa.Api == nil {
		return errors.New("官方 QQ 机器人尚未连接")
	}
	if groupOpenID == "" || memberOpenID == "" {
		return errors.New("缺少群或成员 ID")
	}
	if seconds > 30*24*3600 {
		seconds = 30 * 24 * 3600
	}

	op := "add"
	expireAt := ""
	if seconds > 0 {
		expireAt = time.Now().Add(time.Duration(seconds) * time.Second).In(officialQQTimeZone).Format(time.RFC3339)
	} else {
		op = "del"
	}

	payload := map[string]interface{}{
		"members": []map[string]string{
			{
				"op":             op,
				"member_openid":  memberOpenID,
				"mute_expire_at": expireAt,
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), chatConsoleRecallTimeout)
	defer cancel()

	_, err := pa.Api.Transport(ctx, http.MethodPost,
		qqconstant.APIDomain+"/v2/groups/"+url.PathEscape(groupOpenID)+"/restrict_chat_setting", payload)
	if err != nil {
		pa.chatConsoleLogf("official qq 控制台禁言失败: %v", err)
		return errors.New(translateOfficialQQMuteError(err))
	}
	return nil
}

// chatConsoleQuotaError 把排队超时翻译成骰主能理解的提示。
func chatConsoleQuotaError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return errors.New("发送排队超时：官方主动消息额度紧张，请稍后再试")
	}
	return err
}

// translateOfficialQQSendError 把官方错误翻译成骰主能看懂的中文提示。
func translateOfficialQQSendError(err error) string {
	if err == nil {
		return ""
	}
	raw := err.Error()
	hint := ""
	switch {
	case strings.Contains(raw, "context deadline exceeded"),
		strings.Contains(raw, "Timeout"):
		hint = "请求官方接口超时，请稍后再试"
	case strings.Contains(raw, "429"),
		strings.Contains(raw, "频率"),
		strings.Contains(raw, "限流"),
		strings.Contains(raw, "limit"),
		strings.Contains(raw, "22009"),
		strings.Contains(raw, "40054"):
		hint = "发送频率或主动消息额度受限，请稍后再试"
	case strings.Contains(raw, "403"),
		strings.Contains(raw, "permission"),
		strings.Contains(raw, "无权限"),
		strings.Contains(raw, "没有权限"),
		strings.Contains(raw, "50001"):
		hint = "机器人没有在该会话发送消息的权限"
	case strings.Contains(raw, "401"),
		strings.Contains(raw, "token"),
		strings.Contains(raw, "鉴权"),
		strings.Contains(raw, "AppSecret"):
		hint = "鉴权失败：请检查 AppID/AppSecret 与 IP 白名单"
	case strings.Contains(raw, "400"),
		strings.Contains(raw, "msg_id"),
		strings.Contains(raw, "invalid"):
		hint = "消息内容或回复目标不合法"
	}
	if hint == "" {
		return "发送失败：" + raw
	}
	return "发送失败：" + hint + "（" + raw + "）"
}

// translateOfficialQQRecallError 撤回失败的提示。
func translateOfficialQQRecallError(err error) string {
	raw := err.Error()
	switch {
	case strings.Contains(raw, "403"),
		strings.Contains(raw, "permission"),
		strings.Contains(raw, "无权限"):
		return "撤回失败：官方只允许撤回机器人自己发的消息，且有时间限制（" + raw + "）"
	case strings.Contains(raw, "404"),
		strings.Contains(raw, "not found"):
		return "撤回失败：消息不存在或已经被撤回（" + raw + "）"
	case strings.Contains(raw, "400"):
		return "撤回失败：消息 ID 不合法（" + raw + "）"
	}
	return "撤回失败：" + raw
}

// translateOfficialQQMuteError 禁言失败的提示。
func translateOfficialQQMuteError(err error) string {
	raw := err.Error()
	switch {
	case strings.Contains(raw, "403"),
		strings.Contains(raw, "permission"),
		strings.Contains(raw, "无权限"),
		strings.Contains(raw, "50001"):
		return "禁言失败：机器人需要是群管理员，且不能禁言群主或其他管理员（" + raw + "）"
	case strings.Contains(raw, "404"):
		return "禁言失败：群不存在或机器人不在群里（" + raw + "）"
	case strings.Contains(raw, "429"), strings.Contains(raw, "频率"):
		return "禁言失败：接口调用过于频繁（60 次/分钟），请稍后再试（" + raw + "）"
	}
	return "禁言失败：" + raw
}
