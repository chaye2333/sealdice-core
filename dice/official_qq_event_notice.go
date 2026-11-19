package dice

// 官方 QQ 的事件通知。
//
// 好友增减、机器人进退群、单聊「主动消息」开关这几类事件以前只写日志，
// 骰主看不到「谁加了骰子」「骰子被谁踢了」。这里把事件转成既有的
// NoticeType 分类（invite / group / system）后用 ctx.Notice 发出去，
// 文案与 OneBot 适配器保持同一套说法，发送只经过官方 QQ 端点，
// 目标由骰主在通知列表里自行添加 OpenQQ:<uin>-<openid>。

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/sealdice/botgo/dto"
)

// officialQQFriendEventRaw 是 FRIEND_ADD 事件里 botgo 尚未建模的展示字段。
type officialQQFriendEventRaw struct {
	Scene      int    `json:"scene"`
	SceneParam string `json:"scene_param"`
	ShortCode  string `json:"short_code"`
}

// officialQQUnknownGroupNameSentinel 是 DiceManager 群名缓存未命中时返回的占位串。
const officialQQUnknownGroupNameSentinel = "%未知群名%"

// officialQQFriendSceneTexts 把 FRIEND_ADD 的 scene 场景值翻译成中文。
// 取值见官方文档「用户添加好友」事件。
var officialQQFriendSceneTexts = map[int]string{
	1000: "默认",
	1001: "搜索（全部）",
	1002: "搜索（机器人）",
	1003: "群场景",
	1004: "空间场景",
	2001: "站内分享资料页",
	2002: "站外分享资料页",
	2003: "分享链接（站内）",
	2004: "分享链接（站外）",
}

func officialQQFriendSceneText(scene int) string {
	return officialQQFriendSceneTexts[scene]
}

// officialQQOpenIDNickname 用 openid 末 4 位拼一个可读的临时昵称。
// 官方 QQ 的好友 / 成员事件只给 openid，不带昵称。
func officialQQOpenIDNickname(openID string) string {
	if len(openID) >= 4 {
		return "用户" + openID[len(openID)-4:]
	}
	return "用户"
}

// parseOfficialQQFriendEventRaw 从原始事件体里补齐 scene 等字段。
// botgo 的 C2CFriendData 只有 openid / timestamp，这里按需自取，
// 免得为了几个展示字段去改依赖。
func parseOfficialQQFriendEventRaw(event *dto.WSPayload) officialQQFriendEventRaw {
	var raw officialQQFriendEventRaw
	if event == nil || len(event.RawMessage) == 0 {
		return raw
	}
	var envelope struct {
		D json.RawMessage `json:"d"`
	}
	if err := json.Unmarshal(event.RawMessage, &envelope); err != nil || len(envelope.D) == 0 {
		return raw
	}
	if err := json.Unmarshal(envelope.D, &raw); err != nil {
		return officialQQFriendEventRaw{}
	}
	return raw
}

// officialQQFriendAddNoticeText 生成「有人加了骰子好友」的通知。
func officialQQFriendAddNoticeText(data *dto.WSC2CFriendData, raw officialQQFriendEventRaw, diceID string) string {
	text := fmt.Sprintf("收到官方QQ好友添加: <%s>(%s)", officialQQOpenIDNickname(data.OpenID), diceID)
	extras := make([]string, 0, 3)
	if scene := officialQQFriendSceneText(raw.Scene); scene != "" {
		extras = append(extras, "来源:"+scene)
	}
	if param := strings.TrimSpace(raw.SceneParam); param != "" {
		extras = append(extras, "备注:"+strconv.Quote(param))
	}
	if code := strings.TrimSpace(raw.ShortCode); code != "" {
		extras = append(extras, "短链:"+code)
	}
	if len(extras) > 0 {
		text += "，" + strings.Join(extras, "，")
	}
	return text
}

// officialQQFriendDelNoticeText 生成「有人把骰子删了好友」的通知。
func officialQQFriendDelNoticeText(data *dto.WSC2CFriendData, diceID string) string {
	return fmt.Sprintf("官方QQ好友移除: <%s>(%s)", officialQQOpenIDNickname(data.OpenID), diceID)
}

// officialQQGroupNoticeText 拼装官方 QQ 群变动的通知正文。
func officialQQGroupNoticeText(head, groupName, groupID, opOpenID string) string {
	name := strings.TrimSpace(groupName)
	if name == "" {
		name = "未知群名"
	}
	text := fmt.Sprintf("%s: <%s>(%s)", head, name, groupID)
	if op := strings.TrimSpace(opOpenID); op != "" {
		text += "，操作者 openid:" + op
	}
	return text
}

// officialQQBotJoinedGroupNoticeText 生成「机器人被拉进群」的通知。
func officialQQBotJoinedGroupNoticeText(groupName, groupID, opOpenID string) string {
	return officialQQGroupNoticeText("官方QQ机器人加入群组", groupName, groupID, opOpenID)
}

// officialQQBotLeftGroupNoticeText 生成「机器人被移出群」的通知。
func officialQQBotLeftGroupNoticeText(groupName, groupID, opOpenID string, bindingRemoved bool) string {
	text := officialQQGroupNoticeText("官方QQ机器人被移出群组", groupName, groupID, opOpenID)
	if bindingRemoved {
		return text + "，已清理群绑定"
	}
	return text + "，未找到群绑定，未做清理"
}

// officialQQC2CMsgSwitchNoticeText 生成「用户开关主动消息推送」的通知。
func officialQQC2CMsgSwitchNoticeText(enabled bool, diceID string) string {
	if enabled {
		return "官方QQ用户开启「主动消息」推送: " + diceID + "，现在可以向其发送主动消息"
	}
	return "官方QQ用户关闭「主动消息」推送: " + diceID + "，无法再向其发送主动消息"
}

// officialQQGroupNameForNotice 给通知取一个能读懂的群名。
// 官方 QQ 的群名只存在群对象上（聊天控制台回填），DiceManager 的缓存是启动时
// 从群对象同步过去的，可能还是旧值，所以两处都看一遍。
func officialQQGroupNameForNotice(session *IMSession, dm *DiceManager, groupID string) string {
	if session != nil && session.ServiceAtNew != nil {
		if group, ok := session.ServiceAtNew.Load(groupID); ok && group != nil {
			if name := strings.TrimSpace(group.GroupName); name != "" {
				return name
			}
		}
	}
	if dm != nil {
		if name := strings.TrimSpace(dm.TryGetGroupName(groupID)); name != "" && name != officialQQUnknownGroupNameSentinel {
			return name
		}
	}
	return "未知群名"
}

// officialQQNoticeContext 为事件通知单独建一个上下文。
// 不复用消息事件的 $tMsgID / $tEventID：通知走的是主动消息，不能占掉被动回复
// 窗口（官方限制同一条消息最多回复 4 次），否则会把入群 / 好友致辞挤掉。
func (pa *PlatformAdapterOfficialQQ) officialQQNoticeContext() *MsgContext {
	session := pa.EndPoint.Session
	return &MsgContext{EndPoint: pa.EndPoint, Session: session, Dice: session.Parent}
}
