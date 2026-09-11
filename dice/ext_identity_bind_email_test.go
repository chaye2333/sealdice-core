//nolint:testpackage
package dice

import (
	"strings"
	"sync"
	"testing"
)

// 这一组补上邮箱通道的测试。此前邮箱功能完全没被覆盖（只过了编译），
// 所以像"投递失败却报成功"这种问题能溜过去。

// mailCapture 记录被"寄出"的邮件，替换真实的 SMTP 发送。
type mailCapture struct {
	mu   sync.Mutex
	sent []capturedMail
}

type capturedMail struct {
	subject string
	to      []string
	body    string
}

func (m *mailCapture) sender() func(*Dice, string, []string, string) {
	return func(_ *Dice, subject string, to []string, body string) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.sent = append(m.sent, capturedMail{subject: subject, to: to, body: body})
	}
}

func (m *mailCapture) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

func (m *mailCapture) last() (capturedMail, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sent) == 0 {
		return capturedMail{}, false
	}
	return m.sent[len(m.sent)-1], true
}

// withMailCapture 把邮件发送换成假的，并在用例结束后还原。
func withMailCapture(t *testing.T) *mailCapture {
	t.Helper()
	box := &mailCapture{}
	prev := identityBindMailSender
	identityBindMailSender = box.sender()
	t.Cleanup(func() { identityBindMailSender = prev })
	return box
}

// enableMailConfig 让 CanSendMail() 返回 true（配置齐全即可，不会真连 SMTP）。
func enableMailConfig(d *Dice) {
	d.Config.MailFrom = "bot@qq.com"
	d.Config.MailPassword = "authcode"
	d.Config.MailSMTP = "smtp.qq.com"
}

// TestIdentityBindSameIdentity 身份比对必须先全等、再退回纯 QQ 号比较。
//
// 回归测试：老实现只做「提取纯数字」比较，而官方 ID 形如
// OpenQQ:<UIN>-<MemberOpenID>（MemberOpenID 常是十六进制串），
// 提取结果是空串，于是**邮箱通道下发起者本人也确认不了自己的绑定**。
func TestIdentityBindSameIdentity(t *testing.T) {
	same := [][2]string{
		{"OpenQQ:100-member-openid", "OpenQQ:100-member-openid"},
		{"OpenQQ:4010000000-B1F55BD9471139B4427175CC638920CB", "OpenQQ:4010000000-B1F55BD9471139B4427175CC638920CB"},
		{" OpenQQ:100-member-openid ", "OpenQQ:100-member-openid"},
		{"QQ:12345", "12345"},
		{"QQ:12345", "QQ:12345"},
	}
	for _, c := range same {
		if !identityBindSameIdentity(c[0], c[1]) {
			t.Fatalf("identityBindSameIdentity(%q, %q) = false, want true", c[0], c[1])
		}
	}

	notSame := [][2]string{
		{"OpenQQ:100-member-a", "OpenQQ:100-member-b"},
		{"QQ:12345", "QQ:54321"},
		{"", "QQ:12345"},
		{"QQ:12345", ""},
	}
	for _, c := range notSame {
		if identityBindSameIdentity(c[0], c[1]) {
			t.Fatalf("identityBindSameIdentity(%q, %q) = true, want false", c[0], c[1])
		}
	}

	// 完全相同的非数字串属于同一个身份（走全等分支），这是有意的
	if !identityBindSameIdentity("abc", "abc") {
		t.Fatal("identical non-numeric ids should match by equality")
	}
}

// TestIdentityBindQQMailAddress 旧 QQ 号 → QQ 邮箱地址的推导。
func TestIdentityBindQQMailAddress(t *testing.T) {
	cases := []struct{ in, want string }{
		{"QQ:123456", "123456@qq.com"},
		{"123456", "123456@qq.com"},
		{" QQ:123456 ", "123456@qq.com"},
		{"", ""},
		{"abc", ""},          // 非数字，推不出
		{"OpenQQ:1-abc", ""}, // 官方 ID 不该被当 QQ 号
	}
	for _, c := range cases {
		if got := identityBindQQMailAddress(c.in); got != c.want {
			t.Fatalf("identityBindQQMailAddress(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestIdentityBindEmailCodeUsable 邮箱通道可用性**只看邮件配置**，没有额外开关。
//
// 这是刻意的设计：骰主既然把 SMTP 配好了就说明想用邮件，
// 再要求点一次开关纯属多余，还容易出现"配好了却一直提示没配"的困惑。
func TestIdentityBindEmailCodeUsable(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	d := env.d

	// 没配邮件 → 不可用
	d.Config.MailFrom = ""
	d.Config.MailPassword = ""
	d.Config.MailSMTP = ""
	if identityBindEmailCodeUsable(d) {
		t.Fatal("email channel must be unusable without any SMTP config")
	}

	// 只配一部分 → 仍不可用
	d.Config.MailFrom = "bot@qq.com"
	if identityBindEmailCodeUsable(d) {
		t.Fatal("email channel must require the full config (from/password/smtp)")
	}

	// 三项齐全 → 可用，且**不需要**任何额外开关
	enableMailConfig(d)
	if !identityBindEmailCodeUsable(d) {
		t.Fatal("email channel should be usable once SMTP config is complete")
	}

	// nil dice 不该 panic
	if identityBindEmailCodeUsable(nil) {
		t.Fatal("nil dice must not be usable")
	}
}

// TestIdentityBindDeliverViaEmail 民间 bot 发不出私聊时，验证码走邮箱。
func TestIdentityBindDeliverViaEmail(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	mailBox := withMailCapture(t)
	enableMailConfig(env.d)

	// 移除民间 bot 端点 → 私聊通道不可用
	env.d.ImSession.EndPoints = []*EndPointInfo{env.ctx.EndPoint}

	challenge := &identityBindCodeChallenge{
		Action:    identityBindActionUser,
		New:       identityBindEndpoint{UserID: bindTestNewUserID},
		Old:       identityBindEndpoint{UserID: bindTestOldUserID},
		Code:      "123456",
		DeliverTo: bindTestOldUserID,
		ConfirmBy: bindTestOldUserID,
		Status:    identityBindCodePending,
	}
	identityBindPutCode(challenge)
	identityBindDeliverPendingCodes(env.d)

	got, ok := identityBindLoadCode(identityBindActionUser, bindTestNewUserID)
	if !ok {
		t.Fatal("challenge disappeared")
	}
	if got.Status != identityBindCodeDelivered {
		t.Fatalf("status = %q, want delivered (reason=%q)", got.Status, got.Reason)
	}
	if got.Channel != identityBindCodeChannelEmail {
		t.Fatalf("channel = %q, want email", got.Channel)
	}
	if mailBox.count() != 1 {
		t.Fatalf("expected exactly 1 email, got %d", mailBox.count())
	}
	mail, _ := mailBox.last()
	if len(mail.to) != 1 || mail.to[0] != bindTestOldUserID[3:]+"@qq.com" {
		t.Fatalf("mail recipient = %v, want the QQ-number-derived address", mail.to)
	}
	if !strings.Contains(mail.body, "123456") {
		t.Fatalf("mail body should carry the code, got:\n%s", mail.body)
	}
}

// TestIdentityBindDeliveryPrefersDMByDefault 默认顺序：私聊优先，邮件不参与。
func TestIdentityBindDeliveryPrefersDMByDefault(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	mailBox := withMailCapture(t)
	env.d.Config.IdentityBindPreferEmailCode = false
	enableMailConfig(env.d)

	challenge := &identityBindCodeChallenge{
		Action:    identityBindActionUser,
		New:       identityBindEndpoint{UserID: bindTestNewUserID},
		Old:       identityBindEndpoint{UserID: bindTestOldUserID},
		Code:      "111111",
		DeliverTo: bindTestOldUserID,
		ConfirmBy: bindTestOldUserID,
		Status:    identityBindCodePending,
	}
	identityBindPutCode(challenge)
	identityBindDeliverPendingCodes(env.d)

	got, _ := identityBindLoadCode(identityBindActionUser, bindTestNewUserID)
	if got.Channel != identityBindCodeChannelDM {
		t.Fatalf("default should prefer DM, got channel=%q", got.Channel)
	}
	if mailBox.count() != 0 {
		t.Fatalf("email should not be used when DM works, but %d mail(s) were sent", mailBox.count())
	}
}

// TestIdentityBindDeliveryPrefersEmailWhenConfigured 打开优先邮箱后顺序反转。
func TestIdentityBindDeliveryPrefersEmailWhenConfigured(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	mailBox := withMailCapture(t)
	env.d.Config.IdentityBindPreferEmailCode = true
	enableMailConfig(env.d)

	challenge := &identityBindCodeChallenge{
		Action:    identityBindActionUser,
		New:       identityBindEndpoint{UserID: bindTestNewUserID},
		Old:       identityBindEndpoint{UserID: bindTestOldUserID},
		Code:      "222222",
		DeliverTo: bindTestOldUserID,
		ConfirmBy: bindTestOldUserID,
		Status:    identityBindCodePending,
	}
	identityBindPutCode(challenge)
	identityBindDeliverPendingCodes(env.d)

	got, _ := identityBindLoadCode(identityBindActionUser, bindTestNewUserID)
	if got.Channel != identityBindCodeChannelEmail {
		t.Fatalf("with prefer-email on, channel should be email, got %q (reason=%q)", got.Channel, got.Reason)
	}
	if mailBox.count() != 1 {
		t.Fatalf("expected 1 email, got %d", mailBox.count())
	}
}

// TestIdentityBindGroupNeverUsesEmail 群绑定永远走私聊——群号推不出邮箱。
func TestIdentityBindGroupNeverUsesEmail(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	mailBox := withMailCapture(t)
	env.d.Config.IdentityBindPreferEmailCode = true // 即使优先邮箱也不行
	enableMailConfig(env.d)

	challenge := &identityBindCodeChallenge{
		Action:    identityBindActionGroup,
		New:       identityBindEndpoint{GroupID: bindTestNewGroupID},
		Old:       identityBindEndpoint{GroupID: bindTestOldGroupID},
		Code:      "333333",
		DeliverTo: bindTestOldUserID,
		ConfirmBy: bindTestOldUserID,
		Status:    identityBindCodePending,
	}
	identityBindPutCode(challenge)
	identityBindDeliverPendingCodes(env.d)

	got, _ := identityBindLoadCode(identityBindActionGroup, bindTestNewGroupID)
	if got.Channel == identityBindCodeChannelEmail {
		t.Fatal("group binding must never use the email channel")
	}
	if mailBox.count() != 0 {
		t.Fatalf("no mail should be sent for group binding, got %d", mailBox.count())
	}
	if got.Status != identityBindCodeDelivered || got.Channel != identityBindCodeChannelDM {
		t.Fatalf("group challenge should be delivered via DM, got status=%q channel=%q reason=%q",
			got.Status, got.Channel, got.Reason)
	}
}

// TestIdentityBindEmailCodeConfirmByInitiator 邮箱码由官方侧发起者确认，
// 别人拿到码也没用。
func TestIdentityBindEmailCodeConfirmByInitiator(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	withMailCapture(t)

	challenge := &identityBindCodeChallenge{
		Action:    identityBindActionUser,
		New:       identityBindEndpoint{UserID: bindTestNewUserID},
		Old:       identityBindEndpoint{UserID: bindTestOldUserID},
		Code:      "444444",
		DeliverTo: bindTestOldUserID,
		ConfirmBy: bindTestOldUserID,
		Channel:   identityBindCodeChannelEmail,
		Status:    identityBindCodeDelivered,
		ExpiresAt: 1<<62 - 1,
	}
	identityBindPutCode(challenge)

	// 另一个人在官方群回复同一个码 → 必须被拒
	otherCtx, otherMsg := newQuitCommandTestContext(t, env.d, env.ctx.EndPoint, "OpenQQ:100-other", bindTestNewGroupID, "新群")
	otherMsg.MessageType = "group"
	if !identityBindTryConsumeCode(otherCtx, otherMsg, "444444") {
		t.Fatal("the reply should be consumed (it is code-shaped and matches an email challenge)")
	}
	if _, ok := identityBindStoreOf(env.d).find(env.d, bindTestOldUserID); ok {
		t.Fatal("a third party must not complete an email-channel binding")
	}

	// 发起者本人在官方群回复 → 通过
	initCtx, initMsg := newQuitCommandTestContext(t, env.d, env.ctx.EndPoint, bindTestNewUserID, bindTestNewGroupID, "新群")
	initMsg.MessageType = "group"
	if !identityBindTryConsumeCode(initCtx, initMsg, "444444") {
		t.Fatal("the initiator's reply should be consumed")
	}
	if _, ok := identityBindStoreOf(env.d).find(env.d, bindTestOldUserID); !ok {
		t.Fatal("the initiator should be able to complete the binding from the email code")
	}
}

// TestIdentityBindEmailCodeNotAcceptedOnOldBot 邮箱码不该在民间 bot 侧被认。
func TestIdentityBindEmailCodeNotAcceptedOnOldBot(t *testing.T) {
	env := newCodeTestEnv(t)
	defer env.cleanup()
	withMailCapture(t)

	challenge := &identityBindCodeChallenge{
		Action:    identityBindActionUser,
		New:       identityBindEndpoint{UserID: bindTestNewUserID},
		Old:       identityBindEndpoint{UserID: bindTestOldUserID},
		Code:      "555555",
		DeliverTo: bindTestOldUserID,
		ConfirmBy: bindTestOldUserID,
		Channel:   identityBindCodeChannelEmail,
		Status:    identityBindCodeDelivered,
		ExpiresAt: 1<<62 - 1,
	}
	identityBindPutCode(challenge)

	oldEP := codeOldEndPoint(t, env)
	oldCtx, oldMsg := newQuitCommandTestContext(t, env.d, oldEP, bindTestOldUserID, bindTestOldGroupID, "旧群")
	oldCtx.IsPrivate = true
	oldMsg.MessageType = "private"
	if identityBindTryConsumeCode(oldCtx, oldMsg, "555555") {
		t.Fatal("an email-channel code must not be accepted on the old-bot side")
	}
}
