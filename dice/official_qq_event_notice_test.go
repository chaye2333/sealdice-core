package dice //nolint:testpackage // 直接测试未导出的通知文案构造器

import (
	"testing"

	"github.com/sealdice/botgo/dto"
	"github.com/sealdice/botgo/event"
)

func TestOfficialQQOpenIDNickname(t *testing.T) {
	cases := []struct {
		openID string
		want   string
	}{
		{openID: "A1B2C3D4E5F6", want: "用户E5F6"},
		{openID: "ABCD", want: "用户ABCD"},
		{openID: "AB", want: "用户"},
		{openID: "", want: "用户"},
	}
	for _, c := range cases {
		if got := officialQQOpenIDNickname(c.openID); got != c.want {
			t.Errorf("officialQQOpenIDNickname(%q) = %q, want %q", c.openID, got, c.want)
		}
	}
}

func TestOfficialQQFriendSceneText(t *testing.T) {
	cases := map[int]string{
		1000: "默认",
		1003: "群场景",
		2003: "分享链接（站内）",
		2004: "分享链接（站外）",
		9999: "",
		0:    "",
	}
	for scene, want := range cases {
		if got := officialQQFriendSceneText(scene); got != want {
			t.Errorf("officialQQFriendSceneText(%d) = %q, want %q", scene, got, want)
		}
	}
}

func TestOfficialQQFriendAddNoticeText(t *testing.T) {
	data := &dto.WSC2CFriendData{OpenID: "A1B2C3D4E5F6", Timestamp: 1784570523}
	diceID := formatDiceIDOfficialQQUserOpenID("102000001", data.OpenID)

	// 只有最基础的信息时，不应出现来源/备注/短链
	got := officialQQFriendAddNoticeText(data, officialQQFriendEventRaw{}, diceID)
	want := "收到官方QQ好友添加: <用户E5F6>(OpenQQ:102000001-A1B2C3D4E5F6)"
	if got != want {
		t.Errorf("基础文案 = %q, want %q", got, want)
	}

	// 分享链接场景：场景值、回调数据、短链都要带上
	raw := officialQQFriendEventRaw{Scene: 2003, SceneParam: "callback_abc123", ShortCode: "ab12cd"}
	got = officialQQFriendAddNoticeText(data, raw, diceID)
	want = "收到官方QQ好友添加: <用户E5F6>(OpenQQ:102000001-A1B2C3D4E5F6)，来源:分享链接（站内），备注:\"callback_abc123\"，短链:ab12cd"
	if got != want {
		t.Errorf("分享链接文案 = %q, want %q", got, want)
	}
}

func TestOfficialQQFriendDelNoticeText(t *testing.T) {
	data := &dto.WSC2CFriendData{OpenID: "A1B2C3D4E5F6", Timestamp: 1784570524}
	diceID := formatDiceIDOfficialQQUserOpenID("102000001", data.OpenID)
	want := "官方QQ好友移除: <用户E5F6>(OpenQQ:102000001-A1B2C3D4E5F6)"
	if got := officialQQFriendDelNoticeText(data, diceID); got != want {
		t.Errorf("officialQQFriendDelNoticeText = %q, want %q", got, want)
	}
}

func TestOfficialQQGroupNoticeText(t *testing.T) {
	groupID := formatDiceIDOfficialQQGroupOpenID("102000001", "B2C3D4E5F6")

	joined := officialQQBotJoinedGroupNoticeText("海豹测试群", groupID, "OPOPENID")
	want := "官方QQ机器人加入群组: <海豹测试群>(OpenQQ-Group:102000001-B2C3D4E5F6)，操作者 openid:OPOPENID"
	if joined != want {
		t.Errorf("进群文案 = %q, want %q", joined, want)
	}

	// 没有操作者时不留尾巴
	joinedNoOp := officialQQBotJoinedGroupNoticeText("海豹测试群", groupID, "  ")
	if joinedNoOp != "官方QQ机器人加入群组: <海豹测试群>(OpenQQ-Group:102000001-B2C3D4E5F6)" {
		t.Errorf("无操作者进群文案 = %q", joinedNoOp)
	}

	// 群名缺失时用占位
	leftNoName := officialQQBotLeftGroupNoticeText("", groupID, "OPOPENID", true)
	want = "官方QQ机器人被移出群组: <未知群名>(OpenQQ-Group:102000001-B2C3D4E5F6)，操作者 openid:OPOPENID，已清理群绑定"
	if leftNoName != want {
		t.Errorf("退群文案 = %q, want %q", leftNoName, want)
	}

	leftMissing := officialQQBotLeftGroupNoticeText("海豹测试群", groupID, "", false)
	want = "官方QQ机器人被移出群组: <海豹测试群>(OpenQQ-Group:102000001-B2C3D4E5F6)，未找到群绑定，未做清理"
	if leftMissing != want {
		t.Errorf("无绑定退群文案 = %q, want %q", leftMissing, want)
	}
}

func TestOfficialQQC2CMsgSwitchNoticeText(t *testing.T) {
	diceID := "OpenQQ:102000001-A1B2C3D4E5F6"
	if got := officialQQC2CMsgSwitchNoticeText(true, diceID); got != "官方QQ用户开启「主动消息」推送: "+diceID+"，现在可以向其发送主动消息" {
		t.Errorf("开启文案 = %q", got)
	}
	if got := officialQQC2CMsgSwitchNoticeText(false, diceID); got != "官方QQ用户关闭「主动消息」推送: "+diceID+"，无法再向其发送主动消息" {
		t.Errorf("关闭文案 = %q", got)
	}
}

func TestParseOfficialQQFriendEventRaw(t *testing.T) {
	// 真实形态：网关帧里 d 才是事件体
	frame := &dto.WSPayload{
		RawMessage: []byte(`{"op":0,"t":"FRIEND_ADD","id":"event-1","d":{"openid":"A1B2C3D4E5F6","timestamp":1784570600,"scene":2003,"scene_param":"callback_abc123","short_code":"ab12cd","author":{"union_openid":"DB85"}}}`),
	}
	raw := parseOfficialQQFriendEventRaw(frame)
	if raw.Scene != 2003 || raw.SceneParam != "callback_abc123" || raw.ShortCode != "ab12cd" {
		t.Errorf("解析结果 = %+v", raw)
	}

	// 没有原始报文 / 报文损坏 / 没有 d 时都退化成零值，不能 panic
	for name, payload := range map[string]*dto.WSPayload{
		"nil":       nil,
		"empty":     {},
		"garbage":   {RawMessage: []byte("not json")},
		"no data":   {RawMessage: []byte(`{"op":0,"t":"FRIEND_ADD"}`)},
		"null data": {RawMessage: []byte(`{"d":null}`)},
	} {
		if got := parseOfficialQQFriendEventRaw(payload); got != (officialQQFriendEventRaw{}) {
			t.Errorf("%s: 期望零值，得到 %+v", name, got)
		}
	}
}
func TestOfficialQQMakeHandlersRegistersNoticeEvents(t *testing.T) {
	pa := &PlatformAdapterOfficialQQ{}
	handlers := pa.makeHandlers()

	var friend, groupAdd, groupDel, memberAdd, memberRemove, c2cReceive, c2cReject bool
	for _, handler := range handlers {
		switch handler.(type) {
		case event.C2CFriendEventHandler:
			friend = true
		case event.GroupAddRobotEventHandler:
			groupAdd = true
		case event.GroupDelRobotEventHandler:
			groupDel = true
		case event.GroupMemberAddEventHandler:
			memberAdd = true
		case event.GroupMemberRemoveEventHandler:
			memberRemove = true
		case event.C2CMsgReceiveEventHandler:
			c2cReceive = true
		case event.C2CMsgRejectEventHandler:
			c2cReject = true
		}
	}

	missing := make([]string, 0, 7)
	if !friend {
		missing = append(missing, "C2CFriend（好友增减）")
	}
	if !groupAdd {
		missing = append(missing, "GroupAddRobot（机器人进群）")
	}
	if !groupDel {
		missing = append(missing, "GroupDelRobot（机器人被移出群）")
	}
	if !memberAdd {
		missing = append(missing, "GroupMemberAdd")
	}
	if !memberRemove {
		missing = append(missing, "GroupMemberRemove")
	}
	if !c2cReceive {
		missing = append(missing, "C2CMsgReceive（主动消息开启）")
	}
	if !c2cReject {
		missing = append(missing, "C2CMsgReject（主动消息关闭）")
	}
	if len(missing) > 0 {
		t.Fatalf("makeHandlers 缺少事件订阅：%v", missing)
	}

	// 仅频道模式（OnlyQQGuild）不订阅群/单聊事件
	pa.OnlyQQGuild = true
	for _, handler := range pa.makeHandlers() {
		switch handler.(type) {
		case event.C2CFriendEventHandler, event.GroupAddRobotEventHandler, event.GroupDelRobotEventHandler,
			event.C2CMsgReceiveEventHandler, event.C2CMsgRejectEventHandler:
			t.Fatalf("OnlyQQGuild 模式不应订阅群/单聊事件，却注册了 %T", handler)
		}
	}
}
