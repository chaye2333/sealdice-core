//nolint:testpackage
package dice

import (
	"strings"
	"testing"

	ds "github.com/sealdice/dicescript"
)

// bindCardForTest 给"当前玩家"新建一张角色卡并绑定为当前卡（等价于 .pc tag）。
func bindCardForTest(t *testing.T, ctx *MsgContext, name string) {
	t.Helper()
	am := ctx.Dice.AttrsManager
	if am == nil {
		t.Fatal("AttrsManager is nil")
	}
	card, err := am.CharNew(ctx.Player.UserID, name, "coc7")
	if err != nil {
		t.Fatalf("CharNew(%q): %v", name, err)
	}
	if err := am.CharBind(card.Id, ctx.Group.GroupID, ctx.Player.UserID); err != nil {
		t.Fatalf("CharBind(%q): %v", name, err)
	}
}

// TestOfficialQQStatusBarNameFollowsBoundCard 状态栏里的名字必须跟着"当前角色卡"走。
//
// 玩家实测的回归：官方 bot **改不了群名片**（SetGroupCardName 是空实现），
// 而旧实现优先取"绑定另一侧 / ctx.Player.Name"，于是 .pc tag 换卡之后
// 状态栏里的名字还停在上一次（或旧群那一侧）的名字上，看起来像换卡不生效。
func TestOfficialQQStatusBarNameFollowsBoundCard(t *testing.T) {
	ctx, cleanup := newOfficialQQBarTestCtx(t, "coc7", map[string]*ds.VMValue{"血条": intVal(12)})
	defer cleanup()
	ctx.Player.AutoSetNameTemplate = "{$t玩家_RAW} HP{血条}"

	// 绑定第一张卡
	bindCardForTest(t, ctx, "茶叶")
	bar := officialQQCharacterStatusBar(ctx)
	if !strings.Contains(bar, "茶叶") {
		t.Fatalf("the bar should show the bound card name, got %q", bar)
	}

	// 换成第二张卡 → 状态栏必须跟着变（这就是用户报的那条）
	bindCardForTest(t, ctx, "马丁·弗卢吉尔")
	bar = officialQQCharacterStatusBar(ctx)
	if !strings.Contains(bar, "马丁·弗卢吉尔") {
		t.Fatalf("after switching the card the bar should show the new card name, got %q", bar)
	}
	if strings.Contains(bar, "茶叶") {
		t.Fatalf("the bar must not keep the previous name, got %q", bar)
	}
}

// TestOfficialQQStatusBarNamePrefersCardOverPlayerName 名字与属性来自同一张卡。
func TestOfficialQQStatusBarNamePrefersCardOverPlayerName(t *testing.T) {
	ctx, cleanup := newOfficialQQBarTestCtx(t, "coc7", map[string]*ds.VMValue{"血条": intVal(12)})
	defer cleanup()
	ctx.Player.AutoSetNameTemplate = "{$t玩家_RAW} HP{血条}"
	ctx.Player.Name = "群名片上的旧名字"

	bindCardForTest(t, ctx, "马丁·弗卢吉尔")

	if got := officialQQStatusBarPlayerName(ctx); got != "马丁·弗卢吉尔" {
		t.Fatalf("status bar name = %q, want the bound card name", got)
	}
}

// TestOfficialQQStatusBarNameFallsBackWithoutCard 卡上没名字时退回玩家名。
func TestOfficialQQStatusBarNameFallsBackWithoutCard(t *testing.T) {
	ctx, cleanup := newOfficialQQBarTestCtx(t, "coc7", map[string]*ds.VMValue{"血条": intVal(12)})
	defer cleanup()
	ctx.Player.AutoSetNameTemplate = "{$t玩家_RAW} HP{血条}"
	ctx.Player.Name = "群里的昵称"

	// 真实世界里"默认卡"是懒创建的、没有名字（dice_attrs_manager.go 的 LoadById），
	// 这里把测试夹具建的那张默认卡也清成无名，模拟真实情况。
	item, err := ctx.Dice.AttrsManager.LoadByCtx(ctx)
	if err != nil || item == nil {
		t.Fatalf("setup: load default card: %v", err)
	}
	item.Name = ""

	if got := officialQQStatusBarPlayerName(ctx); got != "群里的昵称" {
		t.Fatalf("status bar name = %q, want the fallback player name", got)
	}
}

// TestOfficialQQStatusBarNameFollowsCardOnBoundIdentity
// 做过身份绑定时同理：卡是数据层共用的那一份，两侧看到同一个名字。
func TestOfficialQQStatusBarNameFollowsCardOnBoundIdentity(t *testing.T) {
	ctx, cleanup := newOfficialQQBarTestCtx(t, "coc7", map[string]*ds.VMValue{"血条": intVal(12)})
	defer cleanup()
	ctx.Player.AutoSetNameTemplate = "{$t玩家_RAW} HP{血条}"

	// 官方身份 ↔ 旧号 的个人绑定
	record := &identityBindRecord{
		Action: identityBindActionUser,
		New: identityBindEndpoint{
			GroupID: ctx.Group.GroupID, UserID: ctx.Player.UserID,
		},
		Old: identityBindEndpoint{UserID: "QQ:1659163858"},
	}
	if err := identityBindStoreOf(ctx.Dice).put(ctx.Dice, record); err != nil {
		t.Fatalf("put binding: %v", err)
	}

	// 数据层身份已经变成旧号：卡要按数据层身份来绑
	dataUserID := identityBindDataUserID(ctx)
	if dataUserID != "QQ:1659163858" {
		t.Fatalf("setup: data user id = %q, want the old qq id", dataUserID)
	}
	card, err := ctx.Dice.AttrsManager.CharNew(dataUserID, "马丁·弗卢吉尔", "coc7")
	if err != nil {
		t.Fatalf("CharNew: %v", err)
	}
	if err := ctx.Dice.AttrsManager.CharBind(card.Id, identityBindDataGroupID(ctx), dataUserID); err != nil {
		t.Fatalf("CharBind: %v", err)
	}

	if got := officialQQStatusBarPlayerName(ctx); got != "马丁·弗卢吉尔" {
		t.Fatalf("status bar name = %q, want the shared card name", got)
	}
}
