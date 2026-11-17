//nolint:testpackage
package dice

import (
	"bytes"
	"crypto/md5"  //nolint:gosec // 与官方接口一致，仅做文件校验
	"crypto/sha1" //nolint:gosec // 同上
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/oauth2"
)

func newMediaTestAdapter(t *testing.T, apiBase string) (*PlatformAdapterOfficialQQ, *Dice) {
	t.Helper()
	d := &Dice{BaseConfig: BaseConfig{DataDir: t.TempDir()}}
	ep := &EndPointInfo{EndPointInfoBase: EndPointInfoBase{ID: "ep-media", Nickname: "素材机器人"}}
	ep.Session = &IMSession{Parent: d}
	pa := &PlatformAdapterOfficialQQ{
		EndPoint:          ep,
		AppID:             "app-media",
		tokenSource:       oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "media-token", TokenType: "Bearer"}),
		apiDomainOverride: apiBase,
	}
	return pa, d
}

func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

func TestOfficialQQMediaImageSize(t *testing.T) {
	pngData := testPNG(t, 800, 400)
	w, h, format, err := officialQQMediaImageSize(pngData)
	if err != nil || w != 800 || h != 400 || format != "png" {
		t.Fatalf("png size: w=%d h=%d format=%q err=%v", w, h, format, err)
	}

	var jbuf bytes.Buffer
	if jerr := jpeg.Encode(&jbuf, image.NewRGBA(image.Rect(0, 0, 64, 32)), nil); jerr != nil {
		t.Fatalf("jpeg encode: %v", jerr)
	}
	if _, _, format, err = officialQQMediaImageSize(jbuf.Bytes()); err != nil || format != "jpeg" {
		t.Fatalf("jpeg format: %q err=%v", format, err)
	}

	var gbuf bytes.Buffer
	if gerr := gif.Encode(&gbuf, image.NewRGBA(image.Rect(0, 0, 16, 8)), nil); gerr != nil {
		t.Fatalf("gif encode: %v", gerr)
	}
	if _, _, format, err = officialQQMediaImageSize(gbuf.Bytes()); err != nil || format != "gif" {
		t.Fatalf("gif format: %q err=%v", format, err)
	}

	if _, _, _, err = officialQQMediaImageSize([]byte("not an image")); err == nil {
		t.Fatal("非图片内容应报错")
	}
}

func TestOfficialQQMediaImageContentType(t *testing.T) {
	cases := []struct{ name, wantType, wantFormat string }{
		{"a.png", "image/png", "png"},
		{"a.JPG", "image/jpeg", "jpeg"},
		{"a.jpeg", "image/jpeg", "jpeg"},
		{"a.gif", "image/gif", "gif"},
		{"a.webp", "image/webp", "webp"},
	}
	for _, c := range cases {
		gotType, gotFormat, err := officialQQMediaImageContentType(c.name)
		if err != nil || gotType != c.wantType || gotFormat != c.wantFormat {
			t.Fatalf("%s -> %q %q %v", c.name, gotType, gotFormat, err)
		}
	}
	if _, _, err := officialQQMediaImageContentType("a.bmp"); err == nil {
		t.Fatal("bmp 应被拒绝")
	}
}

type mediaFakeAPI struct {
	t *testing.T

	mu           sync.Mutex
	prepareBody  map[string]any
	completeBody map[string]any
	putType      string
	putBytes     []byte
	listCursor   string
	deleted      []string
}

func newMediaFakeAPI(t *testing.T) (*mediaFakeAPI, string) {
	t.Helper()
	fake := &mediaFakeAPI{t: t}
	server := httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(server.Close)
	return fake, server.URL
}

func (f *mediaFakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	switch {
	case strings.HasSuffix(r.URL.Path, "/v2/media-images/prepare"):
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		f.mu.Lock()
		f.prepareBody = payload
		f.mu.Unlock()
		base := "http://" + r.Host
		_ = json.NewEncoder(w).Encode(map[string]any{
			"upload_id":     "upload_1",
			"presigned_url": base + "/cos/put",
			"expires_in":    3600,
		})
	case r.URL.Path == "/cos/put":
		f.mu.Lock()
		f.putType = r.Header.Get("Content-Type")
		f.putBytes = append([]byte(nil), body...)
		f.mu.Unlock()
		sum := md5.Sum(body) //nolint:gosec
		w.Header().Set("ETag", "\""+hex.EncodeToString(sum[:])+"\"")
		_, _ = w.Write([]byte("ok"))
	case strings.HasSuffix(r.URL.Path, "/v2/media-images/complete"):
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		f.mu.Lock()
		f.completeBody = payload
		f.mu.Unlock()
		// 平台实际会把数字写成字符串，这里按真实形状返回
		_ = json.NewEncoder(w).Encode(map[string]any{
			"image_id":    "img_1",
			"url":         "qqbot-media://image/v1/token123",
			"ttl":         "7776000",
			"preview_url": "https://example.myqcloud.com/preview.png",
		})
	case r.URL.Path == "/v2/media-images" && r.Method == http.MethodGet:
		f.mu.Lock()
		f.listCursor = r.URL.Query().Get("cursor")
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"images": []map[string]any{
				{
					"image_id":    "img_1",
					"url":         "qqbot-media://image/v1/token123",
					"file_name":   "kline.png",
					"preview_url": "https://example.myqcloud.com/preview.png",
					"created_at":  "1792051409",
					"expires_at":  "1792656210",
				},
			},
			"next_cursor": "cursor-2",
			"total":       "1",
		})
	case strings.HasSuffix(r.URL.Path, "/delete"):
		f.mu.Lock()
		f.deleted = append(f.deleted, r.URL.Path)
		f.mu.Unlock()
		_, _ = w.Write([]byte("{}"))
	default:
		f.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestOfficialQQMediaImageUploadFlow(t *testing.T) {
	fake, base := newMediaFakeAPI(t)
	pa, d := newMediaTestAdapter(t, base)

	data := testPNG(t, 800, 400)
	image, err := pa.UploadOfficialQQMediaImage("kline.png", data)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if image.ImageID != "img_1" || image.URL != "qqbot-media://image/v1/token123" {
		t.Fatalf("unexpected image: %+v", image)
	}
	if image.Width != 800 || image.Height != 400 || image.FileName != "kline.png" {
		t.Fatalf("width/height/fileName wrong: %+v", image)
	}
	if image.PreviewURL == "" || image.ExpiresAt <= image.CreatedAt {
		t.Fatalf("preview/expires not set: %+v", image)
	}

	sum := md5.Sum(data)  //nolint:gosec
	sha := sha1.Sum(data) //nolint:gosec
	fake.mu.Lock()
	prepare := fake.prepareBody
	complete := fake.completeBody
	putType := fake.putType
	putBytes := append([]byte(nil), fake.putBytes...)
	fake.mu.Unlock()
	if prepare["file_name"] != "kline.png" || prepare["content_type"] != "image/png" {
		t.Fatalf("prepare body: %+v", prepare)
	}
	if prepare["md5"] != hex.EncodeToString(sum[:]) || prepare["sha1"] != hex.EncodeToString(sha[:]) {
		t.Fatalf("prepare hashes: %+v", prepare)
	}
	if prepare["file_size"] != float64(len(data)) {
		t.Fatalf("prepare size: %+v", prepare["file_size"])
	}
	if complete["upload_id"] != "upload_1" || complete["md5"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("complete body: %+v", complete)
	}
	if putType != "image/png" || !bytes.Equal(putBytes, data) {
		t.Fatalf("PUT wrong: type=%q len=%d", putType, len(putBytes))
	}

	// 宽高要落到本地文件，列表接口才能补回来
	raw, err := os.ReadFile(filepath.Join(d.BaseConfig.DataDir, "official-qq-media-images.json"))
	if err != nil || !strings.Contains(string(raw), "img_1") || !strings.Contains(string(raw), "800") {
		t.Fatalf("meta not persisted: %v %q", err, string(raw))
	}
}

func TestOfficialQQMediaImageListAndDelete(t *testing.T) {
	fake, base := newMediaFakeAPI(t)
	pa, d := newMediaTestAdapter(t, base)

	officialQQMediaMetaSet(
		filepath.Join(d.BaseConfig.DataDir, "official-qq-media-images.json"),
		officialQQMediaMetaKey(pa.AppID, "img_1"),
		officialQQMediaImageMeta{Width: 800, Height: 400},
	)

	list, err := pa.ListOfficialQQMediaImages("cursor-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Images) != 1 || list.Total != 1 || list.NextCursor != "cursor-2" {
		t.Fatalf("unexpected list: %+v", list)
	}
	if list.Images[0].Width != 800 || list.Images[0].Height != 400 || list.Images[0].FileName != "kline.png" {
		t.Fatalf("meta not merged: %+v", list.Images[0])
	}
	// 字符串数字也要能解析出来（平台实际返回 "1" / "1792051409" 这种）
	if list.Images[0].CreatedAt != 1792051409 || list.Images[0].ExpiresAt != 1792656210 {
		t.Fatalf("timestamps not parsed: %+v", list.Images[0])
	}
	fake.mu.Lock()
	cursor := fake.listCursor
	fake.mu.Unlock()
	if cursor != "cursor-1" {
		t.Fatalf("cursor not forwarded: %q", cursor)
	}

	if err := pa.DeleteOfficialQQMediaImage("img_1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	fake.mu.Lock()
	deleted := append([]string(nil), fake.deleted...)
	fake.mu.Unlock()
	if len(deleted) != 1 || !strings.HasSuffix(deleted[0], "/v2/media-images/img_1/delete") {
		t.Fatalf("delete path: %v", deleted)
	}
	if _, ok := officialQQMediaMetaGet(
		filepath.Join(d.BaseConfig.DataDir, "official-qq-media-images.json"),
		officialQQMediaMetaKey(pa.AppID, "img_1"),
	); ok {
		t.Fatal("delete 后本地宽高记录应被移除")
	}
}

func TestOfficialQQMediaImageRejectsBadInput(t *testing.T) {
	_, base := newMediaFakeAPI(t)
	pa, _ := newMediaTestAdapter(t, base)

	if _, err := pa.UploadOfficialQQMediaImage("a.bmp", []byte("x")); err == nil {
		t.Fatal("bmp 应被拒绝")
	}
	if _, err := pa.UploadOfficialQQMediaImage("a.png", []byte("not png")); err == nil {
		t.Fatal("坏图片应被拒绝")
	}
	// 扩展名是 .jpg 但内容是 png：必须拦下，否则平台 complete 成功、发送时失败
	if _, err := pa.UploadOfficialQQMediaImage("a.jpg", testPNG(t, 4, 4)); err == nil {
		t.Fatal("扩展名与内容不一致应被拒绝")
	}
	if _, err := pa.UploadOfficialQQMediaImage("a.png", make([]byte, officialQQMediaImageMaxBytes+1)); err == nil {
		t.Fatal("超过 10MB 应被拒绝")
	}
	if _, err := (&PlatformAdapterOfficialQQ{}).UploadOfficialQQMediaImage("a.png", testPNG(t, 4, 4)); err == nil {
		t.Fatal("未连接的适配器应报错")
	}
}

func TestTranslateOfficialQQMediaImageError(t *testing.T) {
	if got := TranslateOfficialQQMediaImageError(errTest("HTTP 400: {\"code\":40035007}")); !strings.Contains(got, "100 张") {
		t.Fatalf("40035007 translation: %q", got)
	}
	if got := TranslateOfficialQQMediaImageError(errTest("HTTP 400: {\"code\":40035008}")); !strings.Contains(got, "不存在") {
		t.Fatalf("40035008 translation: %q", got)
	}
	if got := TranslateOfficialQQMediaImageError(nil); got != "" {
		t.Fatalf("nil err -> %q", got)
	}
}
