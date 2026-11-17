package api //nolint:testpackage

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"sealdice-core/dice"
)

// 素材管理接口的鉴权/空列表路径：有 token 时不能返回 403，
// 没有官方机器人时给空列表而不是报错。
func TestOfficialQQMediaImagesAuthAndEmptyList(t *testing.T) {
	token := "official-qq-media-test-token"
	manager := &dice.DiceManager{}
	manager.AccessTokens.Store(token, true)
	testDice := &dice.Dice{
		Parent:     manager,
		BaseConfig: dice.BaseConfig{DataDir: t.TempDir()},
		ImSession:  &dice.IMSession{},
	}
	oldDice, oldDM := myDice, dm
	dm, myDice = manager, testDice
	t.Cleanup(func() { myDice, dm = oldDice, oldDM })

	e := echo.New()
	e.GET("/sd-api/official-qq/media-images", officialQQMediaImagesGet)
	e.POST("/sd-api/official-qq/media-images", officialQQMediaImagesUpload)
	e.DELETE("/sd-api/official-qq/media-images/:imageId", officialQQMediaImagesDelete)

	// 不带 token：和聊天控制台一样返回 403
	req := httptest.NewRequest(http.MethodGet, "/sd-api/official-qq/media-images", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no token: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 带 token：200 + 空列表（没有官方机器人时不应 403）
	req = httptest.NewRequest(http.MethodGet, "/sd-api/official-qq/media-images", nil)
	req.Header.Set("Token", token)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with token: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 反向代理丢掉自定义 token 头时，标准 Authorization 也要能通过（v2 一直如此）
	for _, auth := range []string{"Bearer " + token, token} {
		req = httptest.NewRequest(http.MethodGet, "/sd-api/official-qq/media-images", nil)
		req.Header.Set("Authorization", auth)
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("authorization %q: code=%d body=%s", auth, rec.Code, rec.Body.String())
		}
	}
	var resp struct {
		Result bool   `json:"result"`
		Bots   []any  `json:"bots"`
		Images []any  `json:"images"`
		Total  uint64 `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if !resp.Result || len(resp.Bots) != 0 || len(resp.Images) != 0 || resp.Total != 0 {
		t.Fatalf("unexpected response: %+v", resp)
	}

	// 上传：有 token 但没有机器人时是业务错误（HTTP 200, result:false），不是 403
	req = httptest.NewRequest(http.MethodPost, "/sd-api/official-qq/media-images", nil)
	req.Header.Set("Token", token)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload without bot: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var uploadResp struct {
		Result bool   `json:"result"`
		Err    string `json:"err"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &uploadResp); err != nil {
		t.Fatalf("decode upload: %v", err)
	}
	if uploadResp.Result || uploadResp.Err == "" {
		t.Fatalf("unexpected upload response: %+v", uploadResp)
	}
}
