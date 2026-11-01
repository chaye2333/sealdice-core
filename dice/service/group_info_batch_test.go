package service

import (
	"context"
	"path/filepath"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"sealdice-core/model"
	"sealdice-core/utils/constant"
	"sealdice-core/utils/dboperator/engine/sqlite"
)

// batchTestOperator 是最小的 DatabaseOperator，只挂一个临时 SQLite 文件。
type batchTestOperator struct {
	db *gorm.DB
}

func newBatchTestOperator(t *testing.T) *batchTestOperator {
	t.Helper()
	db, err := sqlite.SQLiteDBInit(filepath.Join(t.TempDir(), "group-player.db"), false)
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	db.Logger = db.Logger.LogMode(logger.Silent)
	if err := db.AutoMigrate(&model.GroupPlayerInfoBase{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	op := &batchTestOperator{db: db}
	t.Cleanup(op.Close)
	return op
}

func (o *batchTestOperator) Init(_ context.Context) error           { return nil }
func (o *batchTestOperator) Type() string                           { return "test-sqlite" }
func (o *batchTestOperator) DBCheck()                               {}
func (o *batchTestOperator) GetDataDB(_ constant.DBMode) *gorm.DB   { return o.db }
func (o *batchTestOperator) GetLogDB(_ constant.DBMode) *gorm.DB    { return o.db }
func (o *batchTestOperator) GetCensorDB(_ constant.DBMode) *gorm.DB { return o.db }
func (o *batchTestOperator) Close() {
	if sqlDB, err := o.db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

// TestGroupPlayerNumGetBatch 回归群组管理页的 N+1：
// 批量版本要和逐个 GroupPlayerNumGet 给出一样的数字，且没玩家的群不出现在结果里。
func TestGroupPlayerNumGetBatch(t *testing.T) {
	op := newBatchTestOperator(t)
	db := op.GetDataDB(constant.WRITE)

	seed := []model.GroupPlayerInfoBase{
		{GroupID: "QQ-Group:1", UserID: "QQ:1"},
		{GroupID: "QQ-Group:1", UserID: "QQ:2"},
		{GroupID: "QQ-Group:1", UserID: "QQ:3"},
		{GroupID: "QQ-Group:2", UserID: "QQ:1"},
		{GroupID: "QQ-Group:4", UserID: "QQ:9"},
	}
	for i := range seed {
		if err := db.Create(&seed[i]).Error; err != nil {
			t.Fatalf("插入玩家失败: %v", err)
		}
	}

	got, err := GroupPlayerNumGetBatch(op, []string{"QQ-Group:1", "QQ-Group:2", "QQ-Group:3", "QQ-Group:4"})
	if err != nil {
		t.Fatalf("批量查询失败: %v", err)
	}
	want := map[string]int64{"QQ-Group:1": 3, "QQ-Group:2": 1, "QQ-Group:4": 1}
	if len(got) != len(want) {
		t.Fatalf("结果条数 = %d, want %d: %+v", len(got), len(want), got)
	}
	for id, wantNum := range want {
		if got[id] != wantNum {
			t.Fatalf("%s 人数 = %d, want %d", id, got[id], wantNum)
		}
		// 和逐个查询的口径对齐
		single, err := GroupPlayerNumGet(op, id)
		if err != nil {
			t.Fatalf("单个查询失败: %v", err)
		}
		if single != got[id] {
			t.Fatalf("%s 批量=%d 单个=%d，两者必须一致", id, got[id], single)
		}
	}

	// 只查一个群时也要正确
	one, err := GroupPlayerNumGetBatch(op, []string{"QQ-Group:2"})
	if err != nil {
		t.Fatalf("单群批量查询失败: %v", err)
	}
	if one["QQ-Group:2"] != 1 || len(one) != 1 {
		t.Fatalf("单群结果异常: %+v", one)
	}

	// 空列表不该产生查询（传 nil operator 也不该崩）
	empty, err := GroupPlayerNumGetBatch(nil, nil)
	if err != nil {
		t.Fatalf("空列表不该报错: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("空列表应返回空 map，实际 %+v", empty)
	}
}
