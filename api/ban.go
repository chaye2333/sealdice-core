package api

import (
	"bufio"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
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
	lst := myDice.GetBanList()
	return c.JSON(http.StatusOK, lst)
}

func banMapDeleteOne(c echo.Context) error {
	v := dice.BanListInfoItem{}
	err := c.Bind(&v)
	if err != nil {
		return c.String(430, err.Error())
	}
	(&myDice.Config).BanList.DeleteByID(myDice, v.ID)
	return c.JSON(http.StatusOK, nil)
}

func banMapAddOne(c echo.Context) error {
	if dm.JustForTest {
		return c.JSON(200, map[string]interface{}{
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

		// 从 ID 前缀推断平台：QQ-Group:123 -> QQ；OpenQQ-Group:... -> QQ（官方 bot 端点 Platform 就是 QQ）
		prefix := strings.Split(v.ID, ":")[0]
		platform := strings.Replace(prefix, "-Group", "", 1)
		if platform == "OpenQQ" {
			platform = "QQ"
		}

		// 只用一个端点做上下文：AddScoreBase 会累加怒气值，多个端点各来一次会重复计数。
		var chosen *dice.EndPointInfo
		for _, i := range myDice.ImSession.EndPoints {
			if i.Platform == platform && i.Enable {
				chosen = i
				break
			}
		}
		// 官方群的平台名与端点 Platform 可能对不上（或该平台端点暂时禁用）：
		// 退化为任意一个启用端点，否则退化为 UI 端点，避免"点了没反应"。
		if chosen == nil {
			for _, i := range myDice.ImSession.EndPoints {
				if i.Enable {
					chosen = i
					break
				}
			}
		}
		if chosen == nil {
			chosen = myDice.UIEndpoint
		}
		if chosen != nil {
			v2 := (&myDice.Config).BanList.AddScoreBase(v.ID, score, "海豹后台", reason, &dice.MsgContext{Dice: myDice, EndPoint: chosen})
			if v2 != nil && v.Name != "" {
				v2.Name = v.Name
			}
		}
	}
	if v.Rank == dice.BanRankTrusted {
		(&myDice.Config).BanList.SetTrustByID(v.ID, "海豹后台", "骰主后台设置")
	}

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
