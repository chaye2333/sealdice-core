package api

import (
	"bufio"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"sealdice-core/dice"
)

func banConfigGet(c echo.Context) error {
	if !doAuth(c) {
		return c.JSON(http.StatusForbidden, nil)
	}

	return c.JSON(http.StatusOK, myDice.Config.BanList)
}

func banConfigSet(c echo.Context) error {
	if !doAuth(c) {
		return c.JSON(http.StatusForbidden, nil)
	}

	v := dice.BanListInfo{}
	err := c.Bind(&v)
	if err != nil {
		return c.String(430, err.Error())
	}

	if v.ThresholdWarn == 0 {
		v.ThresholdWarn = 100
	}

	if v.ThresholdBan == 0 {
		v.ThresholdBan = 200
	}

	config := &myDice.Config
	config.BanList.BanBehaviorRefuseReply = v.BanBehaviorRefuseReply
	config.BanList.BanBehaviorRefuseInvite = v.BanBehaviorRefuseInvite
	config.BanList.BanBehaviorQuitLastPlace = v.BanBehaviorQuitLastPlace
	config.BanList.BanBehaviorQuitPlaceImmediately = v.BanBehaviorQuitPlaceImmediately
	config.BanList.BanBehaviorQuitIfAdmin = v.BanBehaviorQuitIfAdmin
	config.BanList.BanBehaviorQuitIfAdminSilentIfNotAdmin = v.BanBehaviorQuitIfAdminSilentIfNotAdmin
	config.BanList.ScoreReducePerMinute = v.ScoreReducePerMinute
	config.BanList.ThresholdWarn = v.ThresholdWarn
	config.BanList.ThresholdBan = v.ThresholdBan
	config.BanList.ScoreGroupMuted = v.ScoreGroupMuted
	config.BanList.ScoreGroupKicked = v.ScoreGroupKicked
	config.BanList.ScoreTooManyCommand = v.ScoreTooManyCommand

	config.BanList.JointScorePercentOfGroup = v.JointScorePercentOfGroup
	config.BanList.JointScorePercentOfInviter = v.JointScorePercentOfInviter
	config.BanList.BanNotifyIntervalMinutes = v.BanNotifyIntervalMinutes
	myDice.MarkModified()

	return c.JSON(http.StatusOK, myDice.Config.BanList)
}

func banMapList(c echo.Context) error {
	if !doAuth(c) {
		return c.JSON(http.StatusForbidden, nil)
	}
	return c.JSON(http.StatusOK, banListForAPI(myDice))
}

// banMergeLists 把「库里的」和「内存里的」黑白名单条目合并成一份。
//
// 黑白名单不跟着 serve.yaml 自动保存，落库只由三处触发：AfterLoads 里那个每分钟的 cron、
// 程序退出、以及导入 / JS API。而后台拉黑和 .ban 指令改的都只是内存里的 Map，
// 所以"拉黑完立刻刷新列表"（秒级）读到的还是旧库：群组管理页的复核会误报成“操作没有生效”，
// 用户过一会儿再打开黑白名单却又能看到（cron 已经写进去了）。内存里的条目更新鲜，优先它。
func banMergeLists(dbItems []*dice.BanListInfoItem, memory *dice.SyncMap[string, *dice.BanListInfoItem]) []*dice.BanListInfoItem {
	byID := make(map[string]*dice.BanListInfoItem, len(dbItems))
	for _, item := range dbItems {
		if item == nil || item.ID == "" {
			continue
		}
		byID[item.ID] = item
	}
	if memory != nil {
		memory.Range(func(id string, item *dice.BanListInfoItem) bool {
			if item == nil {
				return true
			}
			tmp := *item // 拷一份，别让调用方改到运行时那张表里的对象
			if tmp.ID == "" {
				tmp.ID = id
			}
			byID[tmp.ID] = &tmp
			return true
		})
	}
	lst := make([]*dice.BanListInfoItem, 0, len(byID))
	for _, item := range byID {
		lst = append(lst, item)
	}
	slices.SortFunc(lst, func(a, b *dice.BanListInfoItem) int { return strings.Compare(a.ID, b.ID) })
	return lst
}

// banListForAPI 返回给后台的黑白名单：库 + 内存合并后的结果。
func banListForAPI(d *dice.Dice) []*dice.BanListInfoItem {
	if d == nil {
		return nil
	}
	var memory *dice.SyncMap[string, *dice.BanListInfoItem]
	if d.Config.BanList != nil {
		memory = d.Config.BanList.Map
	}
	return banMergeLists(d.GetBanList(), memory)
}

func banMapDeleteOne(c echo.Context) error {
	if !doAuth(c) {
		return c.JSON(http.StatusForbidden, nil)
	}
	if dm != nil && dm.JustForTest {
		return c.JSON(http.StatusOK, map[string]interface{}{
			"testMode": true,
		})
	}
	v := dice.BanListInfoItem{}
	err := c.Bind(&v)
	if err != nil {
		return c.String(430, err.Error())
	}
	if strings.TrimSpace(v.ID) == "" {
		return c.String(430, "ID 不能为空")
	}
	(&myDice.Config).BanList.DeleteByID(myDice, v.ID)
	return c.JSON(http.StatusOK, nil)
}

// banPickEndpoint 给 AddScoreBase 挑一个和 ID 同源的端点做上下文。
//
// 为什么不让所有端点都来一遍：AddScoreBase 会累加怒气值，多端点各来一次会重复计数；
// 而它只在"首次跨过阈值"时用到 ctx（Platform 为 QQ 时还会顺手删好友，参数是 "海豹后台"
// 这种占位串），所以这里挑最贴近那个 ID 的端点：官方 ID 用官方适配器、民间 ID 优先同平台
// 的民间适配器；实在没有匹配的就退回 UI 端点（Platform=UI，不会触发删好友），
// 既保证"后台点了拉黑真的生效"，又不会拿不相干的端点去打腾讯接口。
func banPickEndpoint(d *dice.Dice, id string) *dice.EndPointInfo {
	if d == nil || d.ImSession == nil {
		return nil
	}
	// OpenQQ-Group:… / OpenQQ:… / OpenQQ-Member:… 都来自官方机器人适配器
	wantOfficial := strings.HasPrefix(id, "OpenQQ")
	prefix := strings.SplitN(id, ":", 2)[0]
	platform := strings.Replace(prefix, "-Group", "", 1)
	if platform == "OpenQQ" {
		platform = "QQ"
	}

	var samePlatform *dice.EndPointInfo
	for _, ep := range d.ImSession.EndPoints {
		if ep == nil || !ep.Enable {
			continue
		}
		if ep.Platform == platform && samePlatform == nil {
			samePlatform = ep
		}
		_, isOfficial := ep.Adapter.(*dice.PlatformAdapterOfficialQQ)
		if isOfficial == wantOfficial {
			// 官方 ID → 官方适配器；民间 ID → 同平台的民间适配器
			if wantOfficial || ep.Platform == platform {
				return ep
			}
		}
	}
	if samePlatform != nil {
		return samePlatform
	}
	return d.UIEndpoint
}

func banMapAddOne(c echo.Context) error {
	if !doAuth(c) {
		return c.JSON(http.StatusForbidden, nil)
	}
	if dm != nil && dm.JustForTest {
		return c.JSON(http.StatusOK, map[string]interface{}{
			"testMode": true,
		})
	}

	v := dice.BanListInfoItem{}
	err := c.Bind(&v)
	if err != nil {
		return c.String(430, err.Error())
	}
	if v.Rank == dice.BanRankBanned {
		score := myDice.Config.BanList.ThresholdBan
		reason := "骰主后台设置"
		if len(v.Reasons) > 0 {
			reason = v.Reasons[0]
		}

		// 挑一个和 ID 同源的端点做上下文：只用一个，避免多端点重复累加怒气值。
		if chosen := banPickEndpoint(myDice, v.ID); chosen != nil {
			v2 := (&myDice.Config).BanList.AddScoreBase(v.ID, score, "海豹后台", reason, &dice.MsgContext{Dice: myDice, EndPoint: chosen})
			if v2 != nil && v.Name != "" {
				v2.Name = v.Name
			}
		}
	}
	if v.Rank == dice.BanRankTrusted {
		(&myDice.Config).BanList.SetTrustByID(v.ID, "海豹后台", "骰主后台设置")
	}

	// 立刻落库：黑白名单不跟着 serve.yaml 自动保存，只由每分钟的 cron / 退出 / 导入落库，
	// 而界面拉黑完会马上重新拉名单复核，等不了那最多 60 秒。不在这里写的话，
	// 复核读到的库还是旧的（群组管理页报“没有变化”），进程被强杀时这条拉黑也会丢。
	(&myDice.Config).BanList.SaveChanged(myDice)

	return c.JSON(http.StatusOK, nil)
}

//
// func banMapSet(c echo.Context) error {
//	if !doAuth(c) {
//		return c.JSON(http.StatusForbidden, nil)
//	}
//
//	v := struct {
//		data []byte
//	}{}
//	err := c.Bind(&v)
//	if err != nil {
//		return c.String(430, err.Error())
//	}
//
//	(&myDice.Config).BanList.LoadMapFromJSON(v.data)
//	return c.JSON(http.StatusOK, nil)
//}

func banExport(c echo.Context) error {
	if dm.JustForTest {
		return Error(&c, "展示模式不支持该操作", Response{"testMode": true})
	}

	lst := myDice.GetBanList()

	temp, _ := os.CreateTemp("", "黑白名单-*.json")
	defer func() {
		_ = temp.Close()
		_ = os.RemoveAll(temp.Name()) //nolint:gosec
	}()
	writer := bufio.NewWriter(temp)
	err := json.NewEncoder(writer).Encode(&lst)
	_ = writer.Flush()
	if err != nil {
		return Error(&c, err.Error(), Response{})
	}

	c.Response().Header().Add("Cache-Control", "no-store")
	return c.Attachment(temp.Name(), "黑白名单.json")
}

func banImport(c echo.Context) error {
	if !doAuth(c) {
		return c.NoContent(http.StatusForbidden)
	}
	if dm.JustForTest {
		return Error(&c, "展示模式不支持该操作", Response{"testMode": true})
	}

	file, err := c.FormFile("file")
	if err != nil {
		return Error(&c, err.Error(), Response{})
	}
	src, err := file.Open()
	if err != nil {
		return Error(&c, err.Error(), Response{})
	}
	defer func(src multipart.File) {
		_ = src.Close()
	}(src)

	var lst []*dice.BanListInfoItem
	data, err := io.ReadAll(src)
	if err != nil {
		return Error(&c, err.Error(), Response{})
	}
	err = json.Unmarshal(data, &lst)
	if err != nil {
		return Error(&c, err.Error(), Response{})
	}

	now := time.Now()
	for _, item := range lst {
		item.UpdatedAt = now.Unix()
		var newReasons []string
		for _, reason := range item.Reasons {
			if !strings.HasSuffix(reason, "（来自导入）") {
				newReasons = append(newReasons, reason+"（来自导入）")
			}
		}
		item.Reasons = newReasons
		(&myDice.Config).BanList.Map.Store(item.ID, item)
	}
	(&myDice.Config).BanList.SaveChanged(myDice)

	return Success(&c, Response{})
}
