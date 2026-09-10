//nolint:testpackage
package dice

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sealdice/botgo/dto"
	"github.com/sealdice/botgo/openapi"
	"github.com/sealdice/botgo/openapi/options"

	"sealdice-core/message"
)

// officialQQStubAPI 只覆写测试关心的那几个方法，其余交给嵌入的接口。
// 这样不需要实现 openapi.OpenAPI 的全部方法，也不会因为 SDK 接口变动而大面积失败。
type officialQQStubAPI struct {
	openapi.OpenAPI

	groupFileTypes []int
	groupFileErr   error
	lastFileInfo   string

	c2cFileTypes []int
	c2cFileErr   error
}

func (s *officialQQStubAPI) PostGroupFile(_ context.Context, _ string, msg *dto.MessageMediaToCreate) (*dto.Media, error) {
	s.groupFileTypes = append(s.groupFileTypes, msg.FileType)
	if s.groupFileErr != nil {
		return nil, s.groupFileErr
	}
	return &dto.Media{FileInfo: s.lastFileInfo}, nil
}

func (s *officialQQStubAPI) PostC2CMessage(_ context.Context, _ string, msg dto.APIMessage, _ ...options.Option) (*dto.Message, error) {
	// C2C 富媒体走的是 dto.APIMessage 接口，这里取回具体类型读 file_type
	if rich, ok := msg.(*C2CRichMediaMessage); ok {
		s.c2cFileTypes = append(s.c2cFileTypes, rich.FileType)
	}
	if s.c2cFileErr != nil {
		return nil, s.c2cFileErr
	}
	// dto.Message.FileInfo 是 []byte，适配器直接透传给 dto.MediaInfo
	return &dto.Message{FileInfo: []byte(s.lastFileInfo)}, nil
}

// newOfficialQQMediaTestAdapter 构造带最小依赖的适配器：
// prepareMediaMessage 会读取 EndPoint.Session.Parent.Config，所以这条链必须存在。
// 用公网 URL 时走「URL 上传」分支，不需要读本地文件。
func newOfficialQQMediaTestAdapter(t *testing.T, stub *officialQQStubAPI) *PlatformAdapterOfficialQQ {
	t.Helper()

	d := &Dice{}
	ep := &EndPointInfo{}
	ep.Session = &IMSession{Parent: d}

	return &PlatformAdapterOfficialQQ{
		Api:      stub,
		EndPoint: ep,
	}
}

// ---------- ① 请求超时可配置且默认放宽 ----------

func TestOfficialQQRequestTimeoutDefaultAndConfig(t *testing.T) {
	if got := DefaultConfig.OfficialQQRequestTimeoutSec; got != 60 {
		t.Fatalf("expected the default request timeout to be 60s, got %d", got)
	}

	// 旧配置缺该字段（0）时必须补成默认值，否则 SetTimeout(0) 会让所有请求立刻超时
	cfg := Config{}
	cfg.FixOfficialQQConfig()
	if cfg.OfficialQQRequestTimeoutSec != DefaultConfig.OfficialQQRequestTimeoutSec {
		t.Fatalf("expected missing timeout to default to %d, got %d",
			DefaultConfig.OfficialQQRequestTimeoutSec, cfg.OfficialQQRequestTimeoutSec)
	}

	// 低于官方建议值的会被抬到 5 秒
	cfg = Config{}
	cfg.OfficialQQRequestTimeoutSec = 1
	cfg.FixOfficialQQConfig()
	if cfg.OfficialQQRequestTimeoutSec != 5 {
		t.Fatalf("expected a tiny timeout to be raised to 5s, got %d", cfg.OfficialQQRequestTimeoutSec)
	}

	// 过大值会被收敛，避免误填把机器人卡死
	cfg = Config{}
	cfg.OfficialQQRequestTimeoutSec = 999999
	cfg.FixOfficialQQConfig()
	if cfg.OfficialQQRequestTimeoutSec != 600 {
		t.Fatalf("expected an oversized timeout to be clamped to 600s, got %d", cfg.OfficialQQRequestTimeoutSec)
	}
}

func TestOfficialQQAdapterRequestTimeoutUsesConfig(t *testing.T) {
	pa := &PlatformAdapterOfficialQQ{}
	// 没有会话时回退到默认值，而不是 0（0 会让 SDK 请求立刻超时）
	if got := pa.requestTimeout(); got != 60*time.Second {
		t.Fatalf("expected the fallback timeout to be 60s, got %v", got)
	}

	d := &Dice{}
	d.Config.OfficialQQRequestTimeoutSec = 120
	ep := &EndPointInfo{}
	ep.Session = &IMSession{Parent: d}
	pa.EndPoint = ep
	if got := pa.requestTimeout(); got != 120*time.Second {
		t.Fatalf("expected the configured timeout 120s, got %v", got)
	}

	// 配置为 0（老配置）时也必须回退到默认值
	d.Config.OfficialQQRequestTimeoutSec = 0
	if got := pa.requestTimeout(); got != 60*time.Second {
		t.Fatalf("expected a zero config to fall back to 60s, got %v", got)
	}
}

// ---------- ② file_info 必须解码后再传下去 ----------

// TestOfficialQQUploadGroupMediaDecodesFileInfo 锁定一个容易误改的行为。
//
// 上传接口返回的 file_info 是 base64 文本，而 dto.MediaInfo.FileInfo 是 []byte；
// 上游实现先 DecodeString 再交给后续发送接口，二者叠加正好是接口期望的形态。
// 曾经按「官方文档说原样透传」把它去掉，结果发送时报
// 40034032「请求参数file_info无效」，图片和语音全部发不出去。
func TestOfficialQQUploadGroupMediaDecodesFileInfo(t *testing.T) {
	raw := []byte{0x00, 0x01, 0xFF, 0x10, 0x7F, 0x80, 0xAB, 0xCD}
	encoded := base64.StdEncoding.EncodeToString(raw)

	stub := &officialQQStubAPI{lastFileInfo: encoded}
	pa := newOfficialQQMediaTestAdapter(t, stub)

	file := &message.FileElement{URL: "https://example.com/a.mp3"}
	media, err := pa.uploadGroupMedia(context.Background(), "group-1", file, 3)
	if err != nil {
		t.Fatalf("uploadGroupMedia: %v", err)
	}
	if !bytes.Equal(media.FileInfo, raw) {
		t.Fatalf("file_info should be base64-decoded before sending: want %v, got %v", raw, media.FileInfo)
	}
	// 同时确认传下去的 file_type 是语音类型 3
	if len(stub.groupFileTypes) != 1 || stub.groupFileTypes[0] != 3 {
		t.Fatalf("expected file_type=3 to be uploaded, got %v", stub.groupFileTypes)
	}
}

// TestOfficialQQUploadGroupMediaFallsBackWhenFileInfoNotBase64
// 解不开时回退为原文，保证不会因为一次异常响应把内容丢掉。
func TestOfficialQQUploadGroupMediaFallsBackWhenFileInfoNotBase64(t *testing.T) {
	const notBase64 = "not-base64-@@@"
	stub := &officialQQStubAPI{lastFileInfo: notBase64}
	pa := newOfficialQQMediaTestAdapter(t, stub)

	file := &message.FileElement{URL: "https://example.com/a.mp3"}
	media, err := pa.uploadGroupMedia(context.Background(), "group-1", file, 3)
	if err != nil {
		t.Fatalf("uploadGroupMedia: %v", err)
	}
	if string(media.FileInfo) != notBase64 {
		t.Fatalf("expected a fallback to the raw value, got %q", media.FileInfo)
	}
}

func TestOfficialQQUploadC2CMediaDecodesFileInfo(t *testing.T) {
	raw := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	encoded := base64.StdEncoding.EncodeToString(raw)

	stub := &officialQQStubAPI{lastFileInfo: encoded}
	pa := newOfficialQQMediaTestAdapter(t, stub)

	file := &message.FileElement{URL: "https://example.com/a.txt"}
	media, err := pa.uploadC2CMedia(context.Background(), "user-1", file, 4)
	if err != nil {
		t.Fatalf("uploadC2CMedia: %v", err)
	}
	if !bytes.Equal(media.FileInfo, raw) {
		t.Fatalf("file_info should be base64-decoded before sending: want %v, got %v", raw, media.FileInfo)
	}
	if len(stub.c2cFileTypes) != 1 || stub.c2cFileTypes[0] != 4 {
		t.Fatalf("expected file_type=4 to be uploaded, got %v", stub.c2cFileTypes)
	}
}

// ---------- ②-3 文件 CQ 码的构造与转义 ----------

func TestOfficialQQFileCQCodeEscapesPath(t *testing.T) {
	if got := officialQQFileCQCode("/tmp/a.txt"); got != "[CQ:file,file=/tmp/a.txt]" {
		t.Fatalf("unexpected cq code %q", got)
	}

	// 含逗号的路径必须转义，否则 CQ 参数会被截断成一个不存在的路径
	got := officialQQFileCQCode("/tmp/a,b.txt")
	if strings.Contains(got, "a,b") {
		t.Fatalf("comma in path must be escaped, got %q", got)
	}
	if got != "[CQ:file,file=/tmp/a&#44;b.txt]" {
		t.Fatalf("unexpected escaped cq code %q", got)
	}

	// 含方括号的路径同理
	got = officialQQFileCQCode("/tmp/a[1].txt")
	if strings.Contains(got, "[1]") {
		t.Fatalf("brackets in path must be escaped, got %q", got)
	}
}

func TestOfficialQQFileCQCodeRoundTripsThroughParser(t *testing.T) {
	dir := t.TempDir()
	// 文件名故意带逗号，验证转义 → 解析 → 还原是闭环的
	path := filepath.Join(dir, "a,b c.txt")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	elems := message.ConvertStringMessage(officialQQFileCQCode(path))
	if len(elems) != 1 {
		t.Fatalf("expected 1 element, got %d", len(elems))
	}
	fe, ok := elems[0].(*message.FileElement)
	if !ok {
		t.Fatalf("expected a FileElement, got %T", elems[0])
	}
	// FilepathToFileElement 会把路径规范化成 file URL，这里确认文件名被完整保留
	if !strings.Contains(fe.File, "a,b c.txt") {
		t.Fatalf("the parsed file reference lost part of the path: %q", fe.File)
	}
}

func TestOfficialQQSendFileNoLongerReportsUnsupported(t *testing.T) {
	// 直接检查实现：SendFileToPerson / SendFileToGroup 必须构造文件 CQ 码，
	// 而不是再回「但不支持」的提示。
	src, err := os.ReadFile("platform_adapter_official_qq.go")
	if err != nil {
		t.Fatalf("read adapter source: %v", err)
	}
	body := string(src)
	for _, fn := range []string{
		"func (pa *PlatformAdapterOfficialQQ) SendFileToPerson(",
		"func (pa *PlatformAdapterOfficialQQ) SendFileToGroup(",
	} {
		idx := strings.Index(body, fn)
		if idx < 0 {
			t.Fatalf("%s not found", fn)
		}
		rest := body[idx:]
		if end := strings.Index(rest, "\n}\n"); end > 0 {
			rest = rest[:end]
		}
		if strings.Contains(rest, "但不支持") {
			t.Fatalf("%s still reports file sending as unsupported:\n%s", fn, rest)
		}
		if !strings.Contains(rest, "officialQQFileCQCode") {
			t.Fatalf("%s should build a file CQ code:\n%s", fn, rest)
		}
	}
}

// ---------- ②-1 / ②-2 文件元素分支存在，频道不受影响 ----------

func TestOfficialQQGroupAndC2CHandleFileElement(t *testing.T) {
	src, err := os.ReadFile("platform_adapter_official_qq.go")
	if err != nil {
		t.Fatalf("read adapter source: %v", err)
	}
	body := string(src)

	for name, marker := range map[string]string{
		"群聊": "case *message.FileElement:\n\t\t\t// file_type=4",
		"单聊": "case *message.FileElement:\n\t\t\t// file_type=4",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("%s 的 FileElement 分支缺失", name)
		}
	}
	// 群聊与单聊各应有一处
	if got := strings.Count(body, "case *message.FileElement:"); got != 2 {
		t.Fatalf("expected exactly 2 FileElement branches (group + c2c), got %d", got)
	}
	if got := strings.Count(body, "uploadGroupMedia(qctx, groupID, elem, 4)"); got != 1 {
		t.Fatalf("expected the group file branch to pass file_type=4, got %d", got)
	}
	if got := strings.Count(body, "uploadC2CMedia(qctx, userOpenID, e, 4)"); got != 1 {
		t.Fatalf("expected the c2c file branch to pass file_type=4, got %d", got)
	}
}

func TestOfficialQQChannelStillIgnoresFileElement(t *testing.T) {
	src, err := os.ReadFile("platform_adapter_official_qq.go")
	if err != nil {
		t.Fatalf("read adapter source: %v", err)
	}
	body := string(src)

	idx := strings.Index(body, "func (pa *PlatformAdapterOfficialQQ) sendQQChannelMsgRaw(")
	if idx < 0 {
		t.Fatal("sendQQChannelMsgRaw not found")
	}
	rest := body[idx:]
	if end := strings.Index(rest, "\nfunc "); end > 0 {
		rest = rest[:end]
	}
	// 频道场景官方不支持文件，本补丁不动它
	if strings.Contains(rest, "FileElement") {
		t.Fatal("频道发送不应处理 FileElement（平台不支持）")
	}
}
