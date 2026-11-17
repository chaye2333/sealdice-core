package dice

// 官方 QQ「素材管理」：把 Markdown 消息要用的图片上传到平台，拿到 qqbot-media:// 引用。
//
// 上传流程（官方文档：服务端接口 > 消息类型 > 素材管理）：
//  1. POST /v2/media-images/prepare  登记任务，拿 presigned_url
//  2. PUT  presigned_url             直传图片二进制（Content-Type 必须与登记一致）
//  3. POST /v2/media-images/complete 用 upload_id + md5 换取 image_id / qqbot-media:// 引用
//
// 平台只返回 image_id / url / 时间，不带图片宽高；而 Markdown 里写
// `![说明 #宽px #高px](qqbot-media://...)` 需要宽高。所以这里在上传时解一次图片头，
// 把宽高记到本地 data 目录的小 JSON 里（按 AppID+image_id 索引），列表接口再合并回来。

import (
	"bytes"
	"context"
	"crypto/md5"  //nolint:gosec // 官方接口要求的文件校验值，非安全用途
	"crypto/sha1" //nolint:gosec // 同上
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/webp"
)

// 官方限制：单张图片不超过 10MB，只支持 png / jpeg / gif / webp。
const officialQQMediaImageMaxBytes = 10 << 20

// OfficialQQMediaImage 是一张已上传的 Markdown 图片素材。
// PreviewURL 是平台给的短时效预览地址，只用于界面展示，不要持久化。
type OfficialQQMediaImage struct {
	ImageID    string `json:"imageId"`
	URL        string `json:"url"` // qqbot-media://image/v1/<token>
	FileName   string `json:"fileName"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	CreatedAt  int64  `json:"createdAt,omitempty"`
	ExpiresAt  int64  `json:"expiresAt,omitempty"`
	PreviewURL string `json:"previewUrl,omitempty"`
}

type OfficialQQMediaImageList struct {
	Images     []OfficialQQMediaImage `json:"images"`
	Total      uint64                 `json:"total"`
	NextCursor string                 `json:"nextCursor,omitempty"`
}

// officialQQMediaImageMeta 是本地补记的宽高（平台接口不返回）。
type officialQQMediaImageMeta struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

var officialQQMediaMetaState = struct {
	sync.Mutex
	files map[string]map[string]officialQQMediaImageMeta
}{files: map[string]map[string]officialQQMediaImageMeta{}}

func (pa *PlatformAdapterOfficialQQ) officialQQMediaMetaPath() string {
	if pa == nil || pa.EndPoint == nil || pa.EndPoint.Session == nil || pa.EndPoint.Session.Parent == nil {
		return ""
	}
	dataDir := strings.TrimSpace(pa.EndPoint.Session.Parent.BaseConfig.DataDir)
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "official-qq-media-images.json")
}

func officialQQMediaMetaKey(appID, imageID string) string {
	return appID + "|" + imageID
}

func officialQQMediaMetaLoadLocked(file string) map[string]officialQQMediaImageMeta {
	if m, ok := officialQQMediaMetaState.files[file]; ok {
		return m
	}
	m := map[string]officialQQMediaImageMeta{}
	if data, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	officialQQMediaMetaState.files[file] = m
	return m
}

func officialQQMediaMetaSaveLocked(file string, m map[string]officialQQMediaImageMeta) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, file)
}

func officialQQMediaMetaGet(file, key string) (officialQQMediaImageMeta, bool) {
	if file == "" || key == "" {
		return officialQQMediaImageMeta{}, false
	}
	officialQQMediaMetaState.Lock()
	defer officialQQMediaMetaState.Unlock()
	m := officialQQMediaMetaLoadLocked(file)
	v, ok := m[key]
	return v, ok
}

func officialQQMediaMetaSet(file, key string, meta officialQQMediaImageMeta) {
	if file == "" || key == "" || meta.Width <= 0 || meta.Height <= 0 {
		return
	}
	officialQQMediaMetaState.Lock()
	defer officialQQMediaMetaState.Unlock()
	m := officialQQMediaMetaLoadLocked(file)
	m[key] = meta
	officialQQMediaMetaSaveLocked(file, m)
}

func officialQQMediaMetaDelete(file, key string) {
	if file == "" || key == "" {
		return
	}
	officialQQMediaMetaState.Lock()
	defer officialQQMediaMetaState.Unlock()
	m := officialQQMediaMetaLoadLocked(file)
	if _, ok := m[key]; !ok {
		return
	}
	delete(m, key)
	officialQQMediaMetaSaveLocked(file, m)
}

// officialQQMediaImageContentType 按扩展名给出 MIME 与实际应匹配的解码格式。
// 官方要求扩展名与 Content-Type 一致，且不校验内容；这里额外用 DecodeConfig
// 验证真实格式，避免出现"complete 成功但发送时被平台拒绝"的图片。
func officialQQMediaImageContentType(fileName string) (contentType, format string, err error) {
	switch strings.ToLower(path.Ext(fileName)) {
	case ".png":
		return "image/png", "png", nil
	case ".jpg", ".jpeg":
		return "image/jpeg", "jpeg", nil
	case ".gif":
		return "image/gif", "gif", nil
	case ".webp":
		return "image/webp", "webp", nil
	}
	return "", "", errors.New("只支持 png / jpeg / gif / webp 图片")
}

func officialQQMediaImageSize(data []byte) (width, height int, format string, err error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, "", errors.New("无法识别图片内容，请确认文件没有损坏")
	}
	return cfg.Width, cfg.Height, format, nil
}

// officialQQMediaImagePut 用预签名 URL 直传图片二进制。
// 该 URL 自带鉴权，不能附加 Authorization / X-Union-Appid，否则 COS 会拒绝。
func (pa *PlatformAdapterOfficialQQ) officialQQMediaImagePut(
	ctx context.Context, presignedURL, contentType string, data []byte,
) (etag string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, presignedURL, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := (&http.Client{Timeout: pa.requestTimeout()}).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("上传图片到临时存储失败 HTTP %d: %s", resp.StatusCode, string(body))
	}
	return strings.Trim(resp.Header.Get("ETag"), "\""), nil
}

// UploadOfficialQQMediaImage 走完 prepare → PUT → complete，返回图片引用与宽高。
func (pa *PlatformAdapterOfficialQQ) UploadOfficialQQMediaImage(fileName string, data []byte) (*OfficialQQMediaImage, error) {
	if pa == nil || pa.tokenSource == nil || strings.TrimSpace(pa.AppID) == "" {
		return nil, errors.New("官方 QQ 机器人尚未连接")
	}
	fileName = path.Base(strings.TrimSpace(fileName))
	if fileName == "" || fileName == "." || fileName == "/" {
		return nil, errors.New("文件名不能为空")
	}
	if len(data) == 0 {
		return nil, errors.New("图片内容为空")
	}
	if len(data) > officialQQMediaImageMaxBytes {
		return nil, errors.New("图片超过 10MB 上限")
	}
	contentType, wantFormat, err := officialQQMediaImageContentType(fileName)
	if err != nil {
		return nil, err
	}
	width, height, format, err := officialQQMediaImageSize(data)
	if err != nil {
		return nil, err
	}
	if format != wantFormat {
		return nil, fmt.Errorf("扩展名与实际图片格式不一致（%s 实际是 %s）", wantFormat, format)
	}

	md5Sum := md5.Sum(data)   //nolint:gosec
	sha1Sum := sha1.Sum(data) //nolint:gosec
	md5Hex := hex.EncodeToString(md5Sum[:])
	sha1Hex := hex.EncodeToString(sha1Sum[:])

	ctx, cancel := context.WithTimeout(context.Background(), pa.requestTimeout())
	defer cancel()

	body, err := pa.officialQQAPIRequest(ctx, http.MethodPost, "/v2/media-images/prepare", map[string]any{
		"file_name":    fileName,
		"file_size":    len(data),
		"content_type": contentType,
		"md5":          md5Hex,
		"sha1":         sha1Hex,
	})
	if err != nil {
		return nil, err
	}
	var prepare struct {
		UploadID     string `json:"upload_id"`
		PresignedURL string `json:"presigned_url"`
	}
	if err = json.Unmarshal(body, &prepare); err != nil {
		return nil, fmt.Errorf("解析上传任务失败: %w", err)
	}
	if prepare.UploadID == "" || prepare.PresignedURL == "" {
		return nil, errors.New("平台没有返回上传任务")
	}

	etag, err := pa.officialQQMediaImagePut(ctx, prepare.PresignedURL, contentType, data)
	if err != nil {
		return nil, err
	}
	// complete 要的是文件 MD5；优先用 COS 回给我们的 ETag。
	completeMD5 := md5Hex
	if len(etag) == 32 {
		completeMD5 = strings.ToLower(etag)
	}

	body, err = pa.officialQQAPIRequest(ctx, http.MethodPost, "/v2/media-images/complete", map[string]any{
		"upload_id": prepare.UploadID,
		"md5":       completeMD5,
	})
	if err != nil {
		return nil, err
	}
	var complete struct {
		ImageID    string `json:"image_id"`
		URL        string `json:"url"`
		TTL        uint32 `json:"ttl"`
		PreviewURL string `json:"preview_url"`
	}
	if err = json.Unmarshal(body, &complete); err != nil {
		return nil, fmt.Errorf("解析上传结果失败: %w", err)
	}
	if complete.ImageID == "" || complete.URL == "" {
		return nil, errors.New("平台没有返回图片引用")
	}
	if !strings.HasPrefix(complete.URL, "qqbot-media://") {
		return nil, errors.New("平台返回的图片引用格式异常")
	}

	now := time.Now().Unix()
	image := &OfficialQQMediaImage{
		ImageID:    complete.ImageID,
		URL:        complete.URL,
		FileName:   fileName,
		Width:      width,
		Height:     height,
		CreatedAt:  now,
		PreviewURL: complete.PreviewURL,
	}
	if complete.TTL > 0 {
		image.ExpiresAt = now + int64(complete.TTL)
	}
	officialQQMediaMetaSet(
		pa.officialQQMediaMetaPath(),
		officialQQMediaMetaKey(pa.AppID, image.ImageID),
		officialQQMediaImageMeta{Width: width, Height: height},
	)
	return image, nil
}

// ListOfficialQQMediaImages 查询当前机器人的有效图片（按上传时间倒序，游标分页）。
func (pa *PlatformAdapterOfficialQQ) ListOfficialQQMediaImages(cursor string) (*OfficialQQMediaImageList, error) {
	if pa == nil || pa.tokenSource == nil || strings.TrimSpace(pa.AppID) == "" {
		return nil, errors.New("官方 QQ 机器人尚未连接")
	}
	ctx, cancel := context.WithTimeout(context.Background(), pa.requestTimeout())
	defer cancel()

	apiPath := "/v2/media-images"
	if strings.TrimSpace(cursor) != "" {
		apiPath += "?cursor=" + url.QueryEscape(strings.TrimSpace(cursor))
	}
	body, err := pa.officialQQAPIRequest(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Images []struct {
			ImageID    string `json:"image_id"`
			URL        string `json:"url"`
			FileName   string `json:"file_name"`
			PreviewURL string `json:"preview_url"`
			CreatedAt  int64  `json:"created_at"`
			ExpiresAt  int64  `json:"expires_at"`
		} `json:"images"`
		NextCursor string `json:"next_cursor"`
		Total      uint64 `json:"total"`
	}
	if err = json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析图片列表失败: %w", err)
	}

	metaFile := pa.officialQQMediaMetaPath()
	list := &OfficialQQMediaImageList{
		Images:     make([]OfficialQQMediaImage, 0, len(payload.Images)),
		Total:      payload.Total,
		NextCursor: payload.NextCursor,
	}
	for _, item := range payload.Images {
		if item.ImageID == "" || item.URL == "" {
			continue
		}
		image := OfficialQQMediaImage{
			ImageID:    item.ImageID,
			URL:        item.URL,
			FileName:   item.FileName,
			CreatedAt:  item.CreatedAt,
			ExpiresAt:  item.ExpiresAt,
			PreviewURL: item.PreviewURL,
		}
		if meta, ok := officialQQMediaMetaGet(metaFile, officialQQMediaMetaKey(pa.AppID, item.ImageID)); ok {
			image.Width = meta.Width
			image.Height = meta.Height
		}
		list.Images = append(list.Images, image)
	}
	return list, nil
}

// DeleteOfficialQQMediaImage 删除图片，释放 100 张配额。
func (pa *PlatformAdapterOfficialQQ) DeleteOfficialQQMediaImage(imageID string) error {
	if pa == nil || pa.tokenSource == nil || strings.TrimSpace(pa.AppID) == "" {
		return errors.New("官方 QQ 机器人尚未连接")
	}
	imageID = strings.TrimSpace(imageID)
	if imageID == "" {
		return errors.New("缺少图片 ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), pa.requestTimeout())
	defer cancel()

	_, err := pa.officialQQAPIRequest(ctx, http.MethodDelete,
		"/v2/media-images/"+url.PathEscape(imageID)+"/delete", nil)
	if err != nil {
		return err
	}
	officialQQMediaMetaDelete(pa.officialQQMediaMetaPath(), officialQQMediaMetaKey(pa.AppID, imageID))
	return nil
}

// TranslateOfficialQQMediaImageError 把官方错误码翻成便于界面展示的中文。
func TranslateOfficialQQMediaImageError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "40035001"):
		return "图片格式不支持：只支持 png / jpeg / gif / webp，且扩展名需与内容一致"
	case strings.Contains(msg, "40035002"):
		return "图片超过 10MB 上限"
	case strings.Contains(msg, "40035003"):
		return "上传任务不存在或已过期（超过 4 小时），请重新上传"
	case strings.Contains(msg, "40035006"):
		return "平台内部错误，请稍后重试"
	case strings.Contains(msg, "40035007"):
		return "该机器人已有 100 张图片，请先删除不用的图片"
	case strings.Contains(msg, "40035008"):
		return "图片不存在或已被删除"
	case strings.Contains(msg, "40035009"):
		return "图片已过期或已被删除，引用链接不再可用"
	case strings.Contains(msg, "40035010"):
		return "引用链接无效，或不属于当前机器人"
	}
	return msg
}
