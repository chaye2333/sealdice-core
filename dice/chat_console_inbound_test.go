package dice //nolint:testpackage

import (
	"strings"
	"testing"

	"github.com/sealdice/botgo/dto"
)

// 这批用例锁定"入站消息按开放平台新文档解析"的行为：
// message_type（0 文本 / 3 卡片 / 101 并行 / 102 聊天记录 / 103 引用）、
// 附件媒体映射、语音 WAV 转码地址与 ASR 转写。

func TestChatConsoleInboundTextArkCard(t *testing.T) {
	payload := &dto.Message{
		MessageType: 3,
		ARKData: &dto.ARKData{
			ArkType: "miniapp",
			ArkName: "小程序",
			Prompt:  "快来完成今日学习打卡",
			Fields: map[string]string{
				"title":    "快来完成今日学习打卡",
				"source":   "学习助手",
				"jump_url": "https://example.com/app",
			},
		},
	}
	text := chatConsoleInboundText("", payload, nil)
	for _, want := range []string{"[卡片消息] 小程序", "摘要: 快来完成今日学习打卡", "来源: 学习助手", "链接: https://example.com/app"} {
		if !strings.Contains(text, want) {
			t.Fatalf("card text = %q, want contains %q", text, want)
		}
	}
	// 标题与 prompt 相同，不应该重复出现两次
	if strings.Count(text, "快来完成今日学习打卡") != 1 {
		t.Fatalf("title/prompt duplicated: %q", text)
	}

	// 官方已经把摘要写进 content 时不要重复追加
	withSummary := chatConsoleInboundText("[卡片消息] 小程序\n摘要: 快来完成今日学习打卡", payload, nil)
	if strings.Count(withSummary, "快来完成今日学习打卡") != 1 {
		t.Fatalf("summary duplicated: %q", withSummary)
	}

	// 没有任何内容时不要只刷一行类型名
	empty := chatConsoleInboundText("原文本", &dto.Message{MessageType: 3, ARKData: &dto.ARKData{ArkName: "小程序"}}, nil)
	if empty != "原文本" {
		t.Fatalf("empty ark changed text: %q", empty)
	}
}

func TestChatConsoleInboundTextChatRecord(t *testing.T) {
	payload := &dto.Message{
		MessageType: 102,
		MsgElements: []*dto.MessageElement{
			{Content: "今天的学习计划已完成"},
			{Content: "很棒！继续保持"},
		},
	}
	text := chatConsoleInboundText("", payload, nil)
	if !strings.Contains(text, "[聊天记录]") || !strings.Contains(text, "今天的学习计划已完成") ||
		!strings.Contains(text, "很棒！继续保持") {
		t.Fatalf("chat record text = %q", text)
	}

	parts := chatConsoleInboundText("", &dto.Message{
		MessageType: 101,
		MsgElements: []*dto.MessageElement{{Content: "并行内容"}},
	}, nil)
	if !strings.Contains(parts, "[合并消息]") {
		t.Fatalf("parts text = %q", parts)
	}
}

func TestChatConsoleInboundQuote(t *testing.T) {
	payload := &dto.Message{
		MessageType: 103,
		MsgElements: []*dto.MessageElement{
			{Content: "每天坚持阅读半小时"},
		},
		MessageScene: dto.MessageScene{Ext: []string{
			"ref_msg_idx=REFIDX_aaaaaaaa",
			"msg_idx=REFIDX_zzzzzzzz",
		}},
	}
	quote := chatConsoleInboundQuote(payload)
	if quote == nil {
		t.Fatal("quote is nil")
	}
	if quote.Text != "每天坚持阅读半小时" {
		t.Fatalf("quote text = %q", quote.Text)
	}
	if quote.MsgID != "REFIDX_aaaaaaaa" {
		t.Fatalf("quote msgId = %q, want ref_msg_idx", quote.MsgID)
	}

	// 非引用消息不应该造出引用
	if chatConsoleInboundQuote(&dto.Message{MessageType: 0}) != nil {
		t.Fatal("plain text should not carry quote")
	}
	// 引用但元素为空 → 不展示
	if chatConsoleInboundQuote(&dto.Message{MessageType: 103}) != nil {
		t.Fatal("empty quote should be dropped")
	}
}

func TestChatConsoleInboundMediaKindsAndVoiceWAV(t *testing.T) {
	payload := &dto.Message{
		Attachments: []*dto.MessageAttachment{
			{URL: "https://multimedia.nt.qq.com.cn/a.jpg", ContentType: "image/jpeg", FileName: "a.jpg"},
			{URL: "https://multimedia.nt.qq.com.cn/b.silk", ContentType: "voice", FileName: "b.silk"},
			{URL: "https://multimedia.nt.qq.com.cn/c.mp4", ContentType: "video/mp4", FileName: "c.mp4"},
			{URL: "https://multimedia.nt.qq.com.cn/d.pdf", ContentType: "file", FileName: "d.pdf"},
		},
	}
	// SDK 没暴露 voice_wav_url / asr_refer_text，这里模拟原始事件体
	event := &dto.WSPayload{RawMessage: []byte(`{"d":{"attachments":[
		{"filename":"b.silk","voice_wav_url":"https://multimedia.nt.qq.com.cn/b.wav","asr_refer_text":"你好呀"}]}}`)}

	media := chatConsoleInboundMedia(payload, event)
	if len(media) != 4 {
		t.Fatalf("media len = %d, want 4", len(media))
	}
	kinds := []string{media[0].Kind, media[1].Kind, media[2].Kind, media[3].Kind}
	want := []string{"image", "record", "video", "file"}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kind[%d] = %q, want %q", i, kinds[i], want[i])
		}
	}
	if media[1].URL != "https://multimedia.nt.qq.com.cn/b.wav" {
		t.Fatalf("voice url = %q, want wav", media[1].URL)
	}
	if media[3].Name != "d.pdf" {
		t.Fatalf("file name = %q", media[3].Name)
	}

	// ASR 结果要补进正文
	text := chatConsoleInboundText("", payload, event)
	if !strings.Contains(text, "[语音转文字] 你好呀") {
		t.Fatalf("asr text missing: %q", text)
	}
}

func TestChatConsoleRecordMediaPrefersExplicit(t *testing.T) {
	explicit := []ChatConsoleMedia{{Kind: "record", URL: "https://x/b.wav"}}
	got := chatConsoleRecordMedia(explicit, "[CQ:image,file=https://x/a.jpg]")
	if len(got) != 1 || got[0].URL != "https://x/b.wav" {
		t.Fatalf("explicit media should win, got %#v", got)
	}

	// 没有显式附件时退回 CQ 码解析，保持老行为
	got = chatConsoleRecordMedia(nil, "[CQ:image,file=https://x/a.jpg]")
	if len(got) != 1 || got[0].Kind != "image" {
		t.Fatalf("cq fallback failed, got %#v", got)
	}
}
