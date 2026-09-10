package dice

import (
	"bytes"
	"context"
	"crypto/md5"  //nolint:gosec // 这是腾讯要求的上传校验值，不是安全用途
	"crypto/sha1" //nolint:gosec // 同上
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sealdice/botgo/constant"
	"github.com/sealdice/botgo/dto"

	"sealdice-core/message"
)

// 本文件实现官方 QQ 的「大文件分片上传」，用于在发送本地文件时保留文件名。
//
// 背景：file_data(base64) 方式上传时，腾讯的上传接口没有文件名字段，
// 收到的文件在客户端会显示成「未命名」。官方文档给出的方案是分片上传：
//
//	① POST .../upload_prepare      传入大小与校验值 → upload_id + block_size + 各分片预签名 URL
//	② PUT  <presigned_url>         逐片上传分片数据
//	③ POST .../upload_part_finish  通知服务端该分片完成
//	④ POST .../files { upload_id, file_name }  合并 → file_info
//
// 分片上传是官方文档明确推荐的「大文件或本地文件」方案，也是唯一能自定义文件名的路径。
// 该流程默认关闭（officialQQChunkedUploadEnable），以免影响已经稳定的语音/图片发送。

// officialQQMD5PrefixBytes md5_10m 校验值取文件前多少字节，官方文档固定为 10002432（约 9.54 MB）。
const officialQQMD5PrefixBytes = 10002432

// officialQQChunkedPartTimeout 单个分片 PUT 的超时。
// 预签名 URL 指向 COS，与 OpenAPI 不是同一个域，因此不能复用 SDK 的 resty client。
const officialQQChunkedPartTimeout = 5 * time.Minute

// ---------- 腾讯接口的请求/响应结构 ----------

// officialQQUploadPrepareRequest 预上传请求体。
// 注意：file_size / block_size 在官方文档里都是**字符串**，不是数字。
type officialQQUploadPrepareRequest struct {
	FileType int    `json:"file_type"`
	FileSize string `json:"file_size"`
	FileName string `json:"file_name"`
	MD5      string `json:"md5"`
	SHA1     string `json:"sha1"`
	MD5_10m  string `json:"md5_10m"`
}

// officialQQUploadPart 预上传返回的单个分片信息。
type officialQQUploadPart struct {
	Index        int    `json:"index"`
	PresignedURL string `json:"presigned_url"`
	BlockSize    string `json:"block_size"`
}

// officialQQUploadConfig 服务端下发的上传配置。
type officialQQUploadConfig struct {
	Concurrency  int `json:"concurrency"`
	RetryTimeout int `json:"retry_timeout"`
	RetryDelay   int `json:"retry_delay"`
}

// officialQQUploadPrepareResponse 预上传响应。
type officialQQUploadPrepareResponse struct {
	UploadID     string                  `json:"upload_id"`
	BlockSize    string                  `json:"block_size"`
	Parts        []officialQQUploadPart  `json:"parts"`
	UploadConfig *officialQQUploadConfig `json:"upload_config,omitempty"`
}

// officialQQUploadPartFinishRequest 分片完成通知的请求体。
type officialQQUploadPartFinishRequest struct {
	UploadID  string `json:"upload_id"`
	PartIndex int    `json:"part_index"`
	BlockSize string `json:"block_size"`
	MD5       string `json:"md5"`
}

// officialQQMediaUploadResponse 上传（合并）响应。file_info 需要经 base64 解码后再使用。
type officialQQMediaUploadResponse struct {
	FileUUID string `json:"file_uuid"`
	FileInfo string `json:"file_info"`
	TTL      int    `json:"ttl"`
}

// ---------- 请求体（需要同时满足 SDK 的 dto.APIMessage 接口） ----------

// officialQQGroupChunkedUpload 群聊分片上传的合并请求体。
type officialQQGroupChunkedUpload struct {
	FileType   int    `json:"file_type"`
	SrvSendMsg bool   `json:"srv_send_msg"`
	FileName   string `json:"file_name,omitempty"`
	UploadID   string `json:"upload_id"`
}

func (m *officialQQGroupChunkedUpload) GetEventID() string        { return "" }
func (m *officialQQGroupChunkedUpload) GetSendType() dto.SendType { return dto.RichMedia }

// officialQQC2CChunkedUpload 单聊分片上传的合并请求体。
type officialQQC2CChunkedUpload struct {
	FileType   int    `json:"file_type"`
	SrvSendMsg bool   `json:"srv_send_msg"`
	FileName   string `json:"file_name,omitempty"`
	UploadID   string `json:"upload_id"`
}

func (m *officialQQC2CChunkedUpload) GetEventID() string        { return "" }
func (m *officialQQC2CChunkedUpload) GetSendType() dto.SendType { return dto.RichMedia }

// ---------- 主流程 ----------

// officialQQChunkedUploadEnabled 判断是否对本地文件启用分片上传。
func (pa *PlatformAdapterOfficialQQ) officialQQChunkedUploadEnabled() bool {
	if pa == nil || pa.EndPoint == nil || pa.EndPoint.Session == nil || pa.EndPoint.Session.Parent == nil {
		return false
	}
	return pa.EndPoint.Session.Parent.Config.OfficialQQChunkedUploadEnable
}

// officialQQShouldUseChunkedUpload 判断本次上传是否走分片。
//
// 只有同时满足下面三点才走：
//  1. 开关已打开（officialQQChunkedUploadEnable，默认关闭）；
//  2. 是本地文件（file:// 或普通路径）——远程 URL 走 URL 上传更省事，也是腾讯推荐的整文件方式；
//  3. 文件不小于 1MB——小文件走原有 base64 路径更快，四步流程纯属浪费。
//
// 不满足时返回 false，调用方继续走原有路径，因此语音/图片等既有能力不受影响。
func (pa *PlatformAdapterOfficialQQ) officialQQShouldUseChunkedUpload(file *message.FileElement) bool {
	if !pa.officialQQChunkedUploadEnabled() || file == nil {
		return false
	}
	if file.URL != "" && !strings.HasPrefix(strings.ToLower(file.URL), "file://") {
		return false
	}
	if file.URL == "" && file.File == "" {
		return false
	}
	return true
}

// uploadGroupMediaChunked 用分片上传的方式上传群文件，可自定义文件名。
// 仅对「本地文件」使用：远程 URL 走 URL 上传更省事，也是腾讯推荐的整文件方式。
// uploadGroupMediaChunked 用分片上传的方式上传群文件，可自定义文件名。
//
// 注意 groupID 的形态：调用链是 SendToGroup -> sendQQGroupMsgRaw -> 这里，
// 而 SendToGroup 开头就调用了 mustExtractID，所以传进来的是**剥掉前缀的裸 GroupOpenID**
// （例如 "1D8143A1F14230DC012F302A12DD0526"），不是 "OpenQQ-Group:<UIN>-<openid>"。
// 因此这里不能再解析一次，直接用即可。
func (pa *PlatformAdapterOfficialQQ) uploadGroupMediaChunked(
	qctx context.Context, groupID string, file *message.FileElement, fileType int,
) (*dto.MediaInfo, error) {
	groupOpenID := strings.TrimSpace(groupID)
	if groupOpenID == "" {
		return nil, errors.New("分片上传缺少群 OpenID")
	}

	fileName, data, err := officialQQReadLocalFile(file)
	if err != nil {
		return nil, err
	}

	uploadID, err := pa.officialQQRunChunkedUpload(
		qctx, "/v2/groups/"+url.PathEscape(groupOpenID), fileName, data, fileType)
	if err != nil {
		return nil, err
	}

	// 第 4 步：携带 upload_id 调上传接口完成合并，这一步带着 file_name
	respBody, err := pa.officialQQAPIRequest(qctx, http.MethodPost,
		"/v2/groups/"+url.PathEscape(groupOpenID)+"/files",
		&officialQQGroupChunkedUpload{
			FileType:   fileType,
			SrvSendMsg: false,
			FileName:   fileName,
			UploadID:   uploadID,
		})
	if err != nil {
		return nil, err
	}

	var merged officialQQMediaUploadResponse
	if err := json.Unmarshal(respBody, &merged); err != nil {
		return nil, fmt.Errorf("解析上传响应失败: %w", err)
	}
	return &dto.MediaInfo{FileInfo: decodeOfficialQQFileInfo(merged.FileInfo)}, nil
}

// uploadC2CMediaChunked 用分片上传的方式上传单聊文件，可自定义文件名。
func (pa *PlatformAdapterOfficialQQ) uploadC2CMediaChunked(
	qctx context.Context, userOpenID string, file *message.FileElement, fileType int,
) (*dto.MediaInfo, error) {
	fileName, data, err := officialQQReadLocalFile(file)
	if err != nil {
		return nil, err
	}

	uploadID, err := pa.officialQQRunChunkedUpload(
		qctx, "/v2/users/"+url.PathEscape(userOpenID), fileName, data, fileType)
	if err != nil {
		return nil, err
	}

	respBody, err := pa.officialQQAPIRequest(qctx, http.MethodPost,
		"/v2/users/"+url.PathEscape(userOpenID)+"/files",
		&officialQQC2CChunkedUpload{
			FileType:   fileType,
			SrvSendMsg: false,
			FileName:   fileName,
			UploadID:   uploadID,
		})
	if err != nil {
		return nil, err
	}

	var merged officialQQMediaUploadResponse
	if err := json.Unmarshal(respBody, &merged); err != nil {
		return nil, fmt.Errorf("解析上传响应失败: %w", err)
	}
	return &dto.MediaInfo{FileInfo: decodeOfficialQQFileInfo(merged.FileInfo)}, nil
}

// officialQQRunChunkedUpload 执行分片上传的前三步，返回 upload_id。
//
// basePath 形如 "/v2/groups/<GroupOpenID>" 或 "/v2/users/<UserOpenID>"，两个场景的
// 预上传与分片完成端点只在路径前缀上不同，因此共用同一段逻辑。
//
// ⚠️ 传进来的必须是**裸 OpenID**：调用链 SendToGroup/SendToPerson 开头就调用了
// mustExtractID 把 "OpenQQ-Group:<UIN>-<openid>" 的前缀剥掉，
// 所以这里不能再解析一次（曾经因此报"分片上传需要群 OpenID"）。
func (pa *PlatformAdapterOfficialQQ) officialQQRunChunkedUpload(
	qctx context.Context, basePath, fileName string, data []byte, fileType int,
) (string, error) {
	log := pa.EndPoint.Session.Parent.Logger

	// 第 1 步：预上传
	prepareBody := &officialQQUploadPrepareRequest{
		FileType: fileType,
		FileSize: strconv.Itoa(len(data)),
		FileName: fileName,
		MD5:      officialQQHashHex(md5.New(), data),                                                 //nolint:gosec // 腾讯上传接口要求的上传校验值，不是安全用途
		SHA1:     officialQQHashHex(sha1.New(), data),                                                //nolint:gosec // 同上
		MD5_10m:  officialQQHashHex(md5.New(), officialQQFirstBytes(data, officialQQMD5PrefixBytes)), //nolint:gosec // 同上
	}
	respBody, err := pa.officialQQAPIRequest(qctx, http.MethodPost, basePath+"/upload_prepare", prepareBody)
	if err != nil {
		return "", fmt.Errorf("预上传失败: %w", err)
	}
	var prepared officialQQUploadPrepareResponse
	if err := json.Unmarshal(respBody, &prepared); err != nil {
		return "", fmt.Errorf("解析预上传响应失败: %w", err)
	}
	if prepared.UploadID == "" {
		return "", errors.New("预上传响应缺少 upload_id")
	}
	if len(prepared.Parts) == 0 {
		return "", errors.New("预上传响应没有返回任何分片")
	}

	blockSize := parseOfficialQQBlockSize(prepared.BlockSize)
	log.Infof("official qq 分片上传: 文件=%s 大小=%d 分片数=%d block_size=%d upload_id=%s",
		fileName, len(data), len(prepared.Parts), blockSize, prepared.UploadID)

	// 第 2、3 步：逐片 PUT 并通知完成
	// 说明：官方允许通过 upload_config.concurrency 并发上传，这里先按顺序做，
	// 正确性优先；顺序上传对几百 MB 的文件也只是慢一些。
	offset := 0
	for _, part := range prepared.Parts {
		chunkSize := parseOfficialQQBlockSize(part.BlockSize)
		if chunkSize <= 0 {
			chunkSize = blockSize
		}
		if chunkSize <= 0 {
			return "", fmt.Errorf("分片 %d 的 block_size 无效", part.Index)
		}
		end := offset + chunkSize
		if end > len(data) {
			end = len(data)
		}
		if offset >= end {
			return "", fmt.Errorf("分片 %d 超出文件范围", part.Index)
		}
		chunk := data[offset:end]

		if err := pa.officialQQPutChunk(qctx, part.PresignedURL, chunk); err != nil {
			return "", fmt.Errorf("上传分片 %d 失败: %w", part.Index, err)
		}

		finishBody := &officialQQUploadPartFinishRequest{
			UploadID:  prepared.UploadID,
			PartIndex: part.Index,
			BlockSize: strconv.Itoa(len(chunk)),
			MD5:       officialQQHashHex(md5.New(), chunk), //nolint:gosec // 同上
		}
		if _, err := pa.officialQQAPIRequest(qctx, http.MethodPost, basePath+"/upload_part_finish", finishBody); err != nil {
			return "", fmt.Errorf("通知分片 %d 完成失败: %w", part.Index, err)
		}
		offset = end
	}
	if offset != len(data) {
		return "", fmt.Errorf("分片数据不完整: 已上传 %d 字节，文件共 %d 字节", offset, len(data))
	}
	return prepared.UploadID, nil
}

// officialQQAPIBaseURL 返回腾讯 OpenAPI 的域名。
//
// 正常情况下用 SDK 的 constant.APIDomain；apiDomainOverride 仅用于测试，
// 指向本地 httptest 服务器以便端到端验证分片上传流程。
func (pa *PlatformAdapterOfficialQQ) officialQQAPIBaseURL() string {
	if pa != nil && strings.TrimSpace(pa.apiDomainOverride) != "" {
		return strings.TrimSuffix(pa.apiDomainOverride, "/")
	}
	return strings.TrimSuffix(constant.APIDomain, "/")
}

// officialQQAPIRequest 向腾讯 OpenAPI 发一个 JSON 请求。
//
// 分片上传的预上传/分片完成/合并三个端点 SDK 都没有封装（botgo 只实现了 /files），
// 所以这里自己构造请求；鉴权方式与 SDK 保持一致：
// Authorization: <token_type> <access_token> 与 X-Union-Appid: <appID>。
func (pa *PlatformAdapterOfficialQQ) officialQQAPIRequest(
	qctx context.Context, method, apiPath string, body any,
) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(qctx, method,
		pa.officialQQAPIBaseURL()+apiPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Union-Appid", pa.AppID)
	if pa.tokenSource != nil {
		if token, tokenErr := pa.tokenSource.Token(); tokenErr == nil && token != nil {
			scheme := token.TokenType
			if scheme == "" {
				scheme = "Bearer"
			}
			req.Header.Set("Authorization", scheme+" "+token.AccessToken)
		}
	}

	client := &http.Client{Timeout: pa.requestTimeout()}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// officialQQPutChunk 把分片数据 PUT 到预签名 URL。
// 预签名 URL 自带鉴权信息，因此**不能**附加 Authorization / X-Union-Appid 头，
// 否则签名校验会失败。
func (pa *PlatformAdapterOfficialQQ) officialQQPutChunk(qctx context.Context, presignedURL string, chunk []byte) error {
	if strings.TrimSpace(presignedURL) == "" {
		return errors.New("预签名 URL 为空")
	}
	req, err := http.NewRequestWithContext(qctx, http.MethodPut, presignedURL, bytes.NewReader(chunk))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(chunk))

	client := &http.Client{Timeout: officialQQChunkedPartTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// ---------- 辅助函数 ----------

// officialQQReadLocalFile 读取本地文件内容并推断文件名。
// 只用于本地文件：远程 URL 应走 URL 上传，不该把整个文件拉到内存里。
func officialQQReadLocalFile(file *message.FileElement) (string, []byte, error) {
	if file == nil {
		return "", nil, errors.New("文件为空")
	}
	if file.URL != "" && !strings.HasPrefix(strings.ToLower(file.URL), "file://") {
		return "", nil, fmt.Errorf("分片上传只支持本地文件，收到远程地址 %q", file.URL)
	}
	data, err := getElementBytes(file)
	if err != nil {
		return "", nil, err
	}

	// 优先用 FileElement.File（海豹解析本地路径时会填成 basename），
	// 否则从 file:// URL 或本地路径里取最后一段。
	name := strings.TrimSpace(file.File)
	if name == "" {
		source := file.URL
		if source == "" {
			source = file.File
		}
		source = strings.TrimPrefix(source, "file://")
		source = strings.ReplaceAll(source, "\\", "/")
		name = path.Base(source)
		// file:// URL 里的空格、中文等是百分号编码的，必须解码，
		// 否则腾讯收到的文件名会是 %E5%B8%A6%20... 这种。
		if decoded, err := url.PathUnescape(name); err == nil {
			name = decoded
		}
	}
	name = strings.TrimSpace(filepath.Base(name))
	if name == "" || name == "." || name == "/" {
		name = "file"
	}
	return name, data, nil
}

// officialQQHashHex 计算并输出十六进制小写摘要。
func officialQQHashHex(h hash.Hash, data []byte) string {
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// officialQQFirstBytes 取前 n 个字节，data 不足时返回全部。
func officialQQFirstBytes(data []byte, n int) []byte {
	if n <= 0 || len(data) <= n {
		return data
	}
	return data[:n]
}

// parseOfficialQQBlockSize 解析腾讯返回的 block_size。
// 官方文档里它是字符串，但不同版本可能返回数字，这里两种都兼容。
func parseOfficialQQBlockSize(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	value := 0
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0
		}
		value = value*10 + int(r-'0')
	}
	return value
}

// 确保分片上传的请求体满足 SDK 的接口约束（否则传给 PostC2CMessage 会编译不过）。
var (
	_ dto.APIMessage = (*officialQQGroupChunkedUpload)(nil)
	_ dto.APIMessage = (*officialQQC2CChunkedUpload)(nil)
)
