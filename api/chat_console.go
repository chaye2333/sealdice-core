package api

// 官方 QQ 聊天控制台的 HTTP 接口。
//
//	GET  /sd-api/chat-console/conversations        会话列表
//	GET  /sd-api/chat-console/messages             历史消息（向前分页）
//	POST /sd-api/chat-console/send                 以机器人身份发送（支持伪引用 / @ 群成员）
//	POST /sd-api/chat-console/upload               发送图片 / 语音 / 视频 / 文件
//	POST /sd-api/chat-console/recall               撤回机器人自己发的消息
//	POST /sd-api/chat-console/mute                 禁言 / 解禁群成员
//	POST /sd-api/chat-console/read                 标记已读
//	POST /sd-api/chat-console/rename               手动改会话名
//	GET  /sd-api/chat-console/events               SSE 实时推送
//
// 所有接口沿用 WebUI 的 AccessToken 鉴权（token 头或 token 查询参数）。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"sealdice-core/dice"
)

// chatConsoleConversationResp 在会话元信息上补一个"端点是否在线"的派生字段。
type chatConsoleConversationResp struct {
	*dice.ChatConsoleConversation
	Online bool `json:"online"`
}

type chatConsoleSendReq struct {
	Key      string                    `json:"key"`
	Text     string                    `json:"text"`
	Quote    *dice.ChatConsoleQuote    `json:"quote"`
	Mentions []dice.ChatConsoleMention `json:"mentions"`
}

type chatConsoleRecallReq struct {
	Key   string `json:"key"`
	MsgID string `json:"msgId"`
}

type chatConsoleMuteReq struct {
	Key          string `json:"key"`
	MemberOpenID string `json:"memberOpenId"`
	Seconds      int    `json:"seconds"`
}

type chatConsoleKeyReq struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

type chatConsoleSettingsReq struct {
	// RetentionDays 保留天数；0 表示永久保留。
	RetentionDays int `json:"retentionDays"`
}

type chatConsolePurgeReq struct {
	Key string `json:"key"`
	All bool   `json:"all"`
}

type chatConsolePinReq struct {
	Key    string `json:"key"`
	Pinned bool   `json:"pinned"`
}

func chatConsoleStoreOf() *dice.ChatConsoleStore {
	if myDice == nil {
		return nil
	}
	return myDice.GetChatConsole()
}

// chatConsoleFindAdapter 找到会话所属的官方 QQ 适配器。
// endpointID 缺失时退化为"任意一个官方 QQ 端点"，避免历史数据因换端点而不可用。
func chatConsoleFindAdapter(endpointID string) *dice.PlatformAdapterOfficialQQ {
	if myDice == nil || myDice.ImSession == nil {
		return nil
	}
	var fallback *dice.PlatformAdapterOfficialQQ
	for _, ep := range myDice.ImSession.EndPoints {
		pa, ok := ep.Adapter.(*dice.PlatformAdapterOfficialQQ)
		if !ok || pa == nil {
			continue
		}
		if endpointID != "" && ep.ID == endpointID {
			return pa
		}
		if fallback == nil && pa.Api != nil {
			fallback = pa
		}
	}
	return fallback
}

func chatConsoleSplitKey(key string) (kind, targetID string, ok bool) {
	kind, targetID, found := strings.Cut(key, ":")
	if !found || targetID == "" {
		return "", "", false
	}
	if kind != "group" && kind != "c2c" {
		return "", "", false
	}
	return kind, targetID, true
}

func chatConsoleConversations(c echo.Context) error {
	if !doAuth(c) {
		return c.NoContent(http.StatusForbidden)
	}
	store := chatConsoleStoreOf()
	if store == nil {
		return Error(&c, "聊天控制台尚未初始化", Response{})
	}

	convs := store.Conversations()
	online := map[string]bool{}
	if myDice.ImSession != nil {
		for _, ep := range myDice.ImSession.EndPoints {
			if pa, ok := ep.Adapter.(*dice.PlatformAdapterOfficialQQ); ok && pa != nil && pa.Api != nil {
				online[ep.ID] = true
			}
		}
	}

	list := make([]chatConsoleConversationResp, 0, len(convs))
	for _, conv := range convs {
		list = append(list, chatConsoleConversationResp{
			ChatConsoleConversation: conv,
			Online:                  online[conv.EndpointID],
		})
	}
	return Success(&c, Response{"conversations": list})
}

func chatConsoleMessages(c echo.Context) error {
	if !doAuth(c) {
		return c.NoContent(http.StatusForbidden)
	}
	store := chatConsoleStoreOf()
	if store == nil {
		return Error(&c, "聊天控制台尚未初始化", Response{})
	}

	key := c.QueryParam("key")
	if _, _, ok := chatConsoleSplitKey(key); !ok {
		return Error(&c, "会话 key 不合法", Response{})
	}
	before, _ := strconv.ParseInt(c.QueryParam("before"), 10, 64)
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	if limit <= 0 {
		limit = 50
	}

	msgs, hasMore, err := store.Messages(key, before, limit)
	if err != nil {
		return Error(&c, "读取历史消息失败: "+err.Error(), Response{})
	}
	return Success(&c, Response{
		"messages": msgs,
		"hasMore":  hasMore,
	})
}

// chatConsoleResolve 解析会话 key，返回适配器与平台侧 ID。
func chatConsoleResolve(key string) (*dice.PlatformAdapterOfficialQQ, string, string, bool) {
	kind, targetID, ok := chatConsoleSplitKey(key)
	if !ok {
		return nil, "", "", false
	}
	var endpointID string
	if store := chatConsoleStoreOf(); store != nil {
		if conv := store.Conversation(key); conv != nil {
			endpointID = conv.EndpointID
		}
	}
	return chatConsoleFindAdapter(endpointID), kind, targetID, true
}

func chatConsoleSend(c echo.Context) error {
	if !doAuth(c) {
		return c.NoContent(http.StatusForbidden)
	}
	if dm != nil && dm.JustForTest {
		return Error(&c, "展示模式不支持该操作", Response{"testMode": true})
	}

	var req chatConsoleSendReq
	if err := c.Bind(&req); err != nil {
		return Error(&c, "请求格式错误", Response{})
	}
	pa, kind, targetID, ok := chatConsoleResolve(req.Key)
	if !ok {
		return Error(&c, "会话 key 不合法", Response{})
	}
	if strings.TrimSpace(req.Text) == "" && len(req.Mentions) == 0 {
		return Error(&c, "消息不能为空", Response{})
	}
	if pa == nil {
		return Error(&c, "没有可用的官方 QQ 机器人连接", Response{})
	}

	opts := dice.ChatConsoleSendOptions{Quote: req.Quote, Mentions: req.Mentions}
	msg, err := pa.SendChatConsoleMessage(kind, targetID, req.Text, opts)
	if err != nil {
		return Error(&c, err.Error(), Response{})
	}
	return Success(&c, Response{"message": msg})
}

// chatConsoleUpload 接收浏览器上传的文件，转成官方富媒体发出去。
func chatConsoleUpload(c echo.Context) error {
	if !doAuth(c) {
		return c.NoContent(http.StatusForbidden)
	}
	if dm != nil && dm.JustForTest {
		return Error(&c, "展示模式不支持该操作", Response{"testMode": true})
	}

	key := c.FormValue("key")
	pa, kind, targetID, ok := chatConsoleResolve(key)
	if !ok {
		return Error(&c, "会话 key 不合法", Response{})
	}
	if pa == nil {
		return Error(&c, "没有可用的官方 QQ 机器人连接", Response{})
	}

	header, err := c.FormFile("file")
	if err != nil {
		return Error(&c, "没有收到文件", Response{})
	}
	const maxUpload = 30 << 20
	if header.Size > maxUpload {
		return Error(&c, "文件过大，请控制在 30 MB 以内", Response{})
	}
	src, err := header.Open()
	if err != nil {
		return Error(&c, "读取上传文件失败", Response{})
	}
	defer func() { _ = src.Close() }()
	data, err := io.ReadAll(io.LimitReader(src, maxUpload+1))
	if err != nil {
		return Error(&c, "读取上传文件失败", Response{})
	}
	if len(data) > maxUpload {
		return Error(&c, "文件过大，请控制在 30 MB 以内", Response{})
	}

	msg, err := pa.SendChatConsoleMedia(kind, targetID, header.Filename, data)
	if err != nil {
		return Error(&c, err.Error(), Response{})
	}
	return Success(&c, Response{"message": msg})
}

// chatConsoleRecall 撤回机器人自己发出的消息。
func chatConsoleRecall(c echo.Context) error {
	if !doAuth(c) {
		return c.NoContent(http.StatusForbidden)
	}
	if dm != nil && dm.JustForTest {
		return Error(&c, "展示模式不支持该操作", Response{"testMode": true})
	}
	var req chatConsoleRecallReq
	if err := c.Bind(&req); err != nil {
		return Error(&c, "请求格式错误", Response{})
	}
	if req.MsgID == "" {
		return Error(&c, "缺少消息 ID", Response{})
	}

	store := chatConsoleStoreOf()
	if store == nil {
		return Error(&c, "聊天控制台尚未初始化", Response{})
	}
	recorded := store.Find(req.Key, req.MsgID)
	if recorded == nil {
		return Error(&c, "没有找到这条消息", Response{})
	}
	if recorded.Dir != "out" {
		return Error(&c, "只能撤回机器人自己发送的消息", Response{})
	}
	if recorded.Recalled {
		return Error(&c, "这条消息已经撤回过了", Response{})
	}
	if recorded.MsgID == "" {
		return Error(&c, "这条消息没有官方消息 ID，无法撤回", Response{})
	}

	pa, kind, targetID, ok := chatConsoleResolve(req.Key)
	if !ok {
		return Error(&c, "会话 key 不合法", Response{})
	}
	if pa == nil {
		return Error(&c, "没有可用的官方 QQ 机器人连接", Response{})
	}
	if err := pa.RecallChatConsoleMessage(kind, targetID, recorded.MsgID); err != nil {
		return Error(&c, err.Error(), Response{})
	}
	msg, err := store.MarkRecalled(req.Key, recorded.ID)
	if err != nil {
		return Error(&c, "官方已撤回，但本地记录更新失败: "+err.Error(), Response{})
	}
	return Success(&c, Response{"message": msg})
}

// chatConsoleMute 禁言 / 解禁群成员（seconds<=0 表示解除禁言）。
func chatConsoleMute(c echo.Context) error {
	if !doAuth(c) {
		return c.NoContent(http.StatusForbidden)
	}
	if dm != nil && dm.JustForTest {
		return Error(&c, "展示模式不支持该操作", Response{"testMode": true})
	}
	var req chatConsoleMuteReq
	if err := c.Bind(&req); err != nil {
		return Error(&c, "请求格式错误", Response{})
	}
	pa, kind, targetID, ok := chatConsoleResolve(req.Key)
	if !ok {
		return Error(&c, "会话 key 不合法", Response{})
	}
	if kind != "group" {
		return Error(&c, "只有群聊支持禁言", Response{})
	}
	if req.MemberOpenID == "" {
		return Error(&c, "缺少成员 openid", Response{})
	}
	if pa == nil {
		return Error(&c, "没有可用的官方 QQ 机器人连接", Response{})
	}
	if err := pa.MuteChatConsoleMember(targetID, req.MemberOpenID, req.Seconds); err != nil {
		return Error(&c, err.Error(), Response{})
	}
	action := "已禁言"
	if req.Seconds <= 0 {
		action = "已解除禁言"
	}
	return Success(&c, Response{"text": action})
}

func chatConsoleRead(c echo.Context) error {
	if !doAuth(c) {
		return c.NoContent(http.StatusForbidden)
	}
	store := chatConsoleStoreOf()
	if store == nil {
		return Error(&c, "聊天控制台尚未初始化", Response{})
	}
	var req chatConsoleKeyReq
	if err := c.Bind(&req); err != nil {
		return Error(&c, "请求格式错误", Response{})
	}
	conv := store.MarkRead(req.Key)
	if conv == nil {
		return Error(&c, "会话不存在", Response{})
	}
	return Success(&c, Response{})
}

func chatConsoleRename(c echo.Context) error {
	if !doAuth(c) {
		return c.NoContent(http.StatusForbidden)
	}
	store := chatConsoleStoreOf()
	if store == nil {
		return Error(&c, "聊天控制台尚未初始化", Response{})
	}
	var req chatConsoleKeyReq
	if err := c.Bind(&req); err != nil {
		return Error(&c, "请求格式错误", Response{})
	}
	conv, err := store.Rename(req.Key, req.Name)
	if err != nil {
		return Error(&c, err.Error(), Response{})
	}
	return Success(&c, Response{"conversation": conv})
}

// chatConsoleStorage 返回保留天数、总占用与每个会话的占用。
func chatConsoleStorage(c echo.Context) error {
	if !doAuth(c) {
		return c.NoContent(http.StatusForbidden)
	}
	store := chatConsoleStoreOf()
	if store == nil {
		return Error(&c, "聊天控制台尚未初始化", Response{})
	}
	total, items := store.Usage()
	return Success(&c, Response{
		"totalBytes":    total,
		"retentionDays": store.RetentionDays(),
		"conversations": items,
	})
}

// chatConsoleSettingsSave 调整保留天数（0 = 永久保留）。
func chatConsoleSettingsSave(c echo.Context) error {
	if !doAuth(c) {
		return c.NoContent(http.StatusForbidden)
	}
	if dm != nil && dm.JustForTest {
		return Error(&c, "展示模式不支持该操作", Response{"testMode": true})
	}
	var req chatConsoleSettingsReq
	if err := c.Bind(&req); err != nil {
		return Error(&c, "请求格式错误", Response{})
	}
	if req.RetentionDays < 0 {
		return Error(&c, "保留天数不能为负数（0 表示永久保留）", Response{})
	}
	store := chatConsoleStoreOf()
	if store == nil {
		return Error(&c, "聊天控制台尚未初始化", Response{})
	}
	days := store.SetRetentionDays(req.RetentionDays)
	return Success(&c, Response{"retentionDays": days})
}

// chatConsolePurge 清空单个会话历史，或（all=true）清空全部。
func chatConsolePurge(c echo.Context) error {
	if !doAuth(c) {
		return c.NoContent(http.StatusForbidden)
	}
	if dm != nil && dm.JustForTest {
		return Error(&c, "展示模式不支持该操作", Response{"testMode": true})
	}
	var req chatConsolePurgeReq
	if err := c.Bind(&req); err != nil {
		return Error(&c, "请求格式错误", Response{})
	}
	store := chatConsoleStoreOf()
	if store == nil {
		return Error(&c, "聊天控制台尚未初始化", Response{})
	}
	if req.All {
		if err := store.ClearAll(); err != nil {
			return Error(&c, "清空失败: "+err.Error(), Response{})
		}
		return Success(&c, Response{})
	}
	if req.Key == "" {
		return Error(&c, "缺少会话 key（或传 all=true 清空全部）", Response{})
	}
	if _, _, ok := chatConsoleSplitKey(req.Key); !ok {
		return Error(&c, "会话 key 不合法", Response{})
	}
	if err := store.ClearConversation(req.Key); err != nil {
		return Error(&c, "清空失败: "+err.Error(), Response{})
	}
	return Success(&c, Response{})
}

// chatConsolePin 置顶 / 取消置顶会话。
func chatConsolePin(c echo.Context) error {
	if !doAuth(c) {
		return c.NoContent(http.StatusForbidden)
	}
	if dm != nil && dm.JustForTest {
		return Error(&c, "展示模式不支持该操作", Response{"testMode": true})
	}
	var req chatConsolePinReq
	if err := c.Bind(&req); err != nil {
		return Error(&c, "请求格式错误", Response{})
	}
	store := chatConsoleStoreOf()
	if store == nil {
		return Error(&c, "聊天控制台尚未初始化", Response{})
	}
	conv, err := store.SetPinned(req.Key, req.Pinned)
	if err != nil {
		return Error(&c, err.Error(), Response{})
	}
	return Success(&c, Response{"conversation": conv})
}

// chatConsoleAllowedMediaHost 控制台媒体代理的域名白名单。
// 只放行腾讯的媒体 / 头像域名，避免把 /media 变成任意 URL 代理（SSRF）。
func chatConsoleAllowedMediaHost(host string) bool {
	host = strings.ToLower(host)
	for _, suffix := range []string{".qq.com", ".qq.com.cn", ".qpic.cn", ".qlogo.cn", ".myqcloud.com"} {
		if host == strings.TrimPrefix(suffix, ".") || strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// chatConsoleMedia 代理拉取官方媒体（图片 / 表情包 / 头像）。
// 浏览器直连 thirdqq.qlogo.cn、multimedia.nt.qq.com.cn 时常被防盗链或跨域挡掉，
// 由后端带 Referer 取回来再转发，界面上就能正常显示。
func chatConsoleMedia(c echo.Context) error {
	if !doAuth(c) {
		return c.NoContent(http.StatusForbidden)
	}
	raw := c.QueryParam("url")
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return c.NoContent(http.StatusBadRequest)
	}
	if !chatConsoleAllowedMediaHost(u.Hostname()) {
		return c.NoContent(http.StatusForbidden)
	}

	const maxBytes = 20 << 20 // 20MB
	ctx := c.Request().Context()
	client := &http.Client{
		Timeout: 20 * time.Second,
		// 不跟随跳转：避免绕过域名白名单
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return c.NoContent(http.StatusBadRequest)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (SealDice chat console)")
	req.Header.Set("Referer", "https://q.qq.com/")

	resp, err := client.Do(req)
	if err != nil {
		return c.NoContent(http.StatusBadGateway)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return c.NoContent(http.StatusBadGateway)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	respHeader := c.Response().Header()
	respHeader.Set("Content-Type", contentType)
	respHeader.Set("Cache-Control", "private, max-age=300")
	respHeader.Set("X-Content-Type-Options", "nosniff")
	c.Response().WriteHeader(http.StatusOK)
	_, _ = io.Copy(c.Response(), io.LimitReader(resp.Body, maxBytes))
	return nil
}

// chatConsoleEvents 用 SSE 把新消息 / 会话变更推给 WebUI。
// 单向推送足够，用 SSE 比 WebSocket 省事（无需额外依赖，断线由浏览器自动重连）。
func chatConsoleEvents(c echo.Context) error {
	if !doAuth(c) {
		return c.NoContent(http.StatusForbidden)
	}
	store := chatConsoleStoreOf()
	if store == nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}

	events := make(chan dice.ChatConsoleEvent, 64)
	cancel := store.Subscribe(func(e dice.ChatConsoleEvent) {
		select {
		case events <- e:
		default:
			// 订阅者积压：丢弃最慢客户端的事件，避免阻塞消息落盘。
		}
	})
	defer cancel()

	res := c.Response()
	res.Header().Set("Content-Type", "text/event-stream")
	res.Header().Set("Cache-Control", "no-cache")
	res.Header().Set("Connection", "keep-alive")
	res.Header().Set("X-Accel-Buffering", "no")
	res.WriteHeader(http.StatusOK)
	res.Flush()

	// 连上先发一个 ready，前端据此把"实时连接"点亮。
	if _, err := io.WriteString(res, "event: ready\ndata: {}\n\n"); err != nil {
		return nil
	}
	res.Flush()

	ctx := c.Request().Context()
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := io.WriteString(res, ": ping\n\n"); err != nil {
				return nil
			}
			res.Flush()
		case e := <-events:
			raw, err := json.Marshal(e)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(res, "event: %s\ndata: %s\n\n", e.Type, raw); err != nil {
				return nil
			}
			res.Flush()
		}
	}
}
