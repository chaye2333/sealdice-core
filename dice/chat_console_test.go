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
	if c := s.SetGroupInfo("group:G1", "读书分享会", 256); c == nil || c.Name != "读书分享会" || c.MemberCount != 256 {
		t.Fatalf("set group info failed: %+v", c)
	}

	// 手动改名后不再被官方群名覆盖，但成员数仍会更新
	if c, err := s.Rename("group:G1", "我的群"); err != nil || c.Name != "我的群" || !c.NameManual {
		t.Fatalf("rename failed: %+v %v", c, err)
	}
	if c := s.SetGroupInfo("group:G1", "官方群名", 300); c == nil || c.Name != "我的群" || c.MemberCount != 300 {
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

// ---------------- 测试小工具 ----------------

type testError string

func (e testError) Error() string { return string(e) }

func errTest(s string) error { return testError(s) }
