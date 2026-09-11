//nolint:testpackage
package dice

import "testing"

// TestParseMailSMTP 锁定 SMTP 地址解析与默认端口。
//
// 背景（真实踩到的坑）：原来 `gomail.NewDialer(addr, 25, ...)` 把端口硬编码成 25，
// 而国内绝大多数云服务器封锁 25 端口，于是骰主无论怎么配都只会得到
// `dial tcp <ip>:25: i/o timeout`。现在默认走 465(SSL)，并允许在地址里写端口。
func TestParseMailSMTP(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantPort int
	}{
		// 只写域名：用默认的 465
		{"smtp.qq.com", "smtp.qq.com", mailDefaultSMTPPort},
		{" smtp.qq.com ", "smtp.qq.com", mailDefaultSMTPPort},
		{"", "", mailDefaultSMTPPort},
		// 写了端口就听骰主的
		{"smtp.qq.com:587", "smtp.qq.com", 587},
		{"smtp.qq.com:465", "smtp.qq.com", 465},
		{"smtp.qq.com:2525", "smtp.qq.com", 2525},
		// IPv6 字面量
		{"[::1]:465", "::1", 465},
		// 端口非法时退回默认，并且不 panic
		{"smtp.qq.com:abc", "smtp.qq.com:abc", mailDefaultSMTPPort},
		{"smtp.qq.com:0", "smtp.qq.com:0", mailDefaultSMTPPort},
		{"smtp.qq.com:99999", "smtp.qq.com:99999", mailDefaultSMTPPort},
	}
	for _, c := range cases {
		host, port := parseMailSMTP(c.in)
		if host != c.wantHost || port != c.wantPort {
			t.Fatalf("parseMailSMTP(%q) = (%q, %d), want (%q, %d)",
				c.in, host, port, c.wantHost, c.wantPort)
		}
	}

	// 默认端口绝不能是 25 —— 那正是被封的那个
	if mailDefaultSMTPPort == 25 {
		t.Fatal("default SMTP port must not be 25 (blocked by most cloud providers)")
	}
}

// TestNewMailDialerPicksSSLFor465 465 必须开隐式 SSL，否则握手失败；其它端口不开。
func TestNewMailDialerPicksSSLFor465(t *testing.T) {
	d465 := newMailDialer("smtp.qq.com:465", "a@qq.com", "pw")
	if !d465.SSL {
		t.Fatal("port 465 must enable implicit SSL")
	}
	if d465.Host != "smtp.qq.com" || d465.Port != 465 {
		t.Fatalf("dialer = %s:%d, want smtp.qq.com:465", d465.Host, d465.Port)
	}

	// 不写端口时默认就是 465，所以也应该开 SSL
	dDefault := newMailDialer("smtp.qq.com", "a@qq.com", "pw")
	if !dDefault.SSL || dDefault.Port != 465 {
		t.Fatalf("default dialer should be 465+SSL, got port=%d ssl=%v", dDefault.Port, dDefault.SSL)
	}

	// 587 交给 gomail 自动 STARTTLS，不该开隐式 SSL
	d587 := newMailDialer("smtp.qq.com:587", "a@qq.com", "pw")
	if d587.SSL {
		t.Fatal("port 587 must not use implicit SSL (it uses STARTTLS)")
	}
	if d587.Port != 587 {
		t.Fatalf("dialer port = %d, want 587", d587.Port)
	}
}
