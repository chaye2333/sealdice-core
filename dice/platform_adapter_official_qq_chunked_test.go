//nolint:testpackage
package dice

import (
	"context"
	"crypto/md5"  //nolint:gosec // 官方接口要求的校验值，非安全用途
	"crypto/sha1" //nolint:gosec // 同上
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/oauth2"

	sealdiceLogger "sealdice-core/logger"
	"sealdice-core/message"
)

// fakeTencentQQAPI 模拟腾讯的预上传 / 分片完成 / 合并三个端点，
// 以及 COS 的分片接收端点，用于端到端验证分片上传流程。
type fakeTencentQQAPI struct {
	t *testing.T

	mu             sync.Mutex
	partData       map[int][]byte
	prepareBodies  []map[string]any
	finishBodies   []map[string]any
	mergeBodies    []map[string]any
	prepareFailure bool
	cosFailure     bool
	apiBase        string
}

func newFakeTencentQQAPI(t *testing.T) *fakeTencentQQAPI {
	t.Helper()
	f := &fakeTencentQQAPI{t: t, partData: map[int][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(server.Close)
	f.apiBase = server.URL
	return f
}

func (f *fakeTencentQQAPI) basePath() string { return f.apiBase }

func (f *fakeTencentQQAPI) handle(w http.ResponseWriter, r *http.Request) {
	f.t.Helper()
	body, _ := io.ReadAll(r.Body)

	switch {
	// 预上传
	case strings.HasSuffix(r.URL.Path, "/upload_prepare"):
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		f.mu.Lock()
		f.prepareBodies = append(f.prepareBodies, payload)
		failed := f.prepareFailure
		f.mu.Unlock()
		if failed {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":850019,"message":"不支持的文件格式"}`))
			return
		}
		// 固定 2 个分片，每片 4 字节，预签名 URL 指回本服务器
		_ = json.NewEncoder(w).Encode(map[string]any{
			"upload_id":  "upload_test_1",
			"block_size": "4",
			"parts": []map[string]any{
				{"index": 0, "presigned_url": f.apiBase + "/cos/part?n=0", "block_size": "4"},
				{"index": 1, "presigned_url": f.apiBase + "/cos/part?n=1", "block_size": "4"},
			},
			"upload_config": map[string]any{"concurrency": 1, "retry_timeout": 300, "retry_delay": 1},
		})

	// 分片完成通知
	case strings.HasSuffix(r.URL.Path, "/upload_part_finish"):
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		f.mu.Lock()
		f.finishBodies = append(f.finishBodies, payload)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))

	// 合并
	case strings.HasSuffix(r.URL.Path, "/files"):
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		f.mu.Lock()
		f.mergeBodies = append(f.mergeBodies, payload)
		f.mu.Unlock()
		// file_info 是 base64 文本
		_ = json.NewEncoder(w).Encode(map[string]any{
			"file_uuid": "uuid_1",
			"file_info": "QUJD",
			"ttl":       300,
		})

	// COS 分片接收（预签名 PUT）
	case strings.HasPrefix(r.URL.Path, "/cos/part"):
		f.mu.Lock()
		failed := f.cosFailure
		f.mu.Unlock()
		if failed {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("signature mismatch"))
			return
		}
		index := 0
		if strings.Contains(r.URL.RawQuery, "n=1") {
			index = 1
		}
		f.mu.Lock()
		f.partData[index] = append([]byte(nil), body...)
		f.mu.Unlock()

	default:
		f.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeTencentQQAPI) snapshot() (prepares, finishes, merges []map[string]any, parts map[int][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts = map[int][]byte{}
	for k, v := range f.partData {
		parts[k] = append([]byte(nil), v...)
	}
	return append([]map[string]any(nil), f.prepareBodies...),
		append([]map[string]any(nil), f.finishBodies...),
		append([]map[string]any(nil), f.mergeBodies...),
		parts
}

// newChunkedTestAdapter 构造一个启用分片上传、指向假服务器的适配器。
func newChunkedTestAdapter(t *testing.T, fake *fakeTencentQQAPI) *PlatformAdapterOfficialQQ {
	t.Helper()

	d := &Dice{Logger: sealdiceLogger.M()}
	d.Config.OfficialQQChunkedUploadEnable = true
	d.Config.OfficialQQRequestTimeoutSec = 30
	ep := &EndPointInfo{}
	ep.Session = &IMSession{Parent: d}

	return &PlatformAdapterOfficialQQ{
		EndPoint:          ep,
		AppID:             "test-app",
		UIN:               "100",
		tokenSource:       oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token", TokenType: "Bearer"}),
		apiDomainOverride: fake.basePath(),
	}
}

func hexOf(h io.Writer, data []byte) string {
	_, _ = h.Write(data)
	type sumer interface{ Sum([]byte) []byte }
	return hex.EncodeToString(h.(sumer).Sum(nil))
}

// ---------- 端到端：群聊分片上传 ----------

func TestOfficialQQChunkedUploadGroupEndToEnd(t *testing.T) {
	fake := newFakeTencentQQAPI(t)
	pa := newChunkedTestAdapter(t, fake)

	// 8 字节 → 按假服务器给的 4 字节分片正好 2 片
	dir := t.TempDir()
	path := filepath.Join(dir, "coc7th空白卡.xlsx")
	content := []byte("ABCDEFGH")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	elem, err := message.FilepathToFileElement(path)
	if err != nil {
		t.Fatalf("FilepathToFileElement: %v", err)
	}

	// 调用链上 SendToGroup 已剥掉前缀，所以这里传的是裸 GroupOpenID
	media, err := pa.uploadGroupMedia(context.Background(), "groupopenid", elem, 4)
	if err != nil {
		t.Fatalf("uploadGroupMedia: %v", err)
	}
	// 合并响应里的 file_info 是 base64 的 "QUJD"，应被解码成 "ABC"
	if got := string(media.FileInfo); got != "ABC" {
		t.Fatalf("file_info 应被 base64 解码，want %q got %q", "ABC", got)
	}

	prepares, finishes, merges, parts := fake.snapshot()

	// ① 预上传：字段与校验值
	if len(prepares) != 1 {
		t.Fatalf("expected 1 upload_prepare, got %d", len(prepares))
	}
	prepare := prepares[0]
	if prepare["file_type"] != float64(4) {
		t.Fatalf("file_type should be 4, got %v", prepare["file_type"])
	}
	if prepare["file_name"] != "coc7th空白卡.xlsx" {
		t.Fatalf("file_name mismatch: %v", prepare["file_name"])
	}
	if prepare["file_size"] != "8" {
		t.Fatalf("file_size should be the string \"8\", got %v", prepare["file_size"])
	}
	if prepare["md5"] != hexOf(md5.New(), content) {
		t.Fatalf("md5 mismatch: %v", prepare["md5"])
	}
	if prepare["sha1"] != hexOf(sha1.New(), content) {
		t.Fatalf("sha1 mismatch: %v", prepare["sha1"])
	}
	// 文件小于 10002432 字节时 md5_10m 就是整个文件的 md5
	if prepare["md5_10m"] != hexOf(md5.New(), content) {
		t.Fatalf("md5_10m mismatch: %v", prepare["md5_10m"])
	}

	// ② 分片内容必须与源文件一一对应
	if len(parts) != 2 {
		t.Fatalf("expected 2 uploaded parts, got %d", len(parts))
	}
	if string(parts[0]) != "ABCD" || string(parts[1]) != "EFGH" {
		t.Fatalf("part data mismatch: %q %q", parts[0], parts[1])
	}

	// ③ 每个分片都要通知完成，且带正确的大小与 md5
	if len(finishes) != 2 {
		t.Fatalf("expected 2 upload_part_finish, got %d", len(finishes))
	}
	for i, finish := range finishes {
		if finish["upload_id"] != "upload_test_1" {
			t.Fatalf("finish[%d] upload_id mismatch: %v", i, finish["upload_id"])
		}
		if finish["part_index"] != float64(i) {
			t.Fatalf("finish[%d] part_index mismatch: %v", i, finish["part_index"])
		}
		if finish["block_size"] != "4" {
			t.Fatalf("finish[%d] block_size should be the string \"4\", got %v", i, finish["block_size"])
		}
		if finish["md5"] == "" || finish["md5"] == nil {
			t.Fatalf("finish[%d] md5 is empty", i)
		}
	}

	// ④ 合并请求必须带 upload_id 与 file_name —— 这是文件名能保留的关键
	if len(merges) != 1 {
		t.Fatalf("expected 1 merge request, got %d", len(merges))
	}
	merge := merges[0]
	if merge["upload_id"] != "upload_test_1" {
		t.Fatalf("merge upload_id mismatch: %v", merge["upload_id"])
	}
	if merge["file_name"] != "coc7th空白卡.xlsx" {
		t.Fatalf("merge file_name mismatch（文件名会丢）: %v", merge["file_name"])
	}
	if merge["file_type"] != float64(4) {
		t.Fatalf("merge file_type should be 4, got %v", merge["file_type"])
	}
	// 分片合并路径不应再传 url
	if url, exists := merge["url"]; exists && url != "" {
		t.Fatalf("merge request should not carry a url, got %v", url)
	}
}

func TestOfficialQQChunkedUploadC2CEndToEnd(t *testing.T) {
	fake := newFakeTencentQQAPI(t)
	pa := newChunkedTestAdapter(t, fake)

	dir := t.TempDir()
	path := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(path, []byte("ABCDEFGH"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	elem, err := message.FilepathToFileElement(path)
	if err != nil {
		t.Fatalf("FilepathToFileElement: %v", err)
	}

	if _, err := pa.uploadC2CMedia(context.Background(), "user-openid", elem, 4); err != nil {
		t.Fatalf("uploadC2CMedia: %v", err)
	}

	_, _, merges, _ := fake.snapshot()
	if len(merges) != 1 {
		t.Fatalf("expected 1 merge request, got %d", len(merges))
	}
	if merges[0]["file_name"] != "report.pdf" {
		t.Fatalf("merge file_name mismatch: %v", merges[0]["file_name"])
	}
}

// ---------- 失败处理 ----------

func TestOfficialQQChunkedUploadPrepareFailure(t *testing.T) {
	fake := newFakeTencentQQAPI(t)
	fake.prepareFailure = true
	pa := newChunkedTestAdapter(t, fake)

	dir := t.TempDir()
	path := filepath.Join(dir, "a.xlsx")
	if err := os.WriteFile(path, []byte("ABCDEFGH"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	elem, err := message.FilepathToFileElement(path)
	if err != nil {
		t.Fatalf("FilepathToFileElement: %v", err)
	}

	_, err = pa.uploadGroupMedia(context.Background(), "g", elem, 4)
	if err == nil {
		t.Fatal("expected an error when upload_prepare fails")
	}
	if !strings.Contains(err.Error(), "预上传失败") {
		t.Fatalf("unexpected error: %v", err)
	}
	// 预上传失败时不应该继续 PUT 分片
	_, _, _, parts := fake.snapshot()
	if len(parts) != 0 {
		t.Fatalf("no part should be uploaded after a failed prepare, got %d", len(parts))
	}
}

func TestOfficialQQChunkedUploadPartFailure(t *testing.T) {
	fake := newFakeTencentQQAPI(t)
	fake.cosFailure = true
	pa := newChunkedTestAdapter(t, fake)

	dir := t.TempDir()
	path := filepath.Join(dir, "a.xlsx")
	if err := os.WriteFile(path, []byte("ABCDEFGH"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	elem, err := message.FilepathToFileElement(path)
	if err != nil {
		t.Fatalf("FilepathToFileElement: %v", err)
	}

	_, err = pa.uploadGroupMedia(context.Background(), "g", elem, 4)
	if err == nil {
		t.Fatal("expected an error when a part PUT fails")
	}
	if !strings.Contains(err.Error(), "上传分片") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------- 开关与路由：不能影响既有能力 ----------

func TestOfficialQQChunkedUploadGating(t *testing.T) {
	d := &Dice{Logger: sealdiceLogger.M()}
	ep := &EndPointInfo{}
	ep.Session = &IMSession{Parent: d}
	pa := &PlatformAdapterOfficialQQ{EndPoint: ep}

	localFile := &message.FileElement{File: "a.xlsx", URL: "file:///app/data/file/a.xlsx"}
	remoteFile := &message.FileElement{URL: "https://example.com/a.png"}
	plainFile := &message.FileElement{File: "a.xlsx"}

	// 默认关闭：任何文件都不走分片
	if pa.officialQQShouldUseChunkedUpload(localFile) {
		t.Fatal("分片上传默认应关闭")
	}

	d.Config.OfficialQQChunkedUploadEnable = true
	if !pa.officialQQShouldUseChunkedUpload(localFile) {
		t.Fatal("开关打开后本地文件应走分片")
	}
	if !pa.officialQQShouldUseChunkedUpload(plainFile) {
		t.Fatal("只有本地路径的文件也应走分片")
	}
	// 远程 URL 仍走 URL 上传（腾讯推荐的整文件方式），不走分片
	if pa.officialQQShouldUseChunkedUpload(remoteFile) {
		t.Fatal("远程 URL 不应走分片上传")
	}
	// nil 不能 panic
	if pa.officialQQShouldUseChunkedUpload(nil) {
		t.Fatal("nil 文件不应走分片上传")
	}
}

func TestOfficialQQChunkedUploadSkipsRemoteURL(t *testing.T) {
	fake := newFakeTencentQQAPI(t)
	pa := newChunkedTestAdapter(t, fake)

	// 远程 URL + 开关打开：应该走原有路径而不是分片。
	// 这里 Api 为 nil，如果错误地走了分片之外的老路径会 panic，
	// 因此用 recover 断言"没有真的发起上传"更稳妥。
	func() {
		defer func() { _ = recover() }()
		_, _ = pa.uploadGroupMedia(context.Background(), "g",
			&message.FileElement{URL: "https://example.com/a.png"}, 1)
	}()

	prepares, _, _, _ := fake.snapshot()
	if len(prepares) != 0 {
		t.Fatalf("远程 URL 不应触发 upload_prepare，got %d", len(prepares))
	}
}

// ---------- 纯函数 ----------

func TestOfficialQQReadLocalFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "带 空格.xlsx")
	content := []byte("hello")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	// 由 CQ 码解析出来的元素：File 是 basename，URL 是 file://
	elem, err := message.FilepathToFileElement(path)
	if err != nil {
		t.Fatalf("FilepathToFileElement: %v", err)
	}
	name, data, err := officialQQReadLocalFile(elem)
	if err != nil {
		t.Fatalf("officialQQReadLocalFile: %v", err)
	}
	if name != "带 空格.xlsx" {
		t.Fatalf("文件名推断错误: %q", name)
	}
	if string(data) != "hello" {
		t.Fatalf("文件内容错误: %q", data)
	}

	// 只有 file:// URL、没有 File 字段时也要能推断出名字。
	// 必须用 url.URL 构造：手写 "file://" + "C:/..." 会把盘符解析成 authority，
	// 海豹内部（localPathToFileURL）也是这么构造的。
	fileURL := (&url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(path)}).String()
	name, _, err = officialQQReadLocalFile(&message.FileElement{URL: fileURL})
	if err != nil {
		t.Fatalf("officialQQReadLocalFile(url only): %v", err)
	}
	if name != "带 空格.xlsx" {
		t.Fatalf("从 file:// URL 推断文件名错误: %q (url=%s)", name, fileURL)
	}

	// 远程 URL 必须被拒绝（避免把整个远程文件拉进内存）
	if _, _, err := officialQQReadLocalFile(&message.FileElement{URL: "https://example.com/a.xlsx"}); err == nil {
		t.Fatal("远程 URL 应被拒绝")
	}
	// nil 不能 panic
	if _, _, err := officialQQReadLocalFile(nil); err == nil {
		t.Fatal("nil 应返回错误")
	}
}

func TestParseOfficialQQBlockSize(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"10485760", 10485760},
		{"4", 4},
		{" 5 ", 5},
		{"", 0},
		{"abc", 0},
		{"-1", 0},
		{"4.5", 0},
	}
	for _, tc := range cases {
		if got := parseOfficialQQBlockSize(tc.in); got != tc.want {
			t.Fatalf("parseOfficialQQBlockSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestOfficialQQFirstBytes(t *testing.T) {
	data := []byte("0123456789")
	if got := string(officialQQFirstBytes(data, 4)); got != "0123" {
		t.Fatalf("expected 0123, got %q", got)
	}
	// 不足 n 时返回全部
	if got := string(officialQQFirstBytes(data, 100)); got != "0123456789" {
		t.Fatalf("expected the whole slice, got %q", got)
	}
	// n <= 0 时返回全部（不 panic）
	if got := string(officialQQFirstBytes(data, 0)); got != "0123456789" {
		t.Fatalf("expected the whole slice for n=0, got %q", got)
	}
}

func TestOfficialQQChunkedUploadConfigDefaultsOff(t *testing.T) {
	if DefaultConfig.OfficialQQChunkedUploadEnable {
		t.Fatal("分片上传默认必须是关闭的，避免影响既有的语音/图片上传")
	}
}
