package dice

// 官方 QQ 群的群名补全：官方只在 /v2/groups/{group_openid}/info 里给群名，
// 而 dice 层的 GroupInfo.GroupName 在处理消息时是空的（尤其是 500 个群的公开机器人，
// 群组管理页会全是「未获取到」）。这里做两件事：
//   1. 聊天控制台已经拿到过群名 → 直接回填，页面立刻有名字；
//   2. 控制台里还没有 → 后台用限流的方式去官方接口拉一次，下次刷新就有了。

import (
	"strings"
	"sync"
	"time"
)

// officialQQGroupNameSem 限制同时进行的群名查询数量（官方 30 QPM，别打满）。
var officialQQGroupNameSem = make(chan struct{}, 2)

// officialQQGroupNameQueue 限制"排队中"的群名查询数量。
// 群组管理页一次可能给几百个群补名，不设上限就是几百个 goroutine 一起等信号量；
// 队列满了就直接放弃，这次没轮到的群下次刷新接着补，不会漏。
var officialQQGroupNameQueue = make(chan struct{}, 64)

// officialQQGroupNamePending 保证同一个群在同一时刻只排一个任务（页面被反复刷新时）。
var officialQQGroupNamePending sync.Map // groupID -> struct{}

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

// OfficialQQGroupConsoleInfoByTarget 返回 group_openid → 会话 的映射，供群列表批量补群资料。
// 群列表一次可能有几百个群，逐个 Conversation() 就是几百次 SQL；这里一次查完。
func OfficialQQGroupConsoleInfoByTarget(d *Dice) map[string]*ChatConsoleConversation {
	if d == nil {
		return nil
	}
	store := d.GetChatConsole()
	if store == nil {
		return nil
	}
	convs := store.Conversations()
	out := make(map[string]*ChatConsoleConversation, len(convs))
	for _, conv := range convs {
		if conv != nil && conv.Kind == chatConsoleKindGroup {
			out[conv.TargetID] = conv
		}
	}
	return out
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

	// 同一个群同时只排一个任务：群列表被反复刷新时不要堆一串重复请求。
	if _, loaded := officialQQGroupNamePending.LoadOrStore(groupID, struct{}{}); loaded {
		return
	}
	select {
	case officialQQGroupNameQueue <- struct{}{}:
	default:
		// 队列已满（一次要补几百个群），这次跳过，下次刷新接着补。
		officialQQGroupNamePending.Delete(groupID)
		return
	}

	go func() {
		defer officialQQGroupNamePending.Delete(groupID)
		defer func() { <-officialQQGroupNameQueue }()

		select {
		case officialQQGroupNameSem <- struct{}{}:
			defer func() { <-officialQQGroupNameSem }()
		case <-time.After(5 * time.Second):
			// 排队太久就算了，这次不占节流额度，下次刷新再来
			return
		}
		// 真正要发请求了才打节流戳：排队超时/放弃的不算一次尝试，
		// 否则那个群会被静默锁 10 分钟，却一次都没查过。
		if !pa.allowChatConsoleInfoFetch(groupOpenID) {
			return
		}
		info, err := pa.fetchChatConsoleGroupInfo(groupOpenID)
		if err != nil || info == nil {
			return
		}
		if info.Name != "" {
			setOfficialQQGroupName(d, groupID, info.Name)
		}
		if store := d.GetChatConsole(); store != nil {
			store.SetGroupInfo(chatConsoleGroupKey(groupOpenID), *info)
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
