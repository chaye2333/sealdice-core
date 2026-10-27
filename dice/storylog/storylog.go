package storylog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func uploadToBackend(env UploadEnv, backend string, data io.Reader) string {
	client := &http.Client{}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	field, err := writer.CreateFormField("name")
	if err == nil {
		_, _ = field.Write([]byte(env.LogName))
	}

	field, err = writer.CreateFormField("uniform_id")
	if err == nil {
		_, _ = field.Write([]byte(env.UniformID))
	}

	field, err = writer.CreateFormField("client")
	if err == nil {
		_, _ = field.Write([]byte("SealDice"))
	}

	field, err = writer.CreateFormField("version")
	if err == nil {
		_, _ = field.Write([]byte(strconv.Itoa(int(env.Version))))
	}

	part, _ := writer.CreateFormFile("file", "log-zlib-compressed")
	_, _ = io.Copy(part, data)
	_ = writer.Close()

	req, err := http.NewRequest(http.MethodPut, backend, body)
	if err != nil {
		env.Log.Errorf(err.Error())
		return ""
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	if len(env.Token) > 0 {
		req.Header.Set("Authorization", "Bearer "+env.Token)
	}

	resp, err := client.Do(req) //nolint:gosec
	if err != nil {
		env.Log.Errorf(err.Error())
		return ""
	}
	defer func() { _ = resp.Body.Close() }()

	bodyText, err := io.ReadAll(resp.Body)
	if err != nil {
		env.Log.Errorf(err.Error())
		return ""
	}

	var ret struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(bodyText, &ret)
	if ret.URL == "" {
		env.Log.Error("日志上传的返回结果异常:", string(bodyText))
	}
	return ret.URL
}
