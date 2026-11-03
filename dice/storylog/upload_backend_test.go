//nolint:testpackage // 测试需要访问包内的上传函数。
package storylog

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
)

// TestPostLogToBackendSurfacesPlatformError 染色器后端被边缘平台顶死时（例如 EdgeOne 的
// 544 "Error return from script"），海豹必须把状态码和响应体一起报出来，而不是只说
// "返回结果异常"，否则完全没法判断到底是 413、544 还是别的问题。
func TestPostLogToBackendSurfacesPlatformError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("method = %s, want PUT", r.Method)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		w.WriteHeader(544)
		_, _ = w.Write([]byte("Error return from script"))
	}))
	defer server.Close()

	env := UploadEnv{Log: zap.NewNop().Sugar(), LogName: "log", UniformID: "QQ:10001", Version: StoryVersionV1}
	url, reason := postLogToBackend(env, server.URL, []byte("payload"), "SealDice")
	if url != "" {
		t.Fatalf("url = %q, want empty", url)
	}
	if !strings.Contains(reason, "544") {
		t.Fatalf("reason = %q, want it to mention HTTP 544", reason)
	}
	if !strings.Contains(reason, "Error return from script") {
		t.Fatalf("reason = %q, want it to include the backend body", reason)
	}
}

func TestPostLogToBackendReturnsURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"url":"https://logs.example/?key=AbCd#123456"}`))
	}))
	defer server.Close()

	env := UploadEnv{Log: zap.NewNop().Sugar(), LogName: "log", UniformID: "QQ:10001", Version: StoryVersionV1}
	url, reason := postLogToBackend(env, server.URL, []byte("payload"), "SealDice")
	if url != "https://logs.example/?key=AbCd#123456" {
		t.Fatalf("url = %q", url)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want empty", reason)
	}
}

// TestPostLogToBackendDetectsFrontendInsteadOfBackend 后端地址误填成前端地址时，
// 染色器前端会把任意路径都渲染成 200 + index.html，这里要能看出问题。
func TestPostLogToBackendDetectsFrontendInsteadOfBackend(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!DOCTYPE html><html></html>"))
	}))
	defer server.Close()

	env := UploadEnv{Log: zap.NewNop().Sugar(), LogName: "log", UniformID: "QQ:10001", Version: StoryVersionV1}
	url, reason := postLogToBackend(env, server.URL, []byte("payload"), "SealDice")
	if url != "" {
		t.Fatalf("url = %q, want empty", url)
	}
	if !strings.Contains(reason, "网页") {
		t.Fatalf("reason = %q, want a hint about HTML instead of JSON", reason)
	}
}

// TestUploadToSealBackendsRetriesWithFullBody 多个后端地址时，先前是用同一个 io.Reader
// 依次上传，第一个后端把数据读完之后，后面几个后端只会收到空 body。
func TestUploadToSealBackendsRetriesWithFullBody(t *testing.T) {
	payload := strings.Repeat("sealdice-log-payload ", 64)

	var (
		mu      sync.Mutex
		bodies  []string
		counter int
	)
	handler := func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll: %v", err)
			return
		}
		mu.Lock()
		bodies = append(bodies, string(raw))
		counter++
		first := counter == 1
		mu.Unlock()

		if first {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("first backend is down"))
			return
		}
		_, _ = w.Write([]byte(`{"url":"https://logs.example/?key=AbCd#123456"}`))
	}
	server := httptest.NewServer(http.HandlerFunc(handler))
	defer server.Close()

	env := UploadEnv{
		Log:       zap.NewNop().Sugar(),
		LogName:   "log",
		UniformID: "QQ:10001",
		Version:   StoryVersionV1,
		Backends:  []string{server.URL + "/first/log", server.URL + "/second/log"},
	}
	url, reason := uploadToSealBackends(env, []byte(payload))
	if url != "https://logs.example/?key=AbCd#123456" {
		t.Fatalf("url = %q, reason = %q", url, reason)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("got %d attempts, want 2", len(bodies))
	}
	for i, body := range bodies {
		if !strings.Contains(body, payload) {
			t.Fatalf("attempt %d body lost the payload (len=%d)", i+1, len(body))
		}
	}
}

func TestLogUploadFailedErrKeepsOriginalWording(t *testing.T) {
	if got := logUploadFailedErr("").Error(); got != "上传 log 到服务器失败，未能获取染色器链接" {
		t.Fatalf("err = %q", got)
	}
	got := logUploadFailedErr("染色器后端返回 HTTP 413: Size is too big!").Error()
	if !strings.HasPrefix(got, "上传 log 到服务器失败，未能获取染色器链接") || !strings.Contains(got, "HTTP 413") {
		t.Fatalf("err = %q", got)
	}
}
