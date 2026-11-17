//nolint:testpackage
package dice

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/oauth2"
)

func newChatConsoleInfoTestAdapter(apiBase, appID string) *PlatformAdapterOfficialQQ {
	return &PlatformAdapterOfficialQQ{
		AppID:             appID,
		tokenSource:       oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token", TokenType: "Bearer"}),
		apiDomainOverride: apiBase,
	}
}

// 官方 /v2/users/{openid}/info 的解析：username / allow_proactive_msg 都要拿到，
// 头像不使用接口返回的签名地址（仍然用 AppID+openid 拼 qlogo）。
func TestFetchChatConsoleUserInfo(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		_, _ = w.Write([]byte(`{"union_openid":"u1","username":"笑心","avatar":"https://thirdqq.qlogo.cn/qqapp/x/y/0","allow_proactive_msg":false}`))
	}))
	defer server.Close()

	pa := newChatConsoleInfoTestAdapter(server.URL, "app1")
	info, err := pa.fetchChatConsoleUserInfo("openid-1")
	if err != nil {
		t.Fatalf("fetchChatConsoleUserInfo: %v", err)
	}
	if gotPath != "/v2/users/openid-1/info" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("unexpected authorization: %q", gotAuth)
	}
	if gotBody != "" {
		t.Fatalf("GET 请求不应带 body: %q", gotBody)
	}
	if info.Name != "笑心" || info.UnionOpenID != "u1" {
		t.Fatalf("unexpected info: %+v", info)
	}
	if info.AllowProactiveMsg == nil || *info.AllowProactiveMsg {
		t.Fatalf("allow_proactive_msg 应解析为 false: %+v", info.AllowProactiveMsg)
	}
}

func TestFetchChatConsoleUserInfoEmptyPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"error":"40184001 无好友关系"}`))
	}))
	defer server.Close()

	pa := newChatConsoleInfoTestAdapter(server.URL, "app1")
	if _, err := pa.fetchChatConsoleUserInfo("openid-1"); err == nil {
		t.Fatal("没有可用字段时应返回错误")
	}
}

// 群资料也改成走同一套签名请求（apiDomainOverride 可指向测试服务器）。
func TestFetchChatConsoleGroupInfoUsesSignedRequest(t *testing.T) {
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"group_name":"跑团集合地","group_member_num":88}`))
	}))
	defer server.Close()

	pa := newChatConsoleInfoTestAdapter(server.URL, "app1")
	info, err := pa.fetchChatConsoleGroupInfo("group-openid-1")
	if err != nil {
		t.Fatalf("fetchChatConsoleGroupInfo: %v", err)
	}
	if gotPath != "/v2/groups/group-openid-1/info" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("unexpected authorization: %q", gotAuth)
	}
	if info.Name != "跑团集合地" || info.MemberCount != 88 {
		t.Fatalf("unexpected info: %+v", info)
	}
}

// 用户资料要能落到会话上：用户名覆盖占位名、allow_proactive_msg 保存并持久化；
// 手动改过名的会话不被覆盖，但标志仍然更新。
func TestChatConsoleUserProfile(t *testing.T) {
	s := newTestChatConsoleStore(t)
	recordTestMsg(t, s, ChatConsoleRecord{Kind: chatConsoleKindC2C, TargetID: "U9"}, "hi", 1000)

	if conv := s.Conversation("c2c:U9"); conv == nil || conv.Name != "私聊 U9" {
		t.Fatalf("unexpected default conversation: %+v", conv)
	}

	allow := false
	conv := s.SetUserInfo("c2c:U9", ChatConsoleUserInfo{Name: "笑心", AllowProactiveMsg: &allow})
	if conv == nil || conv.Name != "笑心" || conv.AllowProactiveMsg == nil || *conv.AllowProactiveMsg {
		t.Fatalf("set user info failed: %+v", conv)
	}

	s.Flush()
	dir := s.Dir()
	s.Close()

	reopened := NewChatConsoleStore(dir)
	t.Cleanup(reopened.Close)
	again := reopened.Conversation("c2c:U9")
	if again == nil || again.Name != "笑心" || again.AllowProactiveMsg == nil || *again.AllowProactiveMsg {
		t.Fatalf("user info not persisted: %+v", again)
	}

	if _, err := reopened.Rename("c2c:U9", "我的好友"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	allowTrue := true
	updated := reopened.SetUserInfo("c2c:U9", ChatConsoleUserInfo{Name: "官方名", AllowProactiveMsg: &allowTrue})
	if updated == nil || updated.Name != "我的好友" {
		t.Fatalf("手动改的名字被覆盖了: %+v", updated)
	}
	if updated.AllowProactiveMsg == nil || !*updated.AllowProactiveMsg {
		t.Fatalf("allow_proactive_msg 没有更新: %+v", updated.AllowProactiveMsg)
	}
}
