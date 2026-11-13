package dice //nolint:testpackage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestChatConsoleStore(t *testing.T) *ChatConsoleStore {
	t.Helper()
	s := NewChatConsoleStore(filepath.Join(t.TempDir(), "chat-console"))
	t.Cleanup(s.Close)
	return s
}

func recordTestMsg(t *testing.T, s *ChatConsoleStore, conv ChatConsoleRecord, text string, ts int64) *ChatConsoleMessage {
	t.Helper()
	conv.Text = text
	conv.TS = ts
	msg, err := s.Append(conv)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	return msg
}

func TestChatConsoleAppendAndRead(t *testing.T) {
	s := newTestChatConsoleStore(t)
	group := ChatConsoleRecord{Kind: chatConsoleKindGroup, TargetID: "G1", UIN: "10001", EndpointID: "ep1"}

	recordTestMsg(t, s, group, "大家好", 1000)
	recordTestMsg(t, s, group, "骰个侦查", 1001)

	out := group
	out.Dir = chatConsoleDirOut
	out.Source = chatConsoleSourceBot
	out.MessageID = "bot-1"
	recordTestMsg(t, s, out, "侦查 45/60 成功", 1002)

	convs := s.Conversations()
	if len(convs) != 1 {
		t.Fatalf("expect 1 conversation, got %d", len(convs))
	}
	conv := convs[0]
	if conv.Key != "group:G1" {
		t.Fatalf("unexpected key: %s", conv.Key)
	}
	if conv.Unread != 2 {
		t.Fatalf("expect unread=2, got %d", conv.Unread)
	}
	if conv.LastText != "侦查 45/60 成功" {
		t.Fatalf("unexpected last text: %q", conv.LastText)
	}

	msgs, hasMore, err := s.Messages("group:G1", 0, 50)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if hasMore {
		t.Fatalf("expect hasMore=false")
	}
	if len(msgs) != 3 {
		t.Fatalf("expect 3 messages, got %d", len(msgs))
	}
	if msgs[0].TS != 1000 || msgs[2].TS != 1002 {
		t.Fatalf("messages not in ascending order: %+v", msgs)
	}
	if msgs[2].Source != chatConsoleSourceBot || msgs[2].Dir != chatConsoleDirOut {
		t.Fatalf("outbound message not tagged: %+v", msgs[2])
	}
	if msgs[0].Media != nil {
		t.Fatalf("plain text message should not have media")
	}

	// 已读
	if c := s.MarkRead("group:G1"); c == nil || c.Unread != 0 {
		t.Fatalf("mark read failed: %+v", c)
	}
	if c := s.Conversation("group:G1"); c.Unread != 0 {
		t.Fatalf("unread not cleared: %d", c.Unread)
	}
}

func TestChatConsoleDedupe(t *testing.T) {
	s := newTestChatConsoleStore(t)
	rec := ChatConsoleRecord{
		Kind: chatConsoleKindGroup, TargetID: "G1",
		Dir: chatConsoleDirIn, Source: chatConsoleSourceUser, MessageID: "m-1",
	}
	recordTestMsg(t, s, rec, "第一次", 1000)
	msg, err := s.Append(ChatConsoleRecord{
		Kind: chatConsoleKindGroup, TargetID: "G1",
		Dir: chatConsoleDirIn, Source: chatConsoleSourceUser, MessageID: "m-1",
		Text: "重复投递", TS: 1001,
	})
	if err != nil {
		t.Fatalf("dedupe append: %v", err)
	}
	if msg != nil {
		t.Fatalf("duplicated message should be dropped")
	}
	msgs, _, _ := s.Messages("group:G1", 0, 50)
	if len(msgs) != 1 {
		t.Fatalf("expect 1 message after dedupe, got %d", len(msgs))
	}
	if c := s.Conversation("group:G1"); c.Unread != 1 {
		t.Fatalf("expect unread=1, got %d", c.Unread)
	}
}

func TestChatConsolePagination(t *testing.T) {
	s := newTestChatConsoleStore(t)
	rec := ChatConsoleRecord{Kind: chatConsoleKindC2C, TargetID: "U1"}
	for i := range 120 {
		recordTestMsg(t, s, rec, "msg", int64(1000+i))
	}

	page1, hasMore, err := s.Messages("c2c:U1", 0, 50)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if !hasMore {
		t.Fatalf("expect hasMore=true on first page")
	}
	if len(page1) != 50 || page1[49].TS != 1119 {
		t.Fatalf("unexpected first page: len=%d last=%d", len(page1), page1[len(page1)-1].TS)
	}

	page2, _, _ := s.Messages("c2c:U1", page1[0].TS, 50)
	if len(page2) != 50 || page2[49].TS != 1069 {
		t.Fatalf("unexpected second page: len=%d last=%d", len(page2), page2[len(page2)-1].TS)
	}

	page3, hasMore3, _ := s.Messages("c2c:U1", page2[0].TS, 50)
	if len(page3) != 20 || hasMore3 {
		t.Fatalf("unexpected third page: len=%d hasMore=%v", len(page3), hasMore3)
	}
}

func TestChatConsoleRenameAndGroupInfo(t *testing.T) {
	s := newTestChatConsoleStore(t)
	rec := ChatConsoleRecord{Kind: chatConsoleKindGroup, TargetID: "G1"}
	recordTestMsg(t, s, rec, "hi", 1000)

	conv := s.Conversation("group:G1")
	if conv.Name != "群 G1" {
		t.Fatalf("unexpected default name: %q", conv.Name)
	}

	// 官方返回的群信息可以覆盖占位名
	if c := s.SetGroupInfo("group:G1", ChatConsoleGroupInfo{Name: "读书分享会", MemberCount: 256}); c == nil || c.Name != "读书分享会" || c.MemberCount != 256 {
		t.Fatalf("set group info failed: %+v", c)
	}

	// 手动改名后不再被官方群名覆盖，但成员数仍会更新
	if c, err := s.Rename("group:G1", "我的群"); err != nil || c.Name != "我的群" || !c.NameManual {
		t.Fatalf("rename failed: %+v %v", c, err)
	}
	if c := s.SetGroupInfo("group:G1", ChatConsoleGroupInfo{Name: "官方群名", MemberCount: 300}); c == nil || c.Name != "我的群" || c.MemberCount != 300 {
		t.Fatalf("manual name should not be overwritten: %+v", c)
	}
	if c := s.Conversation("group:G1"); c.Name != "我的群" {
		t.Fatalf("name changed unexpectedly: %q", c.Name)
	}
}

func TestChatConsoleMediaAndPreview(t *testing.T) {
	s := newTestChatConsoleStore(t)
	rec := ChatConsoleRecord{Kind: chatConsoleKindGroup, TargetID: "G1"}
	recordTestMsg(t, s, rec, "[CQ:image,file=https://example.com/a.png][CQ:file,file=https://example.com/b.pdf]看看", 1000)

	msgs, _, _ := s.Messages("group:G1", 0, 50)
	if len(msgs) != 1 {
		t.Fatalf("expect 1 message")
	}
	if len(msgs[0].Media) != 2 {
		t.Fatalf("expect 2 media, got %d", len(msgs[0].Media))
	}
	if msgs[0].Media[0].Kind != "image" || msgs[0].Media[1].Kind != "file" {
		t.Fatalf("unexpected media kinds: %+v", msgs[0].Media)
	}
	if msgs[0].Media[1].Name != "b.pdf" {
		t.Fatalf("unexpected file name: %q", msgs[0].Media[1].Name)
	}

	if c := s.Conversation("group:G1"); c.LastText != "看看" {
		t.Fatalf("CQ code should be stripped in preview, got %q", c.LastText)
	}
}

func TestChatConsolePersistAcrossReload(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chat-console")
	now := time.Now().Unix()
	s1 := NewChatConsoleStore(dir)
	recordTestMsg(t, s1, ChatConsoleRecord{Kind: chatConsoleKindGroup, TargetID: "G1"}, "第一条", now-10)
	recordTestMsg(t, s1, ChatConsoleRecord{Kind: chatConsoleKindGroup, TargetID: "G1", FullMessage: true}, "第二条", now-5)
	s1.Close()

	s2 := NewChatConsoleStore(dir)
	t.Cleanup(s2.Close)
	convs := s2.Conversations()
	if len(convs) != 1 {
		t.Fatalf("expect 1 conversation after reload, got %d", len(convs))
	}
	if !convs[0].FullMessage {
		t.Fatalf("FullMessage flag should survive reload")
	}
	if convs[0].Unread != 2 {
		t.Fatalf("unread should survive reload, got %d", convs[0].Unread)
	}
	msgs, _, err := s2.Messages("group:G1", 0, 50)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("reload messages failed: len=%d err=%v", len(msgs), err)
	}
}

func TestChatConsolePruneOldDays(t *testing.T) {
	s := newTestChatConsoleStore(t)
	now := time.Now().Unix()
	recordTestMsg(t, s, ChatConsoleRecord{Kind: chatConsoleKindGroup, TargetID: "G1"}, "旧消息", now-40*24*3600)
	recordTestMsg(t, s, ChatConsoleRecord{Kind: chatConsoleKindGroup, TargetID: "G1"}, "新消息", now)

	// 手动触发一次清理（默认保留 30 天，40 天前的应被删掉）
	s.SetRetentionDays(chatConsoleDefaultRetentionDays)

	msgs, _, err := s.Messages("group:G1", 0, 50)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Text != "新消息" {
		t.Fatalf("old message should be pruned, got %+v", msgs)
	}
}

func TestChatConsoleSubscribe(t *testing.T) {
	s := newTestChatConsoleStore(t)
	got := make(chan ChatConsoleEvent, 4)
	cancel := s.Subscribe(func(e ChatConsoleEvent) { got <- e })
	defer cancel()

	recordTestMsg(t, s, ChatConsoleRecord{Kind: chatConsoleKindGroup, TargetID: "G1"}, "hello", time.Now().Unix())

	select {
	case e := <-got:
		if e.Type != "message" || e.Message == nil || e.Message.Text != "hello" {
			t.Fatalf("unexpected event: %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no event received")
	}
}

func TestChatConsoleInvalidRecord(t *testing.T) {
	s := newTestChatConsoleStore(t)
	if _, err := s.Append(ChatConsoleRecord{Kind: "channel", TargetID: "x"}); err == nil {
		t.Fatalf("expect error for unknown kind")
	}
	if _, err := s.Append(ChatConsoleRecord{Kind: chatConsoleKindGroup}); err == nil {
		t.Fatalf("expect error for empty target")
	}
}

func TestChatConsoleRetentionSettings(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chat-console")
	s := NewChatConsoleStore(dir)
	t.Cleanup(s.Close)
	if got := s.RetentionDays(); got != chatConsoleDefaultRetentionDays {
		t.Fatalf("default retention = %d, want %d", got, chatConsoleDefaultRetentionDays)
	}
	if got := s.SetRetentionDays(7); got != 7 {
		t.Fatalf("set retention = %d, want 7", got)
	}

	// 重新加载后仍然是 7 天
	s2 := NewChatConsoleStore(dir)
	t.Cleanup(s2.Close)
	if got := s2.RetentionDays(); got != 7 {
		t.Fatalf("retention not persisted: %d", got)
	}

	// 0 = 永久保留：40 天前的消息不应被清理
	s2.SetRetentionDays(0)
	old := time.Now().Unix() - 40*24*3600
	recordTestMsg(t, s2, ChatConsoleRecord{Kind: chatConsoleKindGroup, TargetID: "G1"}, "很旧的消息", old)
	s2.SetRetentionDays(0)
	if msgs, _, _ := s2.Messages("group:G1", 0, 50); len(msgs) != 1 {
		t.Fatalf("retention=0 should keep old messages, got %d", len(msgs))
	}

	// 调回 7 天后应当立刻清掉
	s2.SetRetentionDays(7)
	if msgs, _, _ := s2.Messages("group:G1", 0, 50); len(msgs) != 0 {
		t.Fatalf("old message should be pruned after shortening retention, got %d", len(msgs))
	}

	// 负数按 0 处理，超大值被截断
	if got := s2.SetRetentionDays(-5); got != 0 {
		t.Fatalf("negative retention should clamp to 0, got %d", got)
	}
	if got := s2.SetRetentionDays(99999); got != chatConsoleMaxRetentionDays {
		t.Fatalf("retention should clamp to max, got %d", got)
	}
}

func TestChatConsoleUsage(t *testing.T) {
	s := newTestChatConsoleStore(t)
	now := time.Now().Unix()
	group := ChatConsoleRecord{Kind: chatConsoleKindGroup, TargetID: "G1"}
	for i := range 3 {
		recordTestMsg(t, s, group, "群里第几条消息", now+int64(i))
	}
	recordTestMsg(t, s, ChatConsoleRecord{Kind: chatConsoleKindC2C, TargetID: "U1"}, "私聊", now)

	total, items := s.Usage()
	if total <= 0 {
		t.Fatalf("total bytes should be > 0")
	}
	if len(items) != 2 {
		t.Fatalf("expect 2 conversations in usage, got %d", len(items))
	}
	if items[0].Key != "group:G1" {
		t.Fatalf("usage should be sorted by bytes desc, first = %q", items[0].Key)
	}
	if items[0].Messages != 3 {
		t.Fatalf("group messages = %d, want 3", items[0].Messages)
	}
	if items[0].Bytes <= 0 || items[0].Days != 1 {
		t.Fatalf("unexpected group usage: %+v", items[0])
	}
	var sum int64
	for _, it := range items {
		sum += it.Bytes
	}
	if sum <= 0 || sum > total {
		t.Fatalf("per-conversation bytes should not exceed total (sum=%d total=%d)", sum, total)
	}
}

func TestChatConsoleClear(t *testing.T) {
	s := newTestChatConsoleStore(t)
	now := time.Now().Unix()
	recordTestMsg(t, s, ChatConsoleRecord{Kind: chatConsoleKindGroup, TargetID: "G1"}, "保留前", now)
	recordTestMsg(t, s, ChatConsoleRecord{Kind: chatConsoleKindC2C, TargetID: "U1"}, "私聊消息", now)

	if err := s.ClearConversation("group:G1"); err != nil {
		t.Fatalf("clear conversation: %v", err)
	}
	if msgs, _, _ := s.Messages("group:G1", 0, 50); len(msgs) != 0 {
		t.Fatalf("group history should be empty, got %d", len(msgs))
	}
	if msgs, _, _ := s.Messages("c2c:U1", 0, 50); len(msgs) != 1 {
		t.Fatalf("other conversation should be kept, got %d", len(msgs))
	}
	if conv := s.Conversation("group:G1"); conv == nil || conv.LastText != "（已清空）" {
		t.Fatalf("conversation meta should be kept: %+v", conv)
	}

	// 清空全部
	if err := s.ClearAll(); err != nil {
		t.Fatalf("clear all: %v", err)
	}
	if convs := s.Conversations(); len(convs) != 0 {
		t.Fatalf("expect no conversations after clear all, got %d", len(convs))
	}
	if msgs, _, _ := s.Messages("c2c:U1", 0, 50); len(msgs) != 0 {
		t.Fatalf("expect no messages after clear all, got %d", len(msgs))
	}
}

func TestTranslateOfficialQQSendError(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"429 too many requests", "额度"},
		{"403 forbidden", "权限"},
		{"401 unauthorized token", "鉴权"},
		{"400 invalid msg_id", "不合法"},
		{"boom", "发送失败：boom"},
	}
	for _, c := range cases {
		got := translateOfficialQQSendError(errTest(c.raw))
		if !strings.Contains(got, c.want) {
			t.Fatalf("%q -> %q, want contains %q", c.raw, got, c.want)
		}
	}
}

func TestSendChatConsoleMessageNotConnected(t *testing.T) {
	pa := &PlatformAdapterOfficialQQ{}
	if _, err := pa.SendChatConsoleMessage(chatConsoleKindGroup, "G1", "hi", ChatConsoleSendOptions{}); err == nil {
		t.Fatalf("expect error when adapter not connected")
	}
	if _, err := pa.SendChatConsoleMessage(chatConsoleKindGroup, "G1", "  ", ChatConsoleSendOptions{}); err == nil {
		t.Fatalf("expect error for empty text")
	}
}

func TestChatConsolePlatformText(t *testing.T) {
	quote := &ChatConsoleQuote{Sender: "许烁", Text: "骰个侦查"}
	opts := ChatConsoleSendOptions{
		Quote:    quote,
		Mentions: []ChatConsoleMention{{OpenID: "OPENID-1", Name: "小明"}},
	}

	platform := opts.platformText(chatConsoleKindGroup, "收到")
	if !strings.Contains(platform, `「回复 许烁：骰个侦查」`) {
		t.Fatalf("quote prefix missing: %q", platform)
	}
	if !strings.Contains(platform, `<qqbot-at-user id="OPENID-1" />`) {
		t.Fatalf("at tag missing: %q", platform)
	}
	if !strings.HasSuffix(platform, "收到") {
		t.Fatalf("text should be at the end: %q", platform)
	}

	// 私聊不发 @ 标签
	if p := opts.platformText(chatConsoleKindC2C, "收到"); strings.Contains(p, "qqbot-at-user") {
		t.Fatalf("c2c should not contain at tags: %q", p)
	}

	display := opts.displayText(chatConsoleKindGroup, "收到")
	if display != "@小明 收到" {
		t.Fatalf("unexpected display text: %q", display)
	}
}

func TestChatConsoleMarkRecalled(t *testing.T) {
	s := newTestChatConsoleStore(t)
	rec := ChatConsoleRecord{
		Kind: chatConsoleKindGroup, TargetID: "G1",
		Dir: chatConsoleDirOut, Source: chatConsoleSourceConsole,
		MessageID: "bot-msg-1",
	}
	recordTestMsg(t, s, rec, "我要被撤回", time.Now().Unix())

	got, err := s.MarkRecalled("group:G1", "bot-msg-1")
	if err != nil {
		t.Fatalf("mark recalled: %v", err)
	}
	if !got.Recalled || got.RecalledAt == 0 {
		t.Fatalf("message not marked: %+v", got)
	}

	// 重新读盘也应该是已撤回
	msgs, _, err := s.Messages("group:G1", 0, 50)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("reload messages failed: %v", err)
	}
	if !msgs[0].Recalled {
		t.Fatalf("recalled flag should be persisted")
	}
	if c := s.Conversation("group:G1"); c.LastText != "（消息已撤回）" {
		t.Fatalf("conversation preview should be updated, got %q", c.LastText)
	}

	// 不存在的消息
	if _, err := s.MarkRecalled("group:G1", "nope"); err == nil {
		t.Fatalf("expect error for unknown message")
	}
}

func TestChatConsoleMigrateLegacyJSONL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chat-console")
	convDir := filepath.Join(dir, "group_G1")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	base := time.Now().Unix() - 3600
	indexRaw, _ := json.MarshalIndent(map[string]interface{}{
		"version": 1,
		"conversations": map[string]interface{}{
			"group:G1": map[string]interface{}{
				"key": "group:G1", "kind": "group", "targetId": "G1",
				"name": "读书分享会", "fullMessage": true, "unread": 1,
				"lastTs": base, "lastText": "旧消息",
			},
		},
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "index.json"), indexRaw, 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	lines := []string{
		fmt.Sprintf(`{"id":"m1","ts":%d,"dir":"in","source":"user","senderId":"OpenQQ:1-x","senderName":"许烁","text":"旧消息[CQ:image,file=https://example.com/a.png]","msgId":"m1"}`, base),
		fmt.Sprintf(`{"id":"m2","ts":%d,"dir":"out","source":"console","senderId":"OpenQQ:bot","senderName":"骰子","text":"回复","msgId":"m2","recalled":true,"recalledAt":%d}`, base+1, base+99),
	}
	if err := os.WriteFile(filepath.Join(convDir, chatConsoleDay(base)+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	s := NewChatConsoleStore(dir)
	t.Cleanup(s.Close)
	convs := s.Conversations()
	if len(convs) != 1 || convs[0].Name != "读书分享会" || !convs[0].FullMessage {
		t.Fatalf("legacy conversation not imported: %+v", convs)
	}
	msgs, _, err := s.Messages("group:G1", 0, 50)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("legacy messages not imported: len=%d err=%v", len(msgs), err)
	}
	if !msgs[1].Recalled || msgs[1].RecalledAt == 0 {
		t.Fatalf("recalled flag not migrated: %+v", msgs[1])
	}

	// 原始文件被挪到 legacy-jsonl/，不再参与读写
	if _, err := os.Stat(filepath.Join(dir, "legacy-jsonl", "index.json")); err != nil {
		t.Fatalf("legacy index should be moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "index.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy index should no longer exist at original path: %v", err)
	}

	// 再打开一次不应重复导入
	s.Close()
	s2 := NewChatConsoleStore(dir)
	t.Cleanup(s2.Close)
	if msgs2, _, _ := s2.Messages("group:G1", 0, 50); len(msgs2) != 2 {
		t.Fatalf("re-open should not duplicate messages, got %d", len(msgs2))
	}
}

func TestChatConsolePinnedAndAvatar(t *testing.T) {
	s := newTestChatConsoleStore(t)
	now := time.Now().Unix()
	recordTestMsg(t, s, ChatConsoleRecord{
		Kind: chatConsoleKindGroup, TargetID: "G1", AppID: "10001",
		SenderOpenID: "MEMBER1", SenderName: "许烁",
	}, "群消息", now)
	recordTestMsg(t, s, ChatConsoleRecord{
		Kind: chatConsoleKindC2C, TargetID: "U1", AppID: "10001",
		SenderOpenID: "USER1", SenderName: "小明",
	}, "私聊消息", now+5)

	convs := s.Conversations()
	if len(convs) != 2 {
		t.Fatalf("expect 2 conversations, got %d", len(convs))
	}
	// 默认按最后消息时间倒序：私聊更新
	if convs[0].Key != "c2c:U1" {
		t.Fatalf("unexpected order: %s first", convs[0].Key)
	}
	// 私聊会话头像用 openid 拼
	if convs[0].AvatarURL != "https://thirdqq.qlogo.cn/qqapp/10001/U1/0" {
		t.Fatalf("unexpected conversation avatar: %q", convs[0].AvatarURL)
	}
	// 群会话没有头像（群头像是群号另一套接口）
	if convs[1].AvatarURL != "" {
		t.Fatalf("group conversation should not have avatar: %q", convs[1].AvatarURL)
	}

	// 消息里的发送者头像
	msgs, _, _ := s.Messages("group:G1", 0, 50)
	if len(msgs) != 1 || msgs[0].AvatarURL != "https://thirdqq.qlogo.cn/qqapp/10001/MEMBER1/0" {
		t.Fatalf("unexpected message avatar: %+v", msgs)
	}

	// 实时推送（SSE）出去的那份消息也必须带头像：
	// 页面靠实时通道收新消息，之前没算头像 → 只有「名字首字」，刷新后才正常。
	pushed, err := s.Append(ChatConsoleRecord{
		Kind: chatConsoleKindGroup, TargetID: "G1", AppID: "10001",
		SenderOpenID: "MEMBER2", SenderName: "茶叶", Text: "实时消息", MessageID: "m-live",
	})
	if err != nil || pushed == nil {
		t.Fatalf("append live message: %v", err)
	}
	if want := "https://thirdqq.qlogo.cn/qqapp/10001/MEMBER2/0"; pushed.AvatarURL != want {
		t.Fatalf("pushed message avatar = %q, want %q", pushed.AvatarURL, want)
	}

	// 置顶后排到最前面
	pinned, err := s.SetPinned("group:G1", true)
	if err != nil || pinned == nil || !pinned.Pinned {
		t.Fatalf("pin failed: %+v %v", pinned, err)
	}
	convs = s.Conversations()
	if convs[0].Key != "group:G1" || !convs[0].Pinned {
		t.Fatalf("pinned conversation should be first: %+v", convs[0])
	}

	// 取消置顶后回到时间顺序
	if _, err := s.SetPinned("group:G1", false); err != nil {
		t.Fatalf("unpin failed: %v", err)
	}
	convs = s.Conversations()
	if convs[0].Key != "c2c:U1" {
		t.Fatalf("unpin should restore time order, got %s", convs[0].Key)
	}

	// 不存在的会话
	if _, err := s.SetPinned("group:NOPE", true); err == nil {
		t.Fatalf("expect error for unknown conversation")
	}
}

func TestOfficialQQGroupNameHelpers(t *testing.T) {
	uin, openID, ok := ParseOfficialQQGroupID("OpenQQ-Group:4019996761-EFD7AC450FE5BFC0DE42513")
	if !ok || uin != "4019996761" || openID != "EFD7AC450FE5BFC0DE42513" {
		t.Fatalf("unexpected parse: %q %q %v", uin, openID, ok)
	}
	for _, bad := range []string{"QQ-Group:1-2", "OpenQQ-Group:onlyuin", "OpenQQ-Group:-2", "OpenQQ:1-2"} {
		if _, _, ok := ParseOfficialQQGroupID(bad); ok {
			t.Fatalf("should not parse: %s", bad)
		}
	}

	// 控制台里已经有群名时应当直接回填
	d := &Dice{BaseConfig: BaseConfig{DataDir: t.TempDir()}}
	store := d.GetChatConsole()
	t.Cleanup(store.Close)
	if _, err := store.Append(ChatConsoleRecord{
		Kind: chatConsoleKindGroup, TargetID: "G1", UIN: "10001",
		Text: "hi", TS: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if got := OfficialQQGroupDisplayName(d, "OpenQQ-Group:10001-G1"); got != "" {
		t.Fatalf("placeholder name should not be used, got %q", got)
	}
	store.SetGroupInfo("group:G1", ChatConsoleGroupInfo{Name: "读书分享会", MemberCount: 256})
	if got := OfficialQQGroupDisplayName(d, "OpenQQ-Group:10001-G1"); got != "读书分享会" {
		t.Fatalf("expect console name, got %q", got)
	}
	// 非官方群 ID 不处理
	if got := OfficialQQGroupDisplayName(d, "QQ-Group:10001"); got != "" {
		t.Fatalf("non official group should return empty, got %q", got)
	}
}

// ---------------- 测试小工具 ----------------

func TestChatConsoleSearch(t *testing.T) {
	s := newTestChatConsoleStore(t)
	now := time.Now().Unix()
	groupOpenID := "74EF9046C98F864D1743E7A762736819"
	memberOpenID := "423C89672A253A4362ACBD00793062B8"

	rec := ChatConsoleRecord{
		Kind: chatConsoleKindGroup, TargetID: groupOpenID, UIN: "4019996761", AppID: "4019996761",
		SenderName: "鼠人自有大骨汤", SenderOpenID: memberOpenID,
		SenderID: "OpenQQ:4019996761-423C89672A253A4362ACBD00793062B8",
	}
	recordTestMsg(t, s, rec, "注意 这里有敏感词", now)
	recordTestMsg(t, s, rec, "普通的一句话", now+1)

	// 1) 直接搜 dice 平台群 ID（OpenQQ-Group:<uin>-<openid>）
	convs, _, err := s.Search("OpenQQ-Group:4019996761-74EF9046C98F864D1743E7A762736819", 10)
	if err != nil {
		t.Fatalf("search group id: %v", err)
	}
	if len(convs) != 1 || convs[0].TargetID != groupOpenID {
		t.Fatalf("dice group id should match conversation, got %+v", convs)
	}

	// 2) 搜 dice 平台用户 ID（OpenQQ:<uin>-<member openid>）
	_, msgs, err := s.Search("OpenQQ:4019996761-423C89672A253A4362ACBD00793062B8", 10)
	if err != nil {
		t.Fatalf("search user id: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("user id should match both of his messages, got %d", len(msgs))
	}

	// 3) 搜用户昵称
	convs, msgs, _ = s.Search("鼠人自有大骨汤", 10)
	if len(msgs) != 2 {
		t.Fatalf("nickname should match the messages, got %d", len(msgs))
	}
	if len(convs) == 0 {
		t.Fatalf("nickname should also resolve the conversation")
	}

	// 4) 搜聊天记录关键词
	_, msgs, _ = s.Search("敏感词", 10)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "敏感词") {
		t.Fatalf("keyword should match one message, got %+v", msgs)
	}
	if msgs[0].ConversationName == "" || msgs[0].ConversationKey == "" {
		t.Fatalf("search message should carry its conversation: %+v", msgs[0])
	}

	// 5) LIKE 通配符要被转义，不能全表命中
	if _, msgs, _ := s.Search("%", 10); len(msgs) != 0 {
		t.Fatalf("wildcard should be escaped, got %d messages", len(msgs))
	}

	// 6) 空搜索
	if convs, msgs, _ := s.Search("   ", 10); len(convs) != 0 || len(msgs) != 0 {
		t.Fatalf("empty query should return nothing")
	}
}

func TestChatConsoleGroupProfile(t *testing.T) {
	s := newTestChatConsoleStore(t)
	recordTestMsg(t, s, ChatConsoleRecord{Kind: chatConsoleKindGroup, TargetID: "G9"}, "hi", 1000)

	conv := s.SetGroupInfo("group:G9", ChatConsoleGroupInfo{
		Name:        "跑团集合地",
		MemberCount: 88,
		FingerMemo:  "每周三晚上八点开车，禁止剧透。",
		ClassText:   "游戏/桌游",
		Tags:        []string{"跑团", "COC"},
	})
	if conv == nil {
		t.Fatalf("group info not applied")
	}
	if conv.FingerMemo != "每周三晚上八点开车，禁止剧透。" || conv.ClassText != "游戏/桌游" ||
		len(conv.Tags) != 2 || conv.Tags[0] != "跑团" || conv.Tags[1] != "COC" {
		t.Fatalf("profile fields lost: %+v", conv)
	}

	// 重新打开库后简介 / 分类 / 标签要能读回来（存的是 JSON 列）
	s.Flush()
	dir := s.Dir()
	s.Close()

	reopened := NewChatConsoleStore(dir)
	t.Cleanup(reopened.Close)

	again := reopened.Conversation("group:G9")
	if again == nil || again.FingerMemo != "每周三晚上八点开车，禁止剧透。" || again.ClassText != "游戏/桌游" || len(again.Tags) != 2 {
		t.Fatalf("profile fields not persisted: %+v", again)
	}

	// 没连官方接口时，手动刷新要给出可读的错误而不是崩
	if _, err := RefreshChatConsoleGroupInfo(nil, "group:G9"); err == nil {
		t.Fatalf("nil dice should return an error")
	}
}

type testError string

func (e testError) Error() string { return string(e) }

func errTest(s string) error { return testError(s) }

// ---------------- 控制台发送：CQ 码（@ / 引用） ----------------

// newCQTestAdapter 造一个带控制台存储的官方 QQ 适配器（不连真实接口）。
func newCQTestAdapter(t *testing.T) (*PlatformAdapterOfficialQQ, *ChatConsoleStore, *Dice) {
	t.Helper()
	d := &Dice{BaseConfig: BaseConfig{DataDir: t.TempDir()}, ImSession: &IMSession{}}
	d.ImSession.Parent = d
	store := d.GetChatConsole()
	t.Cleanup(store.Close)
	pa := &PlatformAdapterOfficialQQ{
		UIN:      "10001",
		EndPoint: &EndPointInfo{EndPointInfoBase: EndPointInfoBase{ID: "ep1", Session: d.ImSession}},
	}
	return pa, store, d
}

func TestChatConsoleParseCQ(t *testing.T) {
	pa := &PlatformAdapterOfficialQQ{UIN: "10001"}

	// [CQ:at,qq=...] -> 官方 @ 标签，uin 前缀会被去掉
	text, replyID, err := pa.chatConsoleParseCQ(chatConsoleKindGroup, "[CQ:at,qq=10001-MEMBER-OPENID] 你好")
	if err != nil {
		t.Fatalf("parse at: %v", err)
	}
	if replyID != "" {
		t.Fatalf("不该有 reply id: %q", replyID)
	}
	if !strings.Contains(text, `<qqbot-at-user id="MEMBER-OPENID" />`) {
		t.Fatalf("at tag missing: %q", text)
	}
	if !strings.HasSuffix(text, "你好") {
		t.Fatalf("正文顺序不对: %q", text)
	}

	// @全体
	text, _, err = pa.chatConsoleParseCQ(chatConsoleKindGroup, "[CQ:at,qq=all] 集合")
	if err != nil || !strings.Contains(text, "<qqbot-at-everyone />") {
		t.Fatalf("at all: %q, %v", text, err)
	}

	// 私聊没有 @，但 reply 要保留
	text, replyID, err = pa.chatConsoleParseCQ(chatConsoleKindC2C, "[CQ:at,qq=user-openid][CQ:reply,id=MSG-1] 在")
	if err != nil {
		t.Fatalf("parse c2c: %v", err)
	}
	if replyID != "MSG-1" {
		t.Fatalf("reply id = %q, want MSG-1", replyID)
	}
	if strings.Contains(text, "qqbot-at-user") || !strings.Contains(text, "在") {
		t.Fatalf("私聊不该发 @ 标签: %q", text)
	}

	// 富媒体 CQ 码在控制台没有上传通道，要明确报错
	if _, _, err := pa.chatConsoleParseCQ(chatConsoleKindGroup, "[CQ:image,file=a.png]"); err == nil {
		t.Fatal("图片 CQ 码应该报错")
	}

	// 普通文本原样保留
	if text, _, err := pa.chatConsoleParseCQ(chatConsoleKindGroup, "1d100 侦查"); err != nil || text != "1d100 侦查" {
		t.Fatalf("纯文本被改动: %q, %v", text, err)
	}
}

func TestChatConsoleBuildSendCQReply(t *testing.T) {
	pa, _, _ := newCQTestAdapter(t)

	payload, err := pa.chatConsoleBuildSend(
		chatConsoleKindGroup, "GROUP-1", "[CQ:reply,id=MSG-9] 收到", ChatConsoleSendOptions{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	toCreate := payload.toCreate
	if toCreate.MsgID != "MSG-9" {
		t.Fatalf("MsgID = %q, want MSG-9（真引用要走被动回复）", toCreate.MsgID)
	}
	if toCreate.MessageReference == nil || toCreate.MessageReference.MessageID != "MSG-9" {
		t.Fatalf("message_reference 没挂上: %+v", toCreate.MessageReference)
	}
	if strings.Contains(payload.content(), "「回复") {
		t.Fatalf("真引用不该再加正文前缀: %q", payload.content())
	}
	if payload.displayText != "收到" {
		t.Fatalf("display = %q, want 收到", payload.displayText)
	}
	quote := payload.quote
	if quote == nil || quote.MsgID != "MSG-9" {
		t.Fatalf("引用信息丢了: %+v", quote)
	}
}

func TestChatConsoleBuildSendQuoteWindow(t *testing.T) {
	pa, store, _ := newCQTestAdapter(t)
	conv := ChatConsoleRecord{
		Kind: chatConsoleKindGroup, TargetID: "GROUP-1",
		Dir: chatConsoleDirIn, Source: chatConsoleSourceUser,
		SenderName: "许烁", SenderOpenID: "MEMBER-1",
	}

	// 刚收到的消息：能做真引用
	freshConv := conv
	freshConv.MessageID = "MSG-IN-FRESH"
	fresh := recordTestMsg(t, store, freshConv, "骰个侦查", time.Now().Unix())
	payload, err := pa.chatConsoleBuildSend(chatConsoleKindGroup, "GROUP-1", "收到",
		ChatConsoleSendOptions{Quote: &ChatConsoleQuote{Sender: "许烁", Text: "骰个侦查", MsgID: fresh.MsgID}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	toCreate := payload.toCreate
	if toCreate.MsgID != "MSG-IN-FRESH" {
		t.Fatalf("窗口内的消息应该能被真引用，MsgID = %q", toCreate.MsgID)
	}
	if toCreate.MessageReference == nil || toCreate.MessageReference.MessageID != "MSG-IN-FRESH" {
		t.Fatalf("窗口内的消息应该挂 message_reference: %+v", toCreate.MessageReference)
	}
	if strings.Contains(payload.content(), "「回复") {
		t.Fatalf("真引用不该再加正文前缀: %q", payload.content())
	}
	if payload.displayText != "收到" || payload.quote == nil || payload.quote.MsgID != "MSG-IN-FRESH" {
		t.Fatalf("展示/引用信息异常: display=%q quote=%+v", payload.displayText, payload.quote)
	}

	// 超出被动回复窗口：退回伪引用
	staleConv := conv
	staleConv.MessageID = "MSG-IN-STALE"
	stale := recordTestMsg(t, store, staleConv, "旧消息", time.Now().Add(-30*time.Minute).Unix())
	payload, err = pa.chatConsoleBuildSend(chatConsoleKindGroup, "GROUP-1", "收到",
		ChatConsoleSendOptions{Quote: &ChatConsoleQuote{Sender: "许烁", Text: "旧消息", MsgID: stale.MsgID}})
	if err != nil {
		t.Fatalf("build stale: %v", err)
	}
	toCreate = payload.toCreate
	if toCreate.MsgID != "" || toCreate.MessageReference != nil {
		t.Fatalf("超出窗口不该挂 msg_id: %+v", toCreate)
	}
	if !strings.Contains(payload.content(), "「回复 许烁：旧消息」") {
		t.Fatalf("超出窗口应退回正文前缀: %q", payload.content())
	}

	// 机器人自己发的消息不能被"引用回复"
	out := conv
	out.Dir = chatConsoleDirOut
	out.Source = chatConsoleSourceBot
	out.MessageID = "BOT-OUT-1"
	botMsg := recordTestMsg(t, store, out, "我发的", time.Now().Unix())
	payload, err = pa.chatConsoleBuildSend(chatConsoleKindGroup, "GROUP-1", "收到",
		ChatConsoleSendOptions{Quote: &ChatConsoleQuote{MsgID: botMsg.MsgID}})
	if err != nil {
		t.Fatalf("build bot msg: %v", err)
	}
	if payload.toCreate.MsgID != "" {
		t.Fatal("不该拿机器人自己的消息当被动回复目标")
	}
}

// TestChatConsoleBuildSendMarkdownForAt 回归：<qqbot-at-user> 只有在 markdown 消息里才被 QQ 解析，
// 纯文本会把标签原样显示出来（“<qqbot-at-user id=... />耶耶耶”）。
func TestChatConsoleBuildSendMarkdownForAt(t *testing.T) {
	pa, _, d := newCQTestAdapter(t)
	d.Config.OfficialQQUseMarkdown = false

	// 带 @：即使没开 md 也必须走 markdown
	payload, err := pa.chatConsoleBuildSend(chatConsoleKindGroup, "GROUP-1", "[CQ:at,qq=MEMBER-1] 耶耶耶", ChatConsoleSendOptions{})
	if err != nil {
		t.Fatalf("build at: %v", err)
	}
	if payload.toCreate.MsgType != 2 || payload.toCreate.Markdown == nil {
		t.Fatalf("带 @ 的消息必须走 markdown，MsgType=%d", payload.toCreate.MsgType)
	}
	if !strings.Contains(payload.content(), `<qqbot-at-user id="MEMBER-1" />`) {
		t.Fatalf("markdown 正文里缺 @ 标签: %q", payload.content())
	}
	if payload.toCreate.Content != "" {
		t.Fatalf("markdown 模式下不该再塞 Content: %q", payload.toCreate.Content)
	}

	// 不带 @、md 关闭：保持纯文本
	payload, err = pa.chatConsoleBuildSend(chatConsoleKindGroup, "GROUP-1", "耶耶耶", ChatConsoleSendOptions{})
	if err != nil {
		t.Fatalf("build text: %v", err)
	}
	if payload.toCreate.MsgType != 0 || payload.toCreate.Markdown != nil || payload.toCreate.Content != "耶耶耶" {
		t.Fatalf("不该走 markdown: %+v", payload.toCreate)
	}

	// 不带 @、md 开启：跟随设置
	d.Config.OfficialQQUseMarkdown = true
	payload, err = pa.chatConsoleBuildSend(chatConsoleKindGroup, "GROUP-1", "耶耶耶", ChatConsoleSendOptions{})
	if err != nil {
		t.Fatalf("build md: %v", err)
	}
	if payload.toCreate.MsgType != 2 || payload.toCreate.Markdown == nil {
		t.Fatalf("开启 md 后应该跟随设置: %+v", payload.toCreate)
	}
}

func TestChatConsoleBuildSendRejectsMediaCQ(t *testing.T) {
	pa, _, _ := newCQTestAdapter(t)
	if _, err := pa.chatConsoleBuildSend(chatConsoleKindGroup, "GROUP-1", "[CQ:file,file=a.txt]", ChatConsoleSendOptions{}); err == nil {
		t.Fatal("文件 CQ 码应该报错，提示用上传按钮")
	}
}
