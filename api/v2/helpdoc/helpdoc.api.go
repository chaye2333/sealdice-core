package helpdoc

import (
	"context"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"sealdice-core/dice"
	"sealdice-core/model/common/request"
	"sealdice-core/model/common/response"
)

type Service struct {
	dice *dice.Dice
	dm   *dice.DiceManager
}

func NewService(dm *dice.DiceManager) *Service {
	return &Service{
		dice: dm.GetDice(),
		dm:   dm,
	}
}

func (s *Service) RegisterRoutes(grp *huma.Group) {
	huma.Get(grp, "/status", s.GetStatus, func(o *huma.Operation) {
		o.Description = "获取帮助文档加载状态"
	})
	huma.Get(grp, "/tree", s.GetTree, func(o *huma.Operation) {
		o.Description = "获取帮助文档文件树"
	})
	huma.Get(grp, "/items/page", s.GetItemsPage, func(o *huma.Operation) {
		o.Description = "分页查询帮助词条"
	})
	huma.Get(grp, "/config", s.GetConfig, func(o *huma.Operation) {
		o.Description = "获取帮助文档配置"
	})
}

func (s *Service) RegisterProtectedRoutes(grp *huma.Group) {
	huma.Post(grp, "/reload", s.Reload, func(o *huma.Operation) {
		o.Description = "重载帮助文档"
	})
	huma.Post(grp, "/delete", s.Delete, func(o *huma.Operation) {
		o.Description = "删除帮助文档文件"
	})
	huma.Post(grp, "/config", s.SetConfig, func(o *huma.Operation) {
		o.Description = "保存帮助文档配置"
	})
	// 上传（含分片/断点续传）统一走 tus 上传网关 /sd-api/v2/upload，
	// 见 api/v2/uploadgw。
}

func (s *Service) GetStatus(_ context.Context, _ *request.Empty) (*response.ItemResponse[StatusResp], error) {
	if s.dm.JustForTest {
		return response.NewItemResponse(StatusResp{Loading: false, TestMode: true}), nil
	}
	return response.NewItemResponse(StatusResp{Loading: s.dm.IsHelpReloading}), nil
}

func (s *Service) GetTree(_ context.Context, _ *request.Empty) (*response.ItemResponse[TreeResp], error) {
	if s.dm.JustForTest {
		return nil, huma.Error400BadRequest("展示模式不支持该操作")
	}
	if s.dm.IsHelpReloading {
		return nil, huma.Error409Conflict("帮助文件正在加载")
	}
	if s.dm.Help == nil {
		return response.NewItemResponse(TreeResp{Data: []*dice.HelpDoc{}}), nil
	}
	return response.NewItemResponse(TreeResp{Data: s.dm.Help.HelpDocTree}), nil
}

func (s *Service) Reload(_ context.Context, _ *request.Empty) (*response.ItemResponse[SimpleResp], error) {
	if s.dm.JustForTest {
		return response.NewItemResponse(SimpleResp{Success: false, TestMode: true}), nil
	}
	if s.dm.IsHelpReloading {
		return nil, huma.Error409Conflict("帮助文档正在重新装载")
	}
	s.dm.IsHelpReloading = true
	if s.dm.Help != nil {
		s.dm.Help.Close()
	}
	s.dm.InitHelp()
	return response.NewItemResponse(SimpleResp{Success: true}), nil
}

func (s *Service) Delete(_ context.Context, req *DeleteReq) (*response.ItemResponse[SimpleResp], error) {
	if s.dm.JustForTest {
		return response.NewItemResponse(SimpleResp{Success: false, TestMode: true}), nil
	}
	if s.dm.Help == nil {
		return nil, huma.Error400BadRequest("帮助文档未初始化")
	}
	if err := s.dm.Help.DeleteHelpDoc(req.Body.Keys); err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	return response.NewItemResponse(SimpleResp{Success: true}), nil
}

func (s *Service) GetItemsPage(_ context.Context, req *HelpTextItemQuery) (*response.ItemResponse[HelpTextItemPageResp], error) {
	if s.dm.Help == nil {
		return response.NewItemResponse(HelpTextItemPageResp{Data: dice.HelpTextVos{}}), nil
	}
	pageNum := req.PageNum
	if pageNum <= 0 {
		pageNum = 1
	}
	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = 20
	}
	total, data := s.dm.Help.GetHelpItemPage(pageNum, pageSize, strings.TrimSpace(req.ID), strings.TrimSpace(req.Group), strings.TrimSpace(req.From), strings.TrimSpace(req.Title))
	return response.NewItemResponse(HelpTextItemPageResp{
		Total:    total,
		Data:     data,
		PageNum:  pageNum,
		PageSize: pageSize,
	}), nil
}

func (s *Service) GetConfig(_ context.Context, _ *request.Empty) (*response.ItemResponse[ConfigResp], error) {
	if s.dm.IsHelpReloading {
		return nil, huma.Error409Conflict("帮助文档正在重新装载")
	}
	resp := ConfigResp{Aliases: map[string][]string{}}
	if s.dm.Help != nil && s.dm.Help.Config != nil {
		if s.dm.Help.Config.Aliases != nil {
			resp.Aliases = s.dm.Help.Config.Aliases
		}
		// fork 新增：帮助文档图片化渲染
		resp.ImageRenderEnable = s.dm.Help.Config.ImageRenderEnable
		resp.ImageRenderMinLength = s.dm.Help.Config.ImageRenderMinLength
		resp.ImageRenderMaskInImage = s.dm.Help.Config.ImageRenderMaskInImage
		resp.ImageRenderURL = s.dm.Help.Config.ImageRenderURL
		resp.ImageRenderToken = s.dm.Help.Config.ImageRenderToken
		resp.ImageRenderTimeoutSec = s.dm.Help.Config.ImageRenderTimeoutSec
		resp.ImageRenderWidth = s.dm.Help.Config.ImageRenderWidth
	}
	return response.NewItemResponse(resp), nil
}

func (s *Service) SetConfig(_ context.Context, req *ConfigReq) (*response.ItemResponse[SimpleResp], error) {
	if s.dm.JustForTest {
		return response.NewItemResponse(SimpleResp{Success: false, TestMode: true}), nil
	}
	if s.dm.IsHelpReloading {
		return nil, huma.Error409Conflict("帮助文档正在重新装载")
	}
	if s.dm.Help == nil {
		return nil, huma.Error400BadRequest("帮助文档未初始化")
	}
	aliases := req.Body.Aliases
	if aliases == nil {
		aliases = map[string][]string{}
	}
	body := req.Body
	// fork 新增：帮助文档图片化渲染。URL 去掉首尾空白；其余数值保持 0 表示"用默认值"，
	// 具体默认值（15 秒 / 820px）由 dice 层兜底。
	cfg := &dice.HelpConfig{
		Aliases:                aliases,
		ImageRenderEnable:      body.ImageRenderEnable,
		ImageRenderMinLength:   body.ImageRenderMinLength,
		ImageRenderMaskInImage: body.ImageRenderMaskInImage,
		ImageRenderURL:         strings.TrimSpace(body.ImageRenderURL),
		ImageRenderToken:       strings.TrimSpace(body.ImageRenderToken),
		ImageRenderTimeoutSec:  body.ImageRenderTimeoutSec,
		ImageRenderWidth:       body.ImageRenderWidth,
	}
	if err := s.dm.Help.SaveHelpConfig(cfg); err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	return response.NewItemResponse(SimpleResp{Success: true}), nil
}
