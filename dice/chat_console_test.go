package dice //nolint:testpackage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestChatConsoleStore(t *testing.T) *ChatConsoleStore {
	t.Helper()
	return NewChatConsoleStore(filepath.Join(t.TempDir(), "chat-console"))
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

	s2 := NewChatConsoleStore(dir)
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
	dir := filepath.Join(t.TempDir(), "chat-console")
	s := NewChatConsoleStore(dir)
	recordTestMsg(t, s, ChatConsoleRecord{Kind: chatConsoleKindGroup, TargetID: "G1"}, "新消息", time.Now().Unix())

	convDir := filepath.Join(dir, "group_G1")
	// 造一个远超保留期的日文件
	old := filepath.Join(convDir, "2000-01-01.jsonl")
	if err := os.WriteFile(old, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write old file: %v", err)
	}

	// 重新加载触发清理
	s2 := NewChatConsoleStore(dir)
	_ = s2.Conversations()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old day file should be pruned, err=%v", err)
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

// ---------------- 测试小工具 ----------------

type testError string

func (e testError) Error() string { return string(e) }

func errTest(s string) error { return testError(s) }
