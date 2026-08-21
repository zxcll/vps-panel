// Package dns 封装各家 DNS 服务商的记录读写。
//
// 三家的凭据格式和签名算法都不一样，但面板只关心两件事：
// 查当前解析值、把解析值改成某个 IP。Provider 接口就这两个方法。
//
// 这里没有引入任何厂商 SDK：Cloudflare 是普通 REST，
// 腾讯云和阿里云的签名算法各自 60 行左右，比拖进整个 SDK 划算。
package dns

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Record 是一条 DNS 记录。
type Record struct {
	// ID 是服务商侧的记录 ID。为空表示这条记录还不存在，需要新建。
	ID      string
	Zone    string // 主域名，如 example.com
	Name    string // 完整记录名，如 us.example.com
	Type    string // A / AAAA
	Content string // IP 地址
	TTL     int
	Proxied bool // 仅 Cloudflare 有意义
	Line    string
}

// Provider 是 DNS 服务商的统一接口。
type Provider interface {
	// Type 返回服务商类型标识。
	Type() string
	// List 查询匹配的记录。没有匹配时返回空切片而非错误。
	List(ctx context.Context, zone, name, rtype string) ([]Record, error)
	// Upsert 写入记录：rec.ID 非空则修改，否则新建。
	Upsert(ctx context.Context, rec Record) (Record, error)
}

// Credentials 是各家凭据的并集，以 JSON 形式加密存库。
// 前端按选中的服务商类型只展示对应的字段。
type Credentials struct {
	// Cloudflare
	APIToken string `json:"api_token,omitempty"`
	ZoneID   string `json:"zone_id,omitempty"` // 可选，填了就跳过按域名查 zone

	// 腾讯云 DNSPod
	SecretID  string `json:"secret_id,omitempty"`
	SecretKey string `json:"secret_key,omitempty"`

	// 阿里云 DNS
	AccessKeyID     string `json:"access_key_id,omitempty"`
	AccessKeySecret string `json:"access_key_secret,omitempty"`
	Region          string `json:"region,omitempty"`
}

// ParseCredentials 解析加密存储的凭据 JSON。
func ParseCredentials(raw string) (Credentials, error) {
	var c Credentials
	if strings.TrimSpace(raw) == "" {
		return c, fmt.Errorf("凭据为空")
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return c, fmt.Errorf("解析凭据 JSON: %w", err)
	}
	return c, nil
}

// New 按类型构造 Provider。
func New(typ string, cred Credentials) (Provider, error) {
	switch typ {
	case "cloudflare":
		return newCloudflare(cred)
	case "dnspod":
		return newDNSPod(cred)
	case "alidns":
		return newAlidns(cred)
	default:
		return nil, fmt.Errorf("不支持的 DNS 服务商类型 %q", typ)
	}
}

// ValidType 判断服务商类型是否受支持。
func ValidType(typ string) bool {
	switch typ {
	case "cloudflare", "dnspod", "alidns":
		return true
	}
	return false
}

// httpClient 是三家共用的 HTTP 客户端。超时设短一点——
// DNS 切换是故障恢复路径，卡住比失败更糟。
var httpClient = &http.Client{Timeout: 20 * time.Second}

// subDomain 从完整记录名里剥掉主域名，得到主机记录部分。
// 例如 (us.example.com, example.com) → "us"，(example.com, example.com) → "@"。
func subDomain(name, zone string) string {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	zone = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(zone)), ".")
	if name == zone || name == "" {
		return "@"
	}
	if strings.HasSuffix(name, "."+zone) {
		return strings.TrimSuffix(name, "."+zone)
	}
	// 记录名和主域名对不上时原样返回，让服务商侧报错，比静默猜错好。
	return name
}

// fqdn 把主机记录还原成完整域名。
func fqdn(sub, zone string) string {
	sub = strings.TrimSpace(sub)
	zone = strings.TrimSuffix(strings.TrimSpace(zone), ".")
	if sub == "" || sub == "@" {
		return zone
	}
	return sub + "." + zone
}
