package helpdoc

import (
	"sealdice-core/dice"
)

type StatusResp struct {
	Loading  bool `json:"loading"`
	TestMode bool `json:"testMode,omitempty"`
}

type TreeResp struct {
	Data []*dice.HelpDoc `json:"data"`
}

type HelpTextItemQuery struct {
	PageNum  int    `query:"pageNum"`
	PageSize int    `query:"pageSize"`
	ID       string `query:"id"`
	Group    string `query:"group"`
	From     string `query:"from"`
	Title    string `query:"title"`
}

type HelpTextItemPageResp struct {
	Total    int              `json:"total"`
	Data     dice.HelpTextVos `json:"data"`
	PageNum  int              `json:"pageNum"`
	PageSize int              `json:"pageSize"`
}

type HelpConfigBody struct {
	Aliases map[string][]string `json:"aliases"`

	// fork 新增：帮助文档图片化渲染。
	//
	// 背景：TRPG 帮助文档里可能有敏感词，或者一堆正常词组合起来像宗教传播。
	// 命中敏感词、或正文过长时，改发一张**现场渲染**的图片（不缓存、一次性），
	// 由骰主自部署的渲染后端出图（见 helpdoc-render）。
	// 只影响 .help / .find 这类帮助文档正文，渲染失败一律回退成文本。
	ImageRenderEnable      bool   `json:"imageRenderEnable"`
	ImageRenderMinLength   int    `json:"imageRenderMinLength"`
	ImageRenderMaskInImage bool   `json:"imageRenderMaskInImage"`
	ImageRenderURL         string `json:"imageRenderUrl"`
	ImageRenderToken       string `json:"imageRenderToken"`
	ImageRenderTimeoutSec  int    `json:"imageRenderTimeoutSec"`
	ImageRenderWidth       int    `json:"imageRenderWidth"`
}

type ConfigReq struct {
	Body HelpConfigBody `json:"body"`
}

type ConfigResp = HelpConfigBody

type DeleteReqBody struct {
	Keys []string `json:"keys"`
}

type DeleteReq struct {
	Body DeleteReqBody `json:"body"`
}

type SimpleResp struct {
	Success  bool   `json:"success"`
	TestMode bool   `json:"testMode,omitempty"`
	Err      string `json:"err,omitempty"`
}
