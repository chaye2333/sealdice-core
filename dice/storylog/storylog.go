package storylog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"sealdice-core/model"
	"sealdice-core/utils/dboperator/engine"
)

const cachedLogProbeTimeout = 3 * time.Second

type cachedLogProbeResult uint8

const (
	cachedLogProbeUnknown cachedLogProbeResult = iota
	cachedLogProbeAlive
	cachedLogProbeMissing
)

type UploadEnv struct {
	Dir      string
	Db       engine.DatabaseOperator
	Log      *zap.SugaredLogger
	Backends []string
	Version  StoryVersion

	LogName   string
	UniformID string
	GroupID   string
	Token     string

	// ForceReupload 跳过"缓存链接仍然有效"的判断，直接重新上传。
	// 用于换了染色器前端域名、想刷新链接等场景。
	ForceReupload bool

	lines  []*model.LogOneItem
	data   *[]byte
	Notice string
}

func (env *UploadEnv) appendNotice(notice string) {
	if notice == "" {
		return
	}
	if env.Notice == "" {
		env.Notice = notice
		return
	}
	if strings.Contains(env.Notice, notice) {
		return
	}
	env.Notice += "\n" + notice
}

func Upload(env UploadEnv) (string, string, error) {
	if env.Version == StoryVersionV1 {
		return uploadV1(env)
	}
	if env.Version == StoryVersionV105 {
		return uploadV105(env)
	}
	return "", "", errors.New("未指定日志版本")
}

func checkCachedLogURL(env UploadEnv, rawURL string) cachedLogProbeResult {
	result := probeCachedLogURL(env, rawURL)
	switch result {
	case cachedLogProbeAlive:
		return result
	case cachedLogProbeMissing:
		env.Log.Infof("之前上传的日志链接已失效，将重新上传 Log:%s.%s URL:%s", env.GroupID, env.LogName, rawURL)
	case cachedLogProbeUnknown:
		env.Log.Warnf("无法确认之前上传的日志链接是否有效，将尝试重新上传 Log:%s.%s URL:%s", env.GroupID, env.LogName, rawURL)
	}
	return result
}

func fallbackToCachedLogURL(env *UploadEnv, rawURL string, probeResult cachedLogProbeResult) (string, string, bool) {
	if rawURL == "" || probeResult != cachedLogProbeUnknown {
		return "", env.Notice, false
	}
	const notice = "重新上传日志失败，现返回之前缓存的链接；该链接可能仍然有效。"
	env.appendNotice(notice)
	env.Log.Warnf("%s Log:%s.%s URL:%s", notice, env.GroupID, env.LogName, rawURL)
	return rawURL, env.Notice, true
}

func probeCachedLogURL(env UploadEnv, rawURL string) cachedLogProbeResult {
	logURL, err := url.Parse(rawURL)
	if err != nil {
		return cachedLogProbeMissing
	}
	key, password := logURL.Query().Get("key"), logURL.Fragment
	if key == "" || password == "" {
		return cachedLogProbeMissing
	}

	ctx, cancel := context.WithTimeout(context.Background(), cachedLogProbeTimeout)
	defer cancel()

	missing := false
	for _, backend := range env.Backends {
		backendURL, parseErr := url.Parse(backend)
		if parseErr != nil {
			continue
		}
		path := strings.TrimRight(backendURL.Path, "/")
		if !strings.HasSuffix(path, "/log") {
			continue
		}
		backendURL.Path = strings.TrimSuffix(path, "/log") + "/load_data"
		backendURL.RawPath = ""
		query := backendURL.Query()
		query.Set("key", key)
		query.Set("password", password)
		backendURL.RawQuery = query.Encode()
		backendURL.Fragment = ""

		req, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, backendURL.String(), nil)
		if requestErr != nil {
			continue
		}
		req.Header.Set("Range", "bytes=0-0")
		if env.Token != "" {
			req.Header.Set("Authorization", "Bearer "+env.Token)
		}

		resp, requestErr := http.DefaultClient.Do(req) //nolint:gosec
		if requestErr != nil {
			continue
		}
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 16))
		_ = resp.Body.Close()

		switch {
		case resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices:
			if cachedLogProbeIsHTMLErrorPage(resp.Header.Get("Content-Type"), snippet) {
				// 有些染色器前端是 SPA：任意路径、任意 key 都返回 200/206 + index.html，
				// 连错误的 key 和不存在的接口都一样。这种 200 说明不了链接有效，
				// 按「无法确认」处理，让调用方去尝试重新上传（失败还能回退旧链接）。
				continue
			}
			return cachedLogProbeAlive
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			missing = true
		}
	}
	if missing {
		return cachedLogProbeMissing
	}
	return cachedLogProbeUnknown
}

// cachedLogProbeIsHTMLErrorPage 判断探测响应是不是"前端兜底页"而不是真正的日志数据。
// 只看响应体的开头，够用且不会多读流量。
func cachedLogProbeIsHTMLErrorPage(contentType string, snippet []byte) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml") {
		return true
	}
	for _, b := range snippet {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '<':
			return true
		default:
			return false
		}
	}
	return false
}

// uploadHTTPTimeout 上传日志的 HTTP 超时。
// 以前用 http.Client{} 没有任何超时，染色器后端卡住时海豹会一直挂着不返回。
const uploadHTTPTimeout = 60 * time.Second

// logUploadFailedErr 在保留原有报错文案的前提下，把染色器后端的真实原因带出来，
// 例如「染色器后端返回 HTTP 413: File size exceeds 5MB limit」。
func logUploadFailedErr(reason string) error {
	if reason == "" {
		return errors.New("上传 log 到服务器失败，未能获取染色器链接")
	}
	return fmt.Errorf("上传 log 到服务器失败，未能获取染色器链接：%s", reason)
}

// logBodySnippet 把响应体压成一行并截断，方便塞进日志和报错信息里。
func logBodySnippet(body []byte) string {
	const limit = 200
	text := strings.ReplaceAll(strings.ReplaceAll(string(body), "\r", " "), "\n", " ")
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}

// postLogToBackend 把日志数据以 multipart 形式 PUT 给染色器后端。
// 成功返回染色器链接；失败返回空链接，以及一段可以直接转给用户看的原因。
//
// data 用 []byte 而不是 io.Reader：多个后端轮流重试时，
// 同一个 io.Reader 被第一个后端读完之后，后面几个后端只能收到空 body。
func postLogToBackend(env UploadEnv, backend string, data []byte, clientName string) (string, string) {
	client := &http.Client{Timeout: uploadHTTPTimeout}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	writeField := func(name, value string) {
		field, err := writer.CreateFormField(name)
		if err != nil {
			return
		}
		_, _ = field.Write([]byte(value))
	}
	writeField("name", env.LogName)
	writeField("uniform_id", env.UniformID)
	writeField("client", clientName)
	writeField("version", strconv.Itoa(int(env.Version)))

	part, err := writer.CreateFormFile("file", "log-zlib-compressed")
	if err == nil {
		_, err = part.Write(data)
	}
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		env.Log.Errorf("构造日志上传请求失败: %v", err)
		return "", "构造上传请求失败"
	}

	req, err := http.NewRequest(http.MethodPut, backend, body)
	if err != nil {
		env.Log.Errorf("构造日志上传请求失败: %v", err)
		return "", "构造上传请求失败"
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	if len(env.Token) > 0 {
		req.Header.Set("Authorization", "Bearer "+env.Token)
	}

	resp, err := client.Do(req) //nolint:gosec
	if err != nil {
		env.Log.Errorf("日志上传请求失败: %v", err)
		return "", "连接染色器后端失败（" + err.Error() + "）"
	}
	defer func() { _ = resp.Body.Close() }()

	bodyText, err := io.ReadAll(resp.Body)
	if err != nil {
		env.Log.Errorf("读取日志上传响应失败: %v", err)
		return "", "读取染色器响应失败"
	}

	var ret struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(bodyText, &ret)
	if ret.URL != "" {
		return ret.URL, ""
	}

	snippet := logBodySnippet(bodyText)
	// 带上状态码：以前只打印响应体，413 / 544 / SPA 的 index.html 在日志里长得一模一样，没法判断。
	env.Log.Errorf("日志上传的返回结果异常: HTTP %s body=%s", resp.Status, snippet)

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Sprintf("染色器后端返回 HTTP %d: %s", resp.StatusCode, snippet)
	}
	if strings.HasPrefix(snippet, "<") {
		return "", fmt.Sprintf("染色器后端返回的是网页而不是 JSON（后端地址可能填成了前端地址）: %s", snippet)
	}
	return "", "染色器后端未返回链接: " + snippet
}
