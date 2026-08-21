package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

const cfAPIBase = "https://api.cloudflare.com/client/v4"

type cloudflare struct {
	token string
	// base 是 API 根地址，单测里指向 httptest 服务器。
	base string

	mu        sync.Mutex
	zoneCache map[string]string // zone 名 → zone ID
}

func newCloudflare(c Credentials) (Provider, error) {
	if c.APIToken == "" {
		return nil, fmt.Errorf("Cloudflare 需要填写 API Token")
	}
	cf := &cloudflare{token: c.APIToken, base: cfAPIBase, zoneCache: map[string]string{}}
	if c.ZoneID != "" {
		// 用户直接给了 Zone ID 就不用再查了（也适用于 Token 权限只到单个 zone 的情况）
		cf.zoneCache["*"] = c.ZoneID
	}
	return cf, nil
}

func (c *cloudflare) Type() string { return "cloudflare" }

// cfResponse 是 Cloudflare 的统一响应信封。
type cfResponse struct {
	Success bool            `json:"success"`
	Errors  []cfError       `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e cfError) String() string { return fmt.Sprintf("%d: %s", e.Code, e.Message) }

func (c *cloudflare) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("请求 Cloudflare API: %w", err)
	}
	defer resp.Body.Close()

	var env cfResponse
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("解析 Cloudflare 响应(HTTP %d): %w", resp.StatusCode, err)
	}
	if !env.Success {
		msgs := make([]string, 0, len(env.Errors))
		for _, e := range env.Errors {
			msgs = append(msgs, e.String())
		}
		if len(msgs) == 0 {
			msgs = append(msgs, fmt.Sprintf("HTTP %d", resp.StatusCode))
		}
		return fmt.Errorf("Cloudflare API 返回错误: %s", strings.Join(msgs, "; "))
	}
	if out != nil && len(env.Result) > 0 {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// zoneID 把主域名解析成 Zone ID，结果缓存在内存里。
func (c *cloudflare) zoneID(ctx context.Context, zone string) (string, error) {
	zone = strings.TrimSuffix(strings.ToLower(zone), ".")

	c.mu.Lock()
	if id, ok := c.zoneCache["*"]; ok {
		c.mu.Unlock()
		return id, nil
	}
	if id, ok := c.zoneCache[zone]; ok {
		c.mu.Unlock()
		return id, nil
	}
	c.mu.Unlock()

	var zones []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := c.do(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(zone), nil, &zones); err != nil {
		return "", err
	}
	if len(zones) == 0 {
		return "", fmt.Errorf("在该 Cloudflare 账号下找不到域名 %s（检查 Token 权限是否覆盖这个 zone）", zone)
	}

	c.mu.Lock()
	c.zoneCache[zone] = zones[0].ID
	c.mu.Unlock()
	return zones[0].ID, nil
}

type cfRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

func (c *cloudflare) List(ctx context.Context, zone, name, rtype string) ([]Record, error) {
	zid, err := c.zoneID(ctx, zone)
	if err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("name", strings.TrimSuffix(strings.ToLower(name), "."))
	if rtype != "" {
		q.Set("type", rtype)
	}

	var recs []cfRecord
	if err := c.do(ctx, http.MethodGet, "/zones/"+zid+"/dns_records?"+q.Encode(), nil, &recs); err != nil {
		return nil, err
	}

	out := make([]Record, 0, len(recs))
	for _, r := range recs {
		out = append(out, Record{
			ID: r.ID, Zone: zone, Name: r.Name, Type: r.Type,
			Content: r.Content, TTL: r.TTL, Proxied: r.Proxied,
		})
	}
	return out, nil
}

func (c *cloudflare) Upsert(ctx context.Context, rec Record) (Record, error) {
	zid, err := c.zoneID(ctx, rec.Zone)
	if err != nil {
		return rec, err
	}

	// Cloudflare 开了代理(proxied)时 TTL 必须是 1（自动）
	ttl := rec.TTL
	if rec.Proxied || ttl <= 0 {
		ttl = 1
	}

	body := cfRecord{
		Type:    rec.Type,
		Name:    strings.TrimSuffix(strings.ToLower(rec.Name), "."),
		Content: rec.Content,
		TTL:     ttl,
		Proxied: rec.Proxied,
	}

	var res cfRecord
	if rec.ID != "" {
		err = c.do(ctx, http.MethodPut, "/zones/"+zid+"/dns_records/"+rec.ID, body, &res)
	} else {
		err = c.do(ctx, http.MethodPost, "/zones/"+zid+"/dns_records", body, &res)
	}
	if err != nil {
		return rec, err
	}

	rec.ID = res.ID
	rec.TTL = res.TTL
	return rec, nil
}
