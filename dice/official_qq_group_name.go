package dice

// 官方 QQ 群的群名补全：官方只在 /v2/groups/{group_openid}/info 里给群名，
// 而 dice 层的 GroupInfo.GroupName 在处理消息时是空的（尤其是 500 个群的公开机器人，
// 群组管理页会全是「未获取到」）。这里做两件事：
//   1. 聊天控制台已经拿到过群名 → 直接回填，页面立刻有名字；
//   2. 控制台里还没有 → 后台用限流的方式去官方接口拉一次，下次刷新就有了。

import (
	"strings"
	"time"
)

// officialQQGroupNameSem 限制同时进行的群名查询数量（官方 30 QPM，别打满）。
var officialQQGroupNameSem = make(chan struct{}, 2)

// ParseOfficialQQGroupID 把 OpenQQ-Group:<uin>-<group_openid> 拆成两部分。
func ParseOfficialQQGroupID(groupID string) (uin, groupOpenID string, ok bool) {
	const prefix = "OpenQQ-Group:"
	if !strings.HasPrefix(groupID, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(groupID, prefix)
	uin, groupOpenID, found := strings.Cut(rest, "-")
	if !found || uin == "" || groupOpenID == "" {
		return "", "", false
	}
	return uin, groupOpenID, true
}

// FindOfficialQQAdapterByGroupID 找到群所属的官方 QQ 适配器。
func FindOfficialQQAdapterByGroupID(d *Dice, groupID string) (*PlatformAdapterOfficialQQ, string) {
	uin, groupOpenID, ok := ParseOfficialQQGroupID(groupID)
	if !ok || d == nil || d.ImSession == nil {
		return nil, ""
	}
	for _, ep := range d.ImSession.EndPoints {
		pa, ok := ep.Adapter.(*PlatformAdapterOfficialQQ)
		if !ok || pa == nil {
			continue
		}
		if strings.TrimSpace(pa.UIN) == uin {
			return pa, groupOpenID
		}
	}
	return nil, ""
}

// OfficialQQGroupDisplayName 重新计算群显示名：
// 先用聊天控制台里的名字（官方接口拉到的，或骰主手动改的），没有就返回空。
func OfficialQQGroupDisplayName(d *Dice, groupID string) string {
	_, groupOpenID, ok := ParseOfficialQQGroupID(groupID)
	if !ok {
		return ""
	}
	store := d.GetChatConsole()
	if store == nil {
		return ""
	}
	conv := store.Conversation(chatConsoleGroupKey(groupOpenID))
	if conv == nil || conv.NameManual {
		return ""
	}
	// 控制台里的占位名（群 XXXXXX）不算真群名
	if conv.Name == chatConsoleDefaultName(chatConsoleKindGroup, groupOpenID) {
		return ""
	}
	return conv.Name
}

// RefreshOfficialQQGroupName 给一个官方群补群名。
// 控制台里有名字就同步回填；否则起一个后台任务（限流）去官方接口查一次。
// 这个函数可以随便调，重复调用会被 chatConsoleInfoThrottle 挡住。
func RefreshOfficialQQGroupName(d *Dice, groupID string) {
	if d == nil {
		return
	}
	pa, groupOpenID := FindOfficialQQAdapterByGroupID(d, groupID)
	if pa == nil {
		return
	}

	if name := OfficialQQGroupDisplayName(d, groupID); name != "" {
		setOfficialQQGroupName(d, groupID, name)
		return
	}
	if pa.Api == nil {
		return
	}
	if !pa.allowChatConsoleInfoFetch(groupOpenID) {
		return
	}

	go func() {
		select {
		case officialQQGroupNameSem <- struct{}{}:
			defer func() { <-officialQQGroupNameSem }()
		case <-time.After(5 * time.Second):
			// 排队太久就算了，下次刷新再来
			return
		}
		name, memberCount, err := pa.fetchChatConsoleGroupInfo(groupOpenID)
		if err != nil || name == "" {
			return
		}
		setOfficialQQGroupName(d, groupID, name)
		if store := d.GetChatConsole(); store != nil {
			store.SetGroupInfo(chatConsoleGroupKey(groupOpenID), name, memberCount)
		}
	}()
}

// setOfficialQQGroupName 把群名写回 dice 层的 GroupInfo（只在原来是空的时候写）。
func setOfficialQQGroupName(d *Dice, groupID, name string) {
	if d == nil || d.ImSession == nil || name == "" {
		return
	}
	if group, ok := d.ImSession.ServiceAtNew.Load(groupID); ok && group != nil && group.GroupName == "" {
		group.GroupName = name
	}
}
