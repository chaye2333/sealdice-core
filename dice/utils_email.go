package dice

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"gopkg.in/gomail.v2"
)

type MailCode int

const (
	// MailTypeConnectClose 掉线
	MailTypeConnectClose MailCode = iota
	// MailTypeCIAMLock 风控 // tx 云把 Customer Identity Access Management 叫做 账号风控平台……
	MailTypeCIAMLock
	// MailTypeNotice 通知
	MailTypeNotice
	// MailTypeSendNote send 指令
	MailTypeSendNote
	// MailTest 测试邮件
	MailTest
)

func (d *Dice) CanSendMail() bool {
	if d.Config.MailFrom == "" || d.Config.MailPassword == "" || d.Config.MailSMTP == "" {
		return false
	}
	return true
}

// SendMail 向允许接收该分类的通知目标发送邮件。
func (d *Dice) SendMail(body string, m MailCode, noticeTypes ...NoticeType) error {
	if !d.CanSendMail() {
		return errors.New("邮件配置不完整")
	}
	noticeType := NoticeTypeSystem
	if len(noticeTypes) > 0 {
		noticeType = noticeTypes[0]
	}
	sub := "Seal News: "
	switch m {
	case MailTypeConnectClose:
		sub += "Connect 连接中断"
	case MailTypeCIAMLock:
		sub += "Bot 机器人被风控"
	case MailTypeNotice:
		sub += "Event 事件通知"
	case MailTypeSendNote:
		sub += "Send 指令反馈"
	case MailTest:
		sub += "Test 测试邮件"
	}
	var to []string
	for _, target := range filterNoticeTargets(d.Config.NoticeIDs, noticeType) {
		if strings.HasPrefix(target.ID, "QQ:") {
			to = append(to, target.ID[3:]+"@qq.com")
		}
		if strings.HasPrefix(target.ID, "Mail:") {
			to = append(to, target.ID[5:])
		}
	}
	if len(to) == 0 {
		return errors.New("没有启用且允许接收此类通知的邮件目标")
	}

	return d.SendMailRow(sub, to, body, nil)
}

// SMTP 默认端口与加密方式。
//
// 为什么不能继续硬编码 25：国内绝大多数云服务器（阿里云/腾讯云等）**封锁 25 端口**，
// 于是无论骰主怎么配，都只会拿到 `dial tcp <ip>:25: i/o timeout`。
// 各大邮箱都支持加密端口，所以默认改用 465(SSL)——QQ 邮箱 / 163 / Gmail 都支持，
// 而且不会被云厂商拦。
const (
	mailDefaultSMTPPort = 465
	// mailSMTPImplicitSSLPort 用隐式 SSL 的端口（其余走 STARTTLS）。
	mailSMTPImplicitSSLPort = 465
)

// parseMailSMTP 解析 SMTP 地址，允许两种写法：
//
//	smtp.qq.com       -> host=smtp.qq.com, port=465（默认）
//	smtp.qq.com:587   -> host=smtp.qq.com, port=587
//	[::1]:465         -> IPv6 字面量也支持
//
// 骰主填了端口就用骰主填的；没填就用 465。
func parseMailSMTP(raw string) (host string, port int) {
	host = strings.TrimSpace(raw)
	port = mailDefaultSMTPPort
	if host == "" {
		return host, port
	}
	if h, p, err := net.SplitHostPort(host); err == nil {
		if n, errConv := strconv.Atoi(p); errConv == nil && n > 0 && n <= 65535 {
			return h, n
		}
		return host, port
	}
	return host, port
}

// newMailDialer 按端口选择加密方式构造 dialer。
//
// 这个版本的 gomail 没有 StartTLSPolicy 字段：它在非 SSL 分支里会自己检查
// 服务器是否支持 STARTTLS 并自动升级（opportunistic）。所以：
//   - 465  → 隐式 SSL（必须显式打开，否则会拿明文去连，握手失败）
//   - 其它 → 交给 gomail 自动 STARTTLS
func newMailDialer(smtpAddr, from, password string) *gomail.Dialer {
	host, port := parseMailSMTP(smtpAddr)
	d := gomail.NewDialer(host, port, from, password)
	if port == mailSMTPImplicitSSLPort {
		d.SSL = true
	}
	return d
}

// SendMailRow 发送一封邮件，并把 SMTP 的错误如实返回。
//
// 为什么必须返回 error：身份绑定的邮箱通道要靠它判断"验证码到底寄出去没有"。
// 老实现把 DialAndSend 的错误只写进日志、对外永远成功，
// 于是 SMTP 配错了也会告诉用户"已寄出，请查收"，用户只能干等。
func (d *Dice) SendMailRow(subject string, to []string, content string, attachments []string) error {
	m := gomail.NewMessage()
	// NOTE(Xiangze Li): 按理说应当统一用DiceFotmatTmpl, 但是那样还得有一个MsgContext, 好复杂
	diceName := "海豹核心"
	if v := d.TextMap["核心:骰子名字"]; v != nil {
		if s := pickChooserWithSource(v, globalRandSource); s != "" {
			diceName = s
		}
	}
	m.SetHeader("Subject", fmt.Sprintf("[%s] %s", diceName, subject))
	m.SetHeader("From", d.Config.MailFrom)
	m.SetHeader("To", to...)
	if content == "" {
		m.SetBody("text/plain", "***自动邮件，无需回复***")
	} else {
		m.SetBody("text/plain", content+"\n\n***自动邮件，无需回复***")
	}
	if len(attachments) > 0 {
		for _, attachment := range attachments {
			m.Attach(attachment)
		}
	}

	dialer := newMailDialer(d.Config.MailSMTP, d.Config.MailFrom, d.Config.MailPassword)
	if err := dialer.DialAndSend(m); err != nil {
		d.Logger.Error(err)
		return err
	}
	d.Logger.Infof("Mail:[%s]%s -> %s", subject, content, strings.Join(to, ";"))
	return nil
}
