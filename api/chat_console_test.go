package api //nolint:testpackage

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"sealdice-core/dice"
)

// chatConsoleTestEnv 搭一套最小可用的 dice + 路由环境。
func chatConsoleTestEnv(t *testing.T) (*echo.Echo, string) {
	t.Helper()

	token := "chat-console-test-token"
	manager := &dice.DiceManager{}
	manager.AccessTokens.Store(token, true)

	testDice := &dice.Dice{
		Parent:     manager,
		BaseConfig: dice.BaseConfig{DataDir: t.TempDir()},
		ImSession:  &dice.IMSession{},
	}

	oldDice, oldDM := myDice, dm
	dm, myDice = manager, testDice
	t.Cleanup(func() { myDice, dm = oldDice, oldDM })

	store := testDice.GetChatConsole()
	t.Cleanup(store.Close)
	now := time.Now().Unix()
	for _, rec := range []dice.ChatConsoleRecord{
		{Kind: "group", TargetID: "G1", UIN: "10001", SenderID: "OpenQQ-Member:1", SenderName: "许烁", Dir: "in", Source: "user", Text: "骰个侦查", MessageID: "m1", TS: now - 10},
		{Kind: "group", TargetID: "G1", UIN: "10001", SenderID: "OpenQQ:bot", SenderName: "骰子", Dir: "out", Source: "bot", Text: "侦查 45/60 成功", MessageID: "m2", TS: now - 5},
	} {
		if _, err := store.Append(rec); err != nil {
			t.Fatalf("seed append: %v", err)
		}
	}

	e := echo.New()
	e.GET("/sd-api/chat-console/conversations", chatConsoleConversations)
	e.GET("/sd-api/chat-console/messages", chatConsoleMessages)
	e.POST("/sd-api/chat-console/send", chatConsoleSend)
	e.POST("/sd-api/chat-console/read", chatConsoleRead)
	e.POST("/sd-api/chat-console/rename", chatConsoleRename)
	e.POST("/sd-api/chat-console/recall", chatConsoleRecall)
	e.POST("/sd-api/chat-console/mute", chatConsoleMute)
	e.POST("/sd-api/chat-console/upload", chatConsoleUpload)
	e.GET("/sd-api/chat-console/storage", chatConsoleStorage)
	e.POST("/sd-api/chat-console/settings", chatConsoleSettingsSave)
	e.POST("/sd-api/chat-console/purge", chatConsolePurge)
	return e, token
}

func chatConsoleDo(t *testing.T, e *echo.Echo, method, path, token, body string) map[string]interface{} {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	if token != "" {
		req.Header.Set("Token", token)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: decode body failed: %v (body=%q)", method, path, err, rec.Body.String())
	}
	return out
}

func TestChatConsoleAPIRequiresAuth(t *testing.T) {
	e, _ := chatConsoleTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/sd-api/chat-console/conversations", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expect 403 without token, got %d", rec.Code)
	}
}

func TestChatConsoleAPIConversationsAndMessages(t *testing.T) {
	e, token := chatConsoleTestEnv(t)

	res := chatConsoleDo(t, e, http.MethodGet, "/sd-api/chat-console/conversations", token, "")
	if res["result"] != true {
		t.Fatalf("conversations failed: %+v", res)
	}
	convs, _ := res["conversations"].([]interface{})
	if len(convs) != 1 {
		t.Fatalf("expect 1 conversation, got %d", len(convs))
	}
	conv, _ := convs[0].(map[string]interface{})
	if conv["key"] != "group:G1" || conv["name"] != "群 G1" {
		t.Fatalf("unexpected conversation: %+v", conv)
	}
	if conv["unread"].(float64) != 1 {
		t.Fatalf("expect unread=1 (only inbound counts), got %v", conv["unread"])
	}

	res = chatConsoleDo(t, e, http.MethodGet, "/sd-api/chat-console/messages?key=group:G1&limit=50", token, "")
	if res["result"] != true {
		t.Fatalf("messages failed: %+v", res)
	}
	msgs, _ := res["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("expect 2 messages, got %d", len(msgs))
	}
	first, _ := msgs[0].(map[string]interface{})
	if first["text"] != "骰个侦查" || first["source"] != "user" {
		t.Fatalf("unexpected first message: %+v", first)
	}

	// 非法 key
	res = chatConsoleDo(t, e, http.MethodGet, "/sd-api/chat-console/messages?key=bad", token, "")
	if res["result"] != false {
		t.Fatalf("expect failure for bad key: %+v", res)
	}
}

func TestChatConsoleAPIReadAndRename(t *testing.T) {
	e, token := chatConsoleTestEnv(t)

	res := chatConsoleDo(t, e, http.MethodPost, "/sd-api/chat-console/read", token, `{"key":"group:G1"}`)
	if res["result"] != true {
		t.Fatalf("read failed: %+v", res)
	}

	res = chatConsoleDo(t, e, http.MethodPost, "/sd-api/chat-console/rename", token, `{"key":"group:G1","name":"读书分享会"}`)
	if res["result"] != true {
		t.Fatalf("rename failed: %+v", res)
	}
	conv, _ := res["conversation"].(map[string]interface{})
	if conv["name"] != "读书分享会" || conv["nameManual"] != true {
		t.Fatalf("unexpected renamed conversation: %+v", conv)
	}

	res = chatConsoleDo(t, e, http.MethodPost, "/sd-api/chat-console/rename", token, `{"key":"group:G9","name":"x"}`)
	if res["result"] != false {
		t.Fatalf("expect failure for unknown conversation: %+v", res)
	}
}

func TestChatConsoleAPISendWithoutAdapter(t *testing.T) {
	e, token := chatConsoleTestEnv(t)

	res := chatConsoleDo(t, e, http.MethodPost, "/sd-api/chat-console/send", token, `{"key":"group:G1","text":"hi"}`)
	if res["result"] != false {
		t.Fatalf("expect failure without official qq endpoint: %+v", res)
	}
	if msg, _ := res["err"].(string); !strings.Contains(msg, "官方 QQ") {
		t.Fatalf("unexpected error message: %v", res["err"])
	}

	res = chatConsoleDo(t, e, http.MethodPost, "/sd-api/chat-console/send", token, `{"key":"group:G1","text":"  "}`)
	if res["result"] != false {
		t.Fatalf("expect failure for empty text: %+v", res)
	}
}

func TestChatConsoleAPIRecall(t *testing.T) {
	e, token := chatConsoleTestEnv(t)

	// 收到的消息不允许撤回
	res := chatConsoleDo(t, e, http.MethodPost, "/sd-api/chat-console/recall", token, `{"key":"group:G1","msgId":"m1"}`)
	if res["result"] != false || !strings.Contains(res["err"].(string), "只能撤回机器人") {
		t.Fatalf("expect inbound recall to be rejected: %+v", res)
	}

	// 不存在的消息
	res = chatConsoleDo(t, e, http.MethodPost, "/sd-api/chat-console/recall", token, `{"key":"group:G1","msgId":"nope"}`)
	if res["result"] != false || !strings.Contains(res["err"].(string), "没有找到") {
		t.Fatalf("expect unknown message rejected: %+v", res)
	}

	// 机器人自己发的消息，但没有可用端点
	res = chatConsoleDo(t, e, http.MethodPost, "/sd-api/chat-console/recall", token, `{"key":"group:G1","msgId":"m2"}`)
	if res["result"] != false || !strings.Contains(res["err"].(string), "官方 QQ") {
		t.Fatalf("expect failure without endpoint: %+v", res)
	}
}

func TestChatConsoleAPIMute(t *testing.T) {
	e, token := chatConsoleTestEnv(t)

	// 私聊不支持禁言
	res := chatConsoleDo(t, e, http.MethodPost, "/sd-api/chat-console/mute", token, `{"key":"c2c:U1","memberOpenId":"x","seconds":60}`)
	if res["result"] != false || !strings.Contains(res["err"].(string), "只有群聊") {
		t.Fatalf("expect c2c mute rejected: %+v", res)
	}

	// 缺少成员 openid
	res = chatConsoleDo(t, e, http.MethodPost, "/sd-api/chat-console/mute", token, `{"key":"group:G1","seconds":60}`)
	if res["result"] != false {
		t.Fatalf("expect missing member rejected: %+v", res)
	}

	// 群聊禁言，但没有可用端点
	res = chatConsoleDo(t, e, http.MethodPost, "/sd-api/chat-console/mute", token, `{"key":"group:G1","memberOpenId":"x","seconds":60}`)
	if res["result"] != false || !strings.Contains(res["err"].(string), "官方 QQ") {
		t.Fatalf("expect failure without endpoint: %+v", res)
	}
}

func TestChatConsoleAPIUploadWithoutFile(t *testing.T) {
	e, token := chatConsoleTestEnv(t)

	req := httptest.NewRequest(http.MethodPost, "/sd-api/chat-console/upload", strings.NewReader(""))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	req.Header.Set("Token", token)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	var res map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode body failed: %v", err)
	}
	if res["result"] != false {
		t.Fatalf("expect failure without file: %+v", res)
	}
}

func TestChatConsoleAPIStorage(t *testing.T) {
	e, token := chatConsoleTestEnv(t)

	res := chatConsoleDo(t, e, http.MethodGet, "/sd-api/chat-console/storage", token, "")
	if res["result"] != true {
		t.Fatalf("storage failed: %+v", res)
	}
	if res["retentionDays"].(float64) != 30 {
		t.Fatalf("default retention should be 30, got %v", res["retentionDays"])
	}
	if res["totalBytes"].(float64) <= 0 {
		t.Fatalf("total bytes should be > 0: %+v", res["totalBytes"])
	}
	items, _ := res["conversations"].([]interface{})
	found := false
	for _, it := range items {
		m := it.(map[string]interface{})
		if m["key"] == "group:G1" {
			found = true
			if m["messages"].(float64) != 2 {
				t.Fatalf("group messages = %v, want 2", m["messages"])
			}
		}
	}
	if !found {
		t.Fatalf("group:G1 missing in usage: %+v", items)
	}
}

func TestChatConsoleAPISettingsAndPurge(t *testing.T) {
	e, token := chatConsoleTestEnv(t)

	res := chatConsoleDo(t, e, http.MethodPost, "/sd-api/chat-console/settings", token, `{"retentionDays":7}`)
	if res["result"] != true || res["retentionDays"].(float64) != 7 {
		t.Fatalf("save settings failed: %+v", res)
	}

	res = chatConsoleDo(t, e, http.MethodPost, "/sd-api/chat-console/settings", token, `{"retentionDays":-1}`)
	if res["result"] != false {
		t.Fatalf("negative retention should be rejected: %+v", res)
	}

	// 清空单个会话
	res = chatConsoleDo(t, e, http.MethodPost, "/sd-api/chat-console/purge", token, `{"key":"group:G1"}`)
	if res["result"] != true {
		t.Fatalf("purge conversation failed: %+v", res)
	}
	res = chatConsoleDo(t, e, http.MethodGet, "/sd-api/chat-console/messages?key=group:G1", token, "")
	if msgs, _ := res["messages"].([]interface{}); len(msgs) != 0 {
		t.Fatalf("expect empty history after purge, got %d", len(msgs))
	}

	// 缺 key 也不带 all 时报错
	res = chatConsoleDo(t, e, http.MethodPost, "/sd-api/chat-console/purge", token, `{}`)
	if res["result"] != false {
		t.Fatalf("expect failure without key or all: %+v", res)
	}

	// 清空全部
	res = chatConsoleDo(t, e, http.MethodPost, "/sd-api/chat-console/purge", token, `{"all":true}`)
	if res["result"] != true {
		t.Fatalf("purge all failed: %+v", res)
	}
	res = chatConsoleDo(t, e, http.MethodGet, "/sd-api/chat-console/conversations", token, "")
	if convs, _ := res["conversations"].([]interface{}); len(convs) != 0 {
		t.Fatalf("expect no conversations after purge all, got %d", len(convs))
	}
}
