//nolint:testpackage // 测试需要访问包内的缓存探测函数和状态类型。
package storylog

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

func TestProbeCachedLogURL(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		want       cachedLogProbeResult
	}{
		{name: "alive", statusCode: http.StatusOK, want: cachedLogProbeAlive},
		{name: "missing", statusCode: http.StatusNotFound, want: cachedLogProbeMissing},
		{name: "gone", statusCode: http.StatusGone, want: cachedLogProbeMissing},
		{name: "server error", statusCode: http.StatusInternalServerError, want: cachedLogProbeUnknown},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/custom/backend/load_data" {
					t.Fatalf("path = %q, want /custom/backend/load_data", r.URL.Path)
				}
				if got := r.URL.Query().Get("key"); got != "AbCd" {
					t.Fatalf("key = %q, want AbCd", got)
				}
				if got := r.URL.Query().Get("password"); got != "123456" {
					t.Fatalf("password = %q, want 123456", got)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer token" {
					t.Fatalf("Authorization = %q, want Bearer token", got)
				}
				w.WriteHeader(test.statusCode)
			}))
			defer server.Close()

			env := UploadEnv{
				Backends: []string{server.URL + "/custom/backend/log"},
				Token:    "token",
				Log:      zap.NewNop().Sugar(),
			}
			if got := probeCachedLogURL(env, "https://logs.example/?key=AbCd#123456"); got != test.want {
				t.Fatalf("probeCachedLogURL() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestProbeCachedLogURLNetworkFailureIsUnknown(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	backend := server.URL + "/dice/api/log"
	server.Close()

	env := UploadEnv{
		Backends: []string{backend},
		Log:      zap.NewNop().Sugar(),
	}
	if got := probeCachedLogURL(env, "https://logs.example/?key=AbCd#123456"); got != cachedLogProbeUnknown {
		t.Fatalf("probeCachedLogURL() = %v, want %v", got, cachedLogProbeUnknown)
	}
}

func TestProbeCachedLogURLRejectsUnusableCachedURL(t *testing.T) {
	env := UploadEnv{Log: zap.NewNop().Sugar()}
	if got := probeCachedLogURL(env, "https://logs.example/"); got != cachedLogProbeMissing {
		t.Fatalf("probeCachedLogURL() = %v, want %v", got, cachedLogProbeMissing)
	}
}

// TestProbeCachedLogURLTreatsSPAFallbackAsUnknown 染色器前端是 SPA 时，
// 任意路径（包括错误的 key、不存在的接口）都会返回 200/206 + index.html。
// 这种响应不能当成"链接仍然有效"，否则永远不会触发重新上传。
func TestProbeCachedLogURLTreatsSPAFallbackAsUnknown(t *testing.T) {
	cases := []struct {
		name        string
		statusCode  int
		contentType string
		body        string
	}{
		{name: "206 html", statusCode: http.StatusPartialContent, contentType: "text/html", body: "<!doctype html><html></html>"},
		{name: "200 html", statusCode: http.StatusOK, contentType: "text/html; charset=utf-8", body: "<!doctype html><html></html>"},
		{name: "no content type but html body", statusCode: http.StatusOK, body: "<!doctype html>"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.contentType != "" {
					w.Header().Set("Content-Type", test.contentType)
				}
				w.WriteHeader(test.statusCode)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			env := UploadEnv{
				Backends: []string{server.URL + "/dice/api/log"},
				Log:      zap.NewNop().Sugar(),
			}
			if got := probeCachedLogURL(env, "https://logs.example/?key=AbCd#123456"); got != cachedLogProbeUnknown {
				t.Fatalf("probeCachedLogURL() = %v, want %v", got, cachedLogProbeUnknown)
			}
		})
	}
}

// TestProbeCachedLogURLKeepsJSONAlive 正常后端返回 JSON 时仍然算"链接有效"。
func TestProbeCachedLogURLKeepsJSONAlive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(`{"items":[{"name":"init"}]}`))
	}))
	defer server.Close()

	env := UploadEnv{
		Backends: []string{server.URL + "/dice/api/log"},
		Log:      zap.NewNop().Sugar(),
	}
	if got := probeCachedLogURL(env, "https://logs.example/?key=AbCd#123456"); got != cachedLogProbeAlive {
		t.Fatalf("probeCachedLogURL() = %v, want %v", got, cachedLogProbeAlive)
	}
}
