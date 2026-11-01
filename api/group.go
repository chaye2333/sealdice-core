package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"sealdice-core/dice"
	"sealdice-core/dice/service"
)

func groupList(c echo.Context) error {
	var items []*dice.GroupInfo
	var groupIDs []string
	// Pinenutn: Range模板 ServiceAtNew重构代码
	myDice.ImSession.ServiceAtNew.Range(func(groupID string, item *dice.GroupInfo) bool {
		// Pinenutn: ServiceAtNew重构
		if item == nil {
			return true
		}
		item.GroupID = groupID
		if !strings.HasPrefix(item.GroupID, "PG-") {
			var exts []string
			// 使用 Raw 版本避免触发全量初始化
			for _, i := range item.GetActivatedExtListRaw() {
				if i != nil {
					exts = append(exts, i.Name)
				}
			}
			item.TmpExtList = exts

			if item.DiceIDExistsMap.Len() > 0 {
				items = append(items, item)
				groupIDs = append(groupIDs, groupID)
			}
		}
		return true
	})

	// 玩家数一次查完：以前是每个群一次 COUNT，几百个群就是几百次查询。
	if myDice.DBOperator != nil {
		playerNum, err := service.GroupPlayerNumGetBatch(myDice.DBOperator, groupIDs)
		if err != nil {
			myDice.Logger.Warnf("统计群玩家数失败: %v", err)
		}
		for _, item := range items {
			item.TmpPlayerNum = playerNum[item.GroupID]
		}
	}

	// 官方 QQ 群的群名官方只在 /v2/groups/{id}/info 里给，dice 层的 GroupName 通常是空的。
	// 这里先用聊天控制台已经拿到的名字回填；还没有的，登录状态下顺手在后台拉一次。
	// 群资料（简介 / 分类 / 标签）一次查完，避免每个群一次 SQL。
	var convByOpenID map[string]*dice.ChatConsoleConversation
	authed := doAuth(c)
	for _, item := range items {
		if !strings.HasPrefix(item.GroupID, "OpenQQ-Group:") {
			continue
		}
		// 群 ID 一定是 <uin>-<group_openid>；万一格式不对就只做群名回填，不因为解析失败而跳过
		if _, groupOpenID, ok := dice.ParseOfficialQQGroupID(item.GroupID); ok {
			if convByOpenID == nil {
				convByOpenID = dice.OfficialQQGroupConsoleInfoByTarget(myDice)
			}
			if conv := convByOpenID[groupOpenID]; conv != nil {
				item.TmpFingerMemo = conv.FingerMemo
				item.TmpClassText = conv.ClassText
				item.TmpGroupTags = conv.Tags
			}
		}
		if item.GroupName != "" {
			continue
		}
		if name := dice.OfficialQQGroupDisplayName(myDice, item.GroupID); name != "" {
			item.GroupName = name
			continue
		}
		if authed {
			dice.RefreshOfficialQQGroupName(myDice, item.GroupID)
		}
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"items": items,
	})
}

func groupSetOne(c echo.Context) error {
	if !doAuth(c) {
		return c.JSON(http.StatusForbidden, nil)
	}
	if dm.JustForTest {
		return c.JSON(200, map[string]interface{}{
			"testMode": true,
		})
	}

	v := struct {
		Active  bool   `json:"active"  yaml:"active"`
		GroupID string `json:"groupId" yaml:"groupId"`
		DiceID  string `json:"diceId"  yaml:"diceId"`
	}{}
	err := c.Bind(&v)

	if err == nil {
		_, exists := myDice.ImSession.ServiceAtNew.Load(v.GroupID)
		if exists {
			for _, ep := range myDice.ImSession.EndPoints {
				// if ep.UserId == v.DiceId {
				ctx := &dice.MsgContext{Dice: myDice, EndPoint: ep, Session: myDice.ImSession}
				if v.Active {
					dice.SetBotOnAtGroup(ctx, v.GroupID)
				} else {
					dice.SetBotOffAtGroup(ctx, v.GroupID)
				}
				//}
			}
		}
		return c.String(http.StatusOK, "")
	}
	return c.String(430, "")
}

func groupQuit(c echo.Context) error {
	if !doAuth(c) {
		return c.JSON(http.StatusForbidden, nil)
	}
	if dm.JustForTest {
		return c.JSON(200, map[string]interface{}{
			"testMode": true,
		})
	}
	v := struct {
		GroupID   string `json:"groupId"   yaml:"groupId"`
		DiceID    string `json:"diceId"    yaml:"diceId"`
		Silence   bool   `json:"silence"   yaml:"silence"`
		ExtraText string `json:"extraText" yaml:"extraText"`
	}{}
	err := c.Bind(&v)
	if err != nil {
		return c.String(430, "")
	}

	// 不太好弄，主要会出现多个帐号在群的情况
	group, exists := myDice.ImSession.ServiceAtNew.Load(v.GroupID)
	if !exists {
		return c.String(430, "")
	}

	for _, ep := range myDice.ImSession.EndPoints {
		if ep.UserID != v.DiceID {
			continue
		}
		// 就是这个
		_txt := fmt.Sprintf("Master后台操作退群: 于群组<%s>(%s)中告别", group.GroupName, group.GroupID)
		myDice.Logger.Info(_txt)

		ctx := &dice.MsgContext{Dice: myDice, EndPoint: ep, Session: myDice.ImSession}
		ctx.Notice(_txt, dice.NoticeTypeGroup)
		// dice.SetBotOffAtGroup(ctx, group.GroupId)

		if !v.Silence {
			txtPost := dice.DiceFormatTmpl(ctx, "核心:提示_手动退群前缀")
			if v.ExtraText != "" {
				txtPost += "\n骰主留言: " + v.ExtraText
			}
			dice.ReplyGroup(ctx, &dice.Message{GroupID: v.GroupID}, txtPost)
		}

		group.DiceIDExistsMap.Delete(v.DiceID)
		time.Sleep(6 * time.Second)
		group.MarkDirty(myDice)

		ep.Adapter.QuitGroup(ctx, v.GroupID)
		return c.String(http.StatusOK, "")
	}
	return c.String(430, "")
}
