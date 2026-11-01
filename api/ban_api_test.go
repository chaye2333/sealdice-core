package api //nolint:testpackage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"sealdice-core/dice"
	"sealdice-core/dice/service"
	"sealdice-core/model"
	"sealdice-core/utils/constant"
	"sealdice-core/utils/dboperator/engine/sqlite"
)

// banTestOperator 是最小的 DatabaseOperator，只挂一个临时 SQLite 文件。
type banTestOperator struct {
	db *gorm.DB
}

func newBanTestOperator(t *testing.T) *banTestOperator {
	t.Helper()
	db, err := sqlite.SQLiteDBInit(filepath.Join(t.TempDir(), "ban.db"), false)
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	// logger 太吵，测试里静音
	db.Logger = db.Logger.LogMode(logger.Silent)
	if err := db.AutoMigrate(&model.BanInfo{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	op := &banTestOperator{db: db}
	t.Cleanup(op.Close)
	return op
}

func (o *banTestOperator) Init(_ context.Context) error           { return nil }
func (o *banTestOperator) Type() string                           { return "test-sqlite" }
func (o *banTestOperator) DBCheck()                               {}
func (o *banTestOperator) GetDataDB(_ constant.DBMode) *gorm.DB   { return o.db }
func (o *banTestOperator) GetLogDB(_ constant.DBMode) *gorm.DB    { return o.db }
func (o *banTestOperator) GetCensorDB(_ constant.DBMode) *gorm.DB { return o.db }
func (o *banTestOperator) Close() {
	if sqlDB, err := o.db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

// newBanTestEnv 搭一套最小可用的 dice + 黑白名单路由。
// 端点故意只给一个 UI 端点：AddScoreBase 里只有 Platform == "QQ" 才会去删好友，
// 用 UI 端点可以避开外部调用。
func newBanTestEnv(t *testing.T) (*echo.Echo, *dice.Dice, string) {
	t.Helper()

	op := newBanTestOperator(t)
	manager := &dice.DiceManager{}
	token := "ban-api-test-token"
	manager.AccessTokens.Store(token, true)

	d := &dice.Dice{
		Parent:     manager,
		BaseConfig: dice.BaseConfig{DataDir: t.TempDir()},
		ImSession:  &dice.IMSession{},
		DBOperator: op,
	}
	d.Config.BanList = &dice.BanListInfo{Parent: d}
	d.Config.BanList.Init()
	uiEP := &dice.EndPointInfo{
		EndPointInfoBase: dice.EndPointInfoBase{ID: "ui", Platform: "UI", Enable: true},
	}
	d.UIEndpoint = uiEP
	d.ImSession.EndPoints = []*dice.EndPointInfo{uiEP}

	oldDice, oldDM := myDice, dm
	myDice, dm = d, manager
	t.Cleanup(func() { myDice, dm = oldDice, oldDM })

	e := echo.New()
	e.GET("/sd-api/banconfig/list", banMapList)
	e.POST("/sd-api/banconfig/map_add_one", banMapAddOne)
	e.POST("/sd-api/banconfig/map_delete_one", banMapDeleteOne)
	return e, d, token
}

func banDo(t *testing.T, e *echo.Echo, method, path, token, body string) (int, []byte) {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	if token != "" {
		req.Header.Set("Token", token)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// TestBanMapAddOnePersists 回归：后台拉黑必须立刻落库。
//
// 以前 banMapAddOne 只改内存 Map，而 BanList 的保存只在退出/导入时发生，
// 于是 /banconfig/list（读库）看不到刚拉黑的群，群组管理页"拉黑后重新拉名单复核"
// 会稳定误报成“名单没有变化”，进程被强杀时这条拉黑还会直接丢。
func TestBanMapAddOnePersists(t *testing.T) {
	e, d, token := newBanTestEnv(t)

	code, body := banDo(t, e, http.MethodPost, "/sd-api/banconfig/map_add_one", token,
		`{"ID":"QQ-Group:123456","rank":-30,"name":"测试群","reasons":["骰主后台设置"]}`)
	if code != http.StatusOK {
		t.Fatalf("map_add_one 状态码 = %d, body=%s", code, body)
	}

	var inDB bool
	if err := service.BanItemList(d.DBOperator, func(id string, _ int64, _ []byte) {
		if id == "QQ-Group:123456" {
			inDB = true
		}
	}); err != nil {
		t.Fatalf("读库失败: %v", err)
	}
	if !inDB {
		t.Fatal("拉黑后没有落库：/banconfig/list 会看不到这条，重启/强杀还会丢")
	}

	// 列表接口立刻可见，且等级、名字都在
	code, body = banDo(t, e, http.MethodGet, "/sd-api/banconfig/list", token, "")
	if code != http.StatusOK {
		t.Fatalf("list 状态码 = %d", code)
	}
	var lst []dice.BanListInfoItem
	if err := json.Unmarshal(body, &lst); err != nil {
		t.Fatalf("解析列表失败: %v (body=%s)", err, body)
	}
	var found *dice.BanListInfoItem
	for i := range lst {
		if lst[i].ID == "QQ-Group:123456" {
			found = &lst[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("列表里没有刚拉黑的群: %s", body)
	}
	if found.Rank != dice.BanRankBanned {
		t.Fatalf("rank = %d, want %d(禁止)", found.Rank, dice.BanRankBanned)
	}
	if found.Name != "测试群" {
		t.Fatalf("name = %q, want 测试群", found.Name)
	}
}

// TestBanMapListMergesMemoryOnly 列表要合并内存里的条目：
// .ban 指令与后台拉黑改的都是内存，没落库之前也应该能在后台看到。
func TestBanMapListMergesMemoryOnly(t *testing.T) {
	e, d, token := newBanTestEnv(t)

	// 模拟“只改了内存、还没落库”的条目
	d.Config.BanList.Map.Store("QQ-Group:777", &dice.BanListInfoItem{
		ID: "QQ-Group:777", Rank: dice.BanRankBanned, Name: "只存在内存里",
	})

	code, body := banDo(t, e, http.MethodGet, "/sd-api/banconfig/list", token, "")
	if code != http.StatusOK {
		t.Fatalf("list 状态码 = %d", code)
	}
	var lst []dice.BanListInfoItem
	if err := json.Unmarshal(body, &lst); err != nil {
		t.Fatalf("解析列表失败: %v (body=%s)", err, body)
	}
	for _, item := range lst {
		if item.ID == "QQ-Group:777" && item.Rank == dice.BanRankBanned {
			return
		}
	}
	t.Fatalf("内存里未落库的条目没有出现在列表中: %s", body)
}

func TestBanMergeLists(t *testing.T) {
	memory := new(dice.SyncMap[string, *dice.BanListInfoItem])
	memory.Store("QQ-Group:1", &dice.BanListInfoItem{ID: "QQ-Group:1", Rank: dice.BanRankBanned, Name: "内存版"})

	got := banMergeLists([]*dice.BanListInfoItem{
		{ID: "QQ-Group:1", Rank: dice.BanRankWarn, Name: "库里版"},
		{ID: "QQ:2", Rank: dice.BanRankBanned, Name: "库里的"},
		nil,
		{ID: ""},
	}, memory)

	if len(got) != 2 {
		t.Fatalf("合并后应该有 2 条，实际 %d: %+v", len(got), got)
	}
	if got[0].ID != "QQ-Group:1" || got[1].ID != "QQ:2" {
		t.Fatalf("结果应按 ID 排序，实际 %q, %q", got[0].ID, got[1].ID)
	}
	if got[0].Name != "内存版" || got[0].Rank != dice.BanRankBanned {
		t.Fatalf("同 ID 应以内存里的为准，实际 %+v", got[0])
	}

	// 没有内存表时也不能崩
	if lst := banMergeLists([]*dice.BanListInfoItem{{ID: "QQ:9"}}, nil); len(lst) != 1 {
		t.Fatalf("nil 内存表应只返回库里的条目，实际 %+v", lst)
	}
	if lst := banMergeLists(nil, nil); len(lst) != 0 {
		t.Fatalf("两边都空时应返回空列表，实际 %+v", lst)
	}
}

// TestBanMapRequiresAuth 回归：拉黑/解除拉黑必须带 token。
func TestBanMapRequiresAuth(t *testing.T) {
	e, d, _ := newBanTestEnv(t)

	addBody := `{"ID":"QQ-Group:999","rank":-30,"name":"没人权的群"}`
	if code, _ := banDo(t, e, http.MethodPost, "/sd-api/banconfig/map_add_one", "", addBody); code != http.StatusForbidden {
		t.Fatalf("无 token 拉黑应返回 403，实际 %d", code)
	}
	if code, _ := banDo(t, e, http.MethodPost, "/sd-api/banconfig/map_delete_one", "", `{"ID":"QQ-Group:999"}`); code != http.StatusForbidden {
		t.Fatalf("无 token 解除拉黑应返回 403，实际 %d", code)
	}
	if _, ok := d.Config.BanList.Map.Load("QQ-Group:999"); ok {
		t.Fatal("无 token 的请求不该改动黑白名单")
	}
	if err := service.BanItemList(d.DBOperator, func(id string, _ int64, _ []byte) {
		if id == "QQ-Group:999" {
			t.Fatal("无 token 的请求不该落库")
		}
	}); err != nil {
		t.Fatalf("读库失败: %v", err)
	}
}
