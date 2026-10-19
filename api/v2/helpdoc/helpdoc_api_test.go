package helpdoc_test

import (
	"os"
	"path/filepath"
	"testing"

	. "sealdice-core/api/v2/helpdoc"
	"sealdice-core/dice"
	"sealdice-core/logger"
	"sealdice-core/model/common/request"
)

func newTestHelpDocService(t *testing.T) *Service {
	t.Helper()

	tempDir := t.TempDir()
	t.Chdir(tempDir)
	if err := os.MkdirAll(filepath.Join("data", "helpdoc"), 0o755); err != nil {
		t.Fatalf("mkdir helpdoc dir: %v", err)
	}

	d := &dice.Dice{
		Logger: logger.M(),
	}
	d.BaseConfig.Name = "helpdoc-test"
	d.BaseConfig.DataDir = tempDir
	d.Config = dice.NewConfig(d)
	dm := &dice.DiceManager{
		Dice: d,
		Help: &dice.HelpManager{
			HelpDocTree: []*dice.HelpDoc{
				{
					Key:   "file-1",
					Name:  "sample.json",
					Path:  "data/helpdoc/sample.json",
					Group: "default",
					Type:  ".json",
				},
			},
			Config: &dice.HelpConfig{Aliases: map[string][]string{
				"default": {"d"},
			}},
		},
	}
	d.Parent = dm
	return NewService(dm)
}

func TestGetTreeReturnsHelpDocTree(t *testing.T) {
	svc := newTestHelpDocService(t)

	resp, err := svc.GetTree(t.Context(), &request.Empty{})
	if err != nil {
		t.Fatalf("GetTree returned error: %v", err)
	}
	if len(resp.Body.Item.Data) != 1 {
		t.Fatalf("tree length = %d, want 1", len(resp.Body.Item.Data))
	}
	if resp.Body.Item.Data[0].Name != "sample.json" {
		t.Fatalf("tree first name = %q, want sample.json", resp.Body.Item.Data[0].Name)
	}
}

func TestSaveConfigPersistsAliases(t *testing.T) {
	svc := newTestHelpDocService(t)

	resp, err := svc.SetConfig(t.Context(), &ConfigReq{
		Body: HelpConfigBody{
			Aliases: map[string][]string{"default": {"d", "main"}},
		},
	})
	if err != nil {
		t.Fatalf("SetConfig returned error: %v", err)
	}
	if !resp.Body.Item.Success {
		t.Fatalf("SetConfig success = false, want true")
	}

	data, err := os.ReadFile(filepath.Join("data", "helpdoc", dice.HelpConfigFilename))
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	if string(data) == "" {
		t.Fatal("saved config is empty")
	}
}

// TestImageRenderConfigRoundTrip 覆盖 fork 新增的帮助文档图片化渲染配置：
// SetConfig 要落盘、GetConfig 要原样读回来，URL/令牌要去掉首尾空白。
func TestImageRenderConfigRoundTrip(t *testing.T) {
	svc := newTestHelpDocService(t)

	_, err := svc.SetConfig(t.Context(), &ConfigReq{
		Body: HelpConfigBody{
			Aliases:                map[string][]string{"default": {"d"}},
			ImageRenderEnable:      true,
			ImageRenderMinLength:   1200,
			ImageRenderMaskInImage: true,
			ImageRenderURL:         "  http://127.0.0.1:3212/render  ",
			ImageRenderToken:       "  secret-token  ",
			ImageRenderTimeoutSec:  20,
			ImageRenderWidth:       960,
		},
	})
	if err != nil {
		t.Fatalf("SetConfig returned error: %v", err)
	}

	resp, err := svc.GetConfig(t.Context(), &request.Empty{})
	if err != nil {
		t.Fatalf("GetConfig returned error: %v", err)
	}
	cfg := resp.Body.Item
	if !cfg.ImageRenderEnable {
		t.Error("imageRenderEnable = false, want true")
	}
	if cfg.ImageRenderMinLength != 1200 {
		t.Errorf("imageRenderMinLength = %d, want 1200", cfg.ImageRenderMinLength)
	}
	if !cfg.ImageRenderMaskInImage {
		t.Error("imageRenderMaskInImage = false, want true")
	}
	if cfg.ImageRenderURL != "http://127.0.0.1:3212/render" {
		t.Errorf("imageRenderUrl = %q, want trimmed URL", cfg.ImageRenderURL)
	}
	if cfg.ImageRenderToken != "secret-token" {
		t.Errorf("imageRenderToken = %q, want trimmed token", cfg.ImageRenderToken)
	}
	if cfg.ImageRenderTimeoutSec != 20 || cfg.ImageRenderWidth != 960 {
		t.Errorf("timeout/width = %d/%d, want 20/960", cfg.ImageRenderTimeoutSec, cfg.ImageRenderWidth)
	}
	if len(cfg.Aliases["default"]) != 1 || cfg.Aliases["default"][0] != "d" {
		t.Errorf("aliases = %#v, want default:[d]", cfg.Aliases)
	}
}
