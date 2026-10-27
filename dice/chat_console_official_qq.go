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
func (pa *PlatformAdapterOfficialQQ) recordChatConsoleInbound(msg *Message, kind, targetID, senderOpenID string, full bool) {
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
		SenderID:     msg.Sender.UserID,
		SenderName:   msg.Sender.Nickname,
		SenderOpenID: senderOpenID,
		Dir:          chatConsoleDirIn,
		Source:       chatConsoleSourceUser,
		Text:         msg.Message,
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
		name, count, err := pa.fetchChatConsoleGroupInfo(groupOpenID)
		if err != nil {
			pa.chatConsoleLogf("official qq 获取群信息失败(%s): %v", groupOpenID, err)
			return
		}
		store.SetGroupInfo(key, name, count)
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
func (pa *PlatformAdapterOfficialQQ) fetchChatConsoleGroupInfo(groupOpenID string) (string, int, error) {
	if pa.Api == nil {
		return "", 0, errors.New("官方 QQ 机器人尚未连接")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	body, err := pa.Api.Transport(ctx, http.MethodGet,
		qqconstant.APIDomain+"/v2/groups/"+url.PathEscape(groupOpenID)+"/info", nil)
	if err != nil {
		return "", 0, err
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", 0, err
	}
	name := chatConsoleFirstString(payload, "group_name", "name")
	count := chatConsoleFirstInt(payload, "group_member_num", "member_count", "group_member_count")
	if name == "" && count == 0 {
		return "", 0, errors.New("群信息接口没有返回群名或成员数")
	}
	return name, count, nil
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

// SendChatConsoleMessage 以机器人身份向会话发送一条文字消息，并记入历史。
func (pa *PlatformAdapterOfficialQQ) SendChatConsoleMessage(kind, targetID, text string, opts ChatConsoleSendOptions) (*ChatConsoleMessage, error) {
	text = strings.TrimSpace(text)
	if text == "" && len(opts.Mentions) == 0 {
		return nil, errors.New("消息不能为空")
	}
	if pa == nil || pa.Api == nil {
		return nil, errors.New("官方 QQ 机器人尚未连接")
	}
	if pa.EndPoint == nil {
		return nil, errors.New("官方 QQ 端点信息缺失")
	}
	if targetID == "" {
		return nil, errors.New("会话目标为空")
	}
	if runes := []rune(text); len(runes) > chatConsoleSendMaxLen {
		return nil, fmt.Errorf("消息过长（%d 字），单条请控制在 %d 字以内", len(runes), chatConsoleSendMaxLen)
	}

	ctx, cancel := context.WithTimeout(context.Background(), chatConsoleSendTimeout)
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

	toCreate := &dto.MessageToCreate{
		Content: opts.platformText(kind, text),
		MsgType: dto.TextMsg,
		MsgSeq:  rand.Uint32()%10000000 + 1, //nolint:gosec // 仅用于消息去重序号
	}

	var (
		res *dto.Message
		err error
	)
	switch kind {
	case chatConsoleKindGroup:
		res, err = pa.Api.PostGroupMessage(ctx, targetID, toCreate)
	case chatConsoleKindC2C:
		res, err = pa.Api.PostC2CMessage(ctx, targetID, toCreate)
	}
	if err != nil {
		pa.chatConsoleLogf("official qq 控制台发送失败: %v", err)
		return nil, errors.New(translateOfficialQQSendError(err))
	}

	msgID := ""
	if res != nil {
		msgID = res.ID
	}

	displayText := opts.displayText(kind, text)
	msg, appendErr := pa.chatConsoleAppendOutgoing(kind, targetID, displayText, msgID, opts.Quote)
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
		Text:       displayText,
		MsgID:      msgID,
		Quote:      opts.Quote,
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
