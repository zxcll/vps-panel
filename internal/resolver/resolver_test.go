package resolver

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIsHostname(t *testing.T) {
	cases := map[string]bool{
		"example.com":     true,
		"hk.example.com":  true,
		"my_host.local":   true,
		"a-b.example.com": true,
		"":                false,
		"1.2.3.4":         false, // IP 字面量不是域名
		"::1":             false,
		"exam ple.com":    false,
		"域名.com":          false,
		"a/b.com":         false,
	}
	for in, want := range cases {
		if got := IsHostname(in); got != want {
			t.Errorf("IsHostname(%q) = %v，期望 %v", in, got, want)
		}
	}
}

func TestPlausibleHostname(t *testing.T) {
	cases := map[string]bool{
		"example.com":    true,
		"hk.example.com": true,
		"example.com.":   true, // 末尾的点是合法的完全限定写法

		// 下面这些是 IsHostname 放行、但绝不可能解析成功的手滑输入。
		// 数字 TLD 不存在，所以最后一段全是数字的一定是填错了。
		"4212":      false, // 把端口填进了地址栏
		"1.2.3.999": false, // IP 打错（超出 255，ParseIP 认不出来）
		"192.168.1": false,
		"":          false,
	}
	for in, want := range cases {
		if got := PlausibleHostname(in); got != want {
			t.Errorf("PlausibleHostname(%q) = %v，期望 %v", in, got, want)
		}
	}
}

func TestLookupIPv4PicksFirstARecord(t *testing.T) {
	r := &Resolver{
		Lookup: func(context.Context, string) ([]string, error) {
			// 双栈域名，混着返回。要 A 记录就必须跳过 AAAA。
			return []string{"2001:db8::1", "203.0.113.10", "203.0.113.11"}, nil
		},
		Timeout: time.Second,
	}
	got, err := r.LookupIPv4(context.Background(), "dual.example.com")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got != "203.0.113.10" {
		t.Errorf("LookupIPv4 = %q，期望 203.0.113.10", got)
	}
}

func TestLookupIPv6SkipsARecords(t *testing.T) {
	r := &Resolver{
		Lookup: func(context.Context, string) ([]string, error) {
			return []string{"203.0.113.10", "2001:db8::1"}, nil
		},
	}
	got, err := r.LookupIPv6(context.Background(), "dual.example.com")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got != "2001:db8::1" {
		t.Errorf("LookupIPv6 = %q，期望 2001:db8::1", got)
	}
}

func TestLookupIPv4OnIPv6OnlyHost(t *testing.T) {
	r := &Resolver{
		Lookup: func(context.Context, string) ([]string, error) {
			return []string{"2001:db8::1"}, nil
		},
	}
	if _, err := r.LookupIPv4(context.Background(), "v6only.example.com"); !errors.Is(err, ErrNoIPv4) {
		t.Errorf("只有 AAAA 记录时应返回 ErrNoIPv4，实际 %v", err)
	}
}

func TestLookupPropagatesError(t *testing.T) {
	boom := errors.New("SERVFAIL")
	r := &Resolver{
		Lookup: func(context.Context, string) ([]string, error) { return nil, boom },
	}
	if _, err := r.LookupIPv4(context.Background(), "bad.example.com"); !errors.Is(err, boom) {
		t.Errorf("解析错误应原样往上抛，实际 %v", err)
	}
}
