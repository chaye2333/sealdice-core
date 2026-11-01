package api //nolint:testpackage

import (
	"testing"

	"sealdice-core/dice"
)

// TestBanPickEndpoint 覆盖后台「拉黑」挑端点做 ctx 的规则：
// AddScoreBase 只在首次跨过阈值时用到 ctx（QQ 平台会顺手删好友），
// 所以官方 ID 要落到官方适配器、民间 ID 优先民间适配器，挑不到再退化。
func TestBanPickEndpoint(t *testing.T) {
	newDice := func(eps ...*dice.EndPointInfo) *dice.Dice {
		d := &dice.Dice{ImSession: &dice.IMSession{EndPoints: eps}}
		d.UIEndpoint = &dice.EndPointInfo{
			EndPointInfoBase: dice.EndPointInfoBase{ID: "ui", Platform: "UI", Enable: true},
		}
		return d
	}

	official := &dice.EndPointInfo{
		EndPointInfoBase: dice.EndPointInfoBase{ID: "official", Platform: "QQ", Enable: true},
		Adapter:          &dice.PlatformAdapterOfficialQQ{},
	}
	gocq := &dice.EndPointInfo{
		EndPointInfoBase: dice.EndPointInfoBase{ID: "gocq", Platform: "QQ", Enable: true},
		Adapter:          &dice.PlatformAdapterGocq{},
	}
	offOfficial := &dice.EndPointInfo{
		EndPointInfoBase: dice.EndPointInfoBase{ID: "official-off", Platform: "QQ", Enable: false},
		Adapter:          &dice.PlatformAdapterOfficialQQ{},
	}
	discord := &dice.EndPointInfo{
		EndPointInfoBase: dice.EndPointInfoBase{ID: "discord", Platform: "DISCORD", Enable: true},
	}

	t.Run("官方群优先官方适配器", func(t *testing.T) {
		// gocq 排在前面也不能选它，否则会去 delete_friend("海豹后台")
		d := newDice(gocq, official)
		if got := banPickEndpoint(d, "OpenQQ-Group:4019996761-74EF9046"); got != official {
			t.Fatalf("got %v, want official endpoint", got.ID)
		}
	})

	t.Run("民间群优先民间适配器", func(t *testing.T) {
		d := newDice(official, gocq)
		if got := banPickEndpoint(d, "QQ-Group:123456"); got != gocq {
			t.Fatalf("got %v, want gocq endpoint", got.ID)
		}
	})

	t.Run("官方端点禁用时退化为同平台端点", func(t *testing.T) {
		d := newDice(offOfficial, gocq)
		if got := banPickEndpoint(d, "OpenQQ-Group:1-abc"); got != gocq {
			t.Fatalf("got %v, want gocq fallback", got.ID)
		}
	})

	t.Run("平台对不上时退回 UI 端点", func(t *testing.T) {
		// 不能拿不相干的端点当 ctx：AddScoreBase 在 QQ 平台会顺手 delete_friend("海豹后台")
		d := newDice(discord)
		if got := banPickEndpoint(d, "QQ:1008611"); got != d.UIEndpoint {
			t.Fatalf("got %v, want UI endpoint", got.ID)
		}
	})

	t.Run("没有任何端点时退化为 UI 端点", func(t *testing.T) {
		d := newDice()
		if got := banPickEndpoint(d, "QQ:1008611"); got != d.UIEndpoint {
			t.Fatalf("got %v, want UI endpoint", got)
		}
	})

	t.Run("空指针不 panic", func(t *testing.T) {
		if got := banPickEndpoint(nil, "QQ:1"); got != nil {
			t.Fatalf("nil dice should return nil, got %v", got)
		}
		d := &dice.Dice{}
		if got := banPickEndpoint(d, "QQ:1"); got != nil {
			t.Fatalf("nil session should return nil, got %v", got)
		}
		// 列表里混入 nil 元素时也不能崩
		d2 := newDice(nil, official)
		if got := banPickEndpoint(d2, "OpenQQ-Group:1-abc"); got != official {
			t.Fatalf("nil endpoint entry should be skipped, got %v", got)
		}
	})
}
