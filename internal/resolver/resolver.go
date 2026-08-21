// Package resolver 负责把转发规则里的目标域名解析成 IP。
//
// 转发目标允许填域名（比如落地机挂了 DDNS），但 nftables 的 DNAT 只认 IP，
// 所以探针要定期重解析，解析结果变了才重新 apply 规则。
// 查询函数做成字段是为了测试能直接替换掉，不用真的发 DNS 请求。
package resolver

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"
)

var (
	ErrNoIPv4 = errors.New("该域名没有 A 记录")
	ErrNoIPv6 = errors.New("该域名没有 AAAA 记录")
)

// IsHostname 判断字符串看起来像域名而不是 IP 字面量。
// 空串和含非法字符的一律返回 false，让调用方尽早拒掉。
func IsHostname(s string) bool {
	if s == "" {
		return false
	}
	if net.ParseIP(s) != nil {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '.' || r == '_':
		default:
			return false
		}
	}
	return true
}

// PlausibleHostname 在 IsHostname 的基础上再排掉"最后一段全是数字"的情况。
// 数字 TLD 不可能存在，所以这种串必然是用户填错了——要么把端口当成地址填了
// （"4212"），要么 IP 打错了（"1.2.3.999"，因为超出 255 所以没被 ParseIP 认出来）。
// 提前报错比等到解析超时再报要好懂得多。
func PlausibleHostname(s string) bool {
	if !IsHostname(s) {
		return false
	}
	s = strings.TrimSuffix(s, ".")
	if s == "" {
		return false
	}
	labels := strings.Split(s, ".")
	last := labels[len(labels)-1]
	if last == "" {
		return false
	}
	for _, r := range last {
		if r < '0' || r > '9' {
			return true
		}
	}
	return false
}

// Resolver 包一层 net.LookupHost，方便测试替换。
type Resolver struct {
	Lookup  func(ctx context.Context, host string) ([]string, error)
	Timeout time.Duration
}

func New() *Resolver {
	return &Resolver{
		Lookup: func(ctx context.Context, host string) ([]string, error) {
			return net.DefaultResolver.LookupHost(ctx, host)
		},
		Timeout: 3 * time.Second,
	}
}

// LookupIPv4 返回第一个 A 记录。
func (r *Resolver) LookupIPv4(ctx context.Context, host string) (string, error) {
	return r.lookupFamily(ctx, host, false)
}

// LookupIPv6 返回第一个 AAAA 记录。
func (r *Resolver) LookupIPv6(ctx context.Context, host string) (string, error) {
	return r.lookupFamily(ctx, host, true)
}

func (r *Resolver) lookupFamily(ctx context.Context, host string, want6 bool) (string, error) {
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	addrs, err := r.Lookup(ctx, host)
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil {
			continue
		}
		is6 := ip.To4() == nil
		if is6 != want6 {
			continue
		}
		if want6 {
			return ip.String(), nil
		}
		return ip.To4().String(), nil
	}
	if want6 {
		return "", ErrNoIPv6
	}
	return "", ErrNoIPv4
}
