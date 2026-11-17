package api

// 官方 QQ「素材管理」：Markdown 图片上传 / 列表 / 删除。
//
// 平台侧每次上传都要 prepare → PUT → complete 三步；浏览器只需要把文件交给这里，
// 由后端完成与腾讯 OpenAPI/COS 的交互。图片宽高平台不返回，上传时由后端解图片头补记。
//
//	GET    /sd-api/official-qq/media-images?endpointId=&cursor=
//	POST   /sd-api/official-qq/media-images   (multipart: file, endpointId)
//	DELETE /sd-api/official-qq/media-images/:imageId?endpointId=

import (
	"io"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"sealdice-core/dice"
)

// officialQQMediaImageMaxBytes 与官方限制一致：单张不超过 10MB。
const officialQQMediaImageMaxBytes = 10 << 20

type officialQQMediaBot struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	AppID string `json:"appId"`
}

// officialQQMediaBots 列出可用于素材管理的官方 QQ 机器人（有 AppID 的端点）。
func officialQQMediaBots() []officialQQMediaBot {
	bots := make([]officialQQMediaBot, 0)
	if myDice == nil || myDice.ImSession == nil {
		return bots
	}
	for _, ep := range myDice.ImSession.EndPoints {
		if ep == nil || ep.Adapter == nil {
			continue
		}
		pa, ok := ep.Adapter.(*dice.PlatformAdapterOfficialQQ)
		if !ok || pa == nil || strings.TrimSpace(pa.AppID) == "" {
			continue
		}
		// 只列已连接的机器人：断线状态下调素材接口必然失败，列出来只会误导。
		if pa.Api == nil {
			continue
		}
		name := strings.TrimSpace(ep.Nickname)
		if name == "" {
			name = pa.AppID
		}
		bots = append(bots, officialQQMediaBot{ID: ep.ID, Name: name, AppID: pa.AppID})
	}
	return bots
}

// officialQQMediaFindAdapter 按端点 ID 找适配器；没指定时退化为第一个已连接的官方机器人。
func officialQQMediaFindAdapter(endpointID string) *dice.PlatformAdapterOfficialQQ {
	if myDice == nil || myDice.ImSession == nil {
		return nil
	}
	var fallback *dice.PlatformAdapterOfficialQQ
	for _, ep := range myDice.ImSession.EndPoints {
		if ep == nil || ep.Adapter == nil {
			continue
		}
		pa, ok := ep.Adapter.(*dice.PlatformAdapterOfficialQQ)
		if !ok || pa == nil || strings.TrimSpace(pa.AppID) == "" {
			continue
		}
		if endpointID != "" {
			if ep.ID == endpointID {
				return pa
			}
			continue
		}
		if fallback == nil && pa.Api != nil {
			fallback = pa
		}
	}
	return fallback
}

func officialQQMediaEndpointID(pa *dice.PlatformAdapterOfficialQQ) string {
	if pa == nil || pa.EndPoint == nil {
		return ""
	}
	return pa.EndPoint.ID
}

// officialQQMediaImagesGet 查当前机器人的有效图片（游标分页）。
func officialQQMediaImagesGet(c echo.Context) error {
	if !doAuth(c) {
		return c.NoContent(http.StatusForbidden)
	}
	if dm != nil && dm.JustForTest {
		return Error(&c, "展示模式不支持该操作", Response{"testMode": true})
	}

	bots := officialQQMediaBots()
	pa := officialQQMediaFindAdapter(strings.TrimSpace(c.QueryParam("endpointId")))
	if pa == nil {
		return Success(&c, Response{
			"bots":       bots,
			"endpointId": "",
			"images":     []any{},
			"total":      0,
		})
	}
	list, err := pa.ListOfficialQQMediaImages(c.QueryParam("cursor"))
	if err != nil {
		return Error(&c, dice.TranslateOfficialQQMediaImageError(err), Response{})
	}
	return Success(&c, Response{
		"bots":       bots,
		"endpointId": officialQQMediaEndpointID(pa),
		"images":     list.Images,
		"total":      list.Total,
		"nextCursor": list.NextCursor,
	})
}

// officialQQMediaImagesUpload 上传一张 Markdown 图片素材。
func officialQQMediaImagesUpload(c echo.Context) error {
	if !doAuth(c) {
		return c.NoContent(http.StatusForbidden)
	}
	if dm != nil && dm.JustForTest {
		return Error(&c, "展示模式不支持该操作", Response{"testMode": true})
	}

	pa := officialQQMediaFindAdapter(strings.TrimSpace(c.FormValue("endpointId")))
	if pa == nil {
		return Error(&c, "没有可用的官方 QQ 机器人连接", Response{})
	}
	header, err := c.FormFile("file")
	if err != nil {
		return Error(&c, "没有收到图片文件", Response{})
	}
	if header.Size > officialQQMediaImageMaxBytes {
		return Error(&c, "图片超过 10MB 上限", Response{})
	}
	src, err := header.Open()
	if err != nil {
		return Error(&c, "读取上传文件失败", Response{})
	}
	defer func() { _ = src.Close() }()
	data, err := io.ReadAll(io.LimitReader(src, officialQQMediaImageMaxBytes+1))
	if err != nil {
		return Error(&c, "读取上传文件失败", Response{})
	}
	if len(data) > officialQQMediaImageMaxBytes {
		return Error(&c, "图片超过 10MB 上限", Response{})
	}

	image, err := pa.UploadOfficialQQMediaImage(header.Filename, data)
	if err != nil {
		return Error(&c, dice.TranslateOfficialQQMediaImageError(err), Response{})
	}
	return Success(&c, Response{"image": image, "endpointId": officialQQMediaEndpointID(pa)})
}

// officialQQMediaImagesDelete 删除图片，释放配额。
func officialQQMediaImagesDelete(c echo.Context) error {
	if !doAuth(c) {
		return c.NoContent(http.StatusForbidden)
	}
	if dm != nil && dm.JustForTest {
		return Error(&c, "展示模式不支持该操作", Response{"testMode": true})
	}

	pa := officialQQMediaFindAdapter(strings.TrimSpace(c.QueryParam("endpointId")))
	if pa == nil {
		return Error(&c, "没有可用的官方 QQ 机器人连接", Response{})
	}
	imageID := strings.TrimSpace(c.Param("imageId"))
	if imageID == "" {
		return Error(&c, "缺少图片 ID", Response{})
	}
	if err := pa.DeleteOfficialQQMediaImage(imageID); err != nil {
		return Error(&c, dice.TranslateOfficialQQMediaImageError(err), Response{})
	}
	return Success(&c, Response{})
}
