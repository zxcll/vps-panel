package dns

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 阿里云 DNS 用的是经典 RPC 风格接口（2015-01-09 版本），
// 签名是 HMAC-SHA1 + 特定的百分号编码规则。同样手写，不引 SDK。
const (
	aliEndpoint = "https://alidns.aliyuncs.com/"
	aliVersion  = "2015-01-09"
)

type alidns struct {
	keyID  string
	secret string
	// endpoint 是接口地址，单测里指向 httptest 服务器。
	endpoint string
}

func newAlidns(c Credentials) (Provider, error) {
	if c.AccessKeyID == "" || c.AccessKeySecret == "" {
		return nil, fmt.Errorf("阿里云 DNS 需要填写 AccessKeyId 和 AccessKeySecret")
	}
	return &alidns{keyID: c.AccessKeyID, secret: c.AccessKeySecret, endpoint: aliEndpoint}, nil
}

func (a *alidns) Type() string { return "alidns" }

// aliEncode 实现阿里云要求的百分号编码：
// 在标准 URL 编码基础上，把 + 换成 %20、* 换成 %2A、%7E 还原成 ~。
func aliEncode(s string) string {
	e := url.QueryEscape(s)
	e = strings.ReplaceAll(e, "+", "%20")
	e = strings.ReplaceAll(e, "*", "%2A")
	e = strings.ReplaceAll(e, "%7E", "~")
	return e
}

func nonce() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// 随机源不可用时退化成时间戳，签名仍然合法，只是防重放弱一些
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(b)
}

// sign 生成签名并返回完整的查询串。
func (a *alidns) sign(params map[string]string) string {
	params["Format"] = "JSON"
	params["Version"] = aliVersion
	params["AccessKeyId"] = a.keyID
	params["SignatureMethod"] = "HMAC-SHA1"
	params["SignatureVersion"] = "1.0"
	params["SignatureNonce"] = nonce()
	params["Timestamp"] = time.Now().UTC().Format("2006-01-02T15:04:05Z")

	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var canonical strings.Builder
	for i, k := range keys {
		if i > 0 {
			canonical.WriteByte('&')
		}
		canonical.WriteString(aliEncode(k))
		canonical.WriteByte('=')
		canonical.WriteString(aliEncode(params[k]))
	}

	stringToSign := http.MethodGet + "&" + aliEncode("/") + "&" + aliEncode(canonical.String())

	mac := hmac.New(sha1.New, []byte(a.secret+"&"))
	mac.Write([]byte(stringToSign))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	return "Signature=" + aliEncode(signature) + "&" + canonical.String()
}

func (a *alidns) call(ctx context.Context, action string, params map[string]string, out any) error {
	if params == nil {
		params = map[string]string{}
	}
	params["Action"] = action

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.endpoint+"?"+a.sign(params), nil)
	if err != nil {
		return err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("请求阿里云 DNS API: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("读取阿里云响应: %w", err)
	}
	body := string(raw)

	// 阿里云用 HTTP 状态码 + Code/Message 字段表达错误
	if resp.StatusCode >= 400 {
		var e struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		}
		if json.Unmarshal([]byte(body), &e) == nil && e.Code != "" {
			return fmt.Errorf("阿里云 DNS 返回错误 %s: %s", e.Code, e.Message)
		}
		return fmt.Errorf("阿里云 DNS 返回 HTTP %d: %s", resp.StatusCode, truncate(body, 300))
	}

	if out != nil {
		if err := json.Unmarshal([]byte(body), out); err != nil {
			return fmt.Errorf("解析阿里云响应: %w", err)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func (a *alidns) List(ctx context.Context, zone, name, rtype string) ([]Record, error) {
	params := map[string]string{
		"SubDomain":  fqdn(subDomain(name, zone), zone),
		"PageSize":   "100",
		"PageNumber": "1",
	}
	if rtype != "" {
		params["Type"] = rtype
	}

	var res struct {
		DomainRecords struct {
			Record []struct {
				RecordId   string `json:"RecordId"`
				DomainName string `json:"DomainName"`
				RR         string `json:"RR"`
				Type       string `json:"Type"`
				Value      string `json:"Value"`
				TTL        int    `json:"TTL"`
				Line       string `json:"Line"`
			} `json:"Record"`
		} `json:"DomainRecords"`
	}

	if err := a.call(ctx, "DescribeSubDomainRecords", params, &res); err != nil {
		// 子域名还没有任何记录时阿里云也报错，视作空结果
		if strings.Contains(err.Error(), "InvalidDomainName.NoExist") ||
			strings.Contains(err.Error(), "DomainRecordNotBelongToUser") {
			return []Record{}, nil
		}
		return nil, err
	}

	out := make([]Record, 0, len(res.DomainRecords.Record))
	for _, r := range res.DomainRecords.Record {
		out = append(out, Record{
			ID:      r.RecordId,
			Zone:    zone,
			Name:    fqdn(r.RR, r.DomainName),
			Type:    r.Type,
			Content: r.Value,
			TTL:     r.TTL,
			Line:    r.Line,
		})
	}
	return out, nil
}

func (a *alidns) Upsert(ctx context.Context, rec Record) (Record, error) {
	ttl := rec.TTL
	if ttl <= 0 {
		ttl = 600
	}
	rr := subDomain(rec.Name, rec.Zone)

	params := map[string]string{
		"RR":    rr,
		"Type":  rec.Type,
		"Value": rec.Content,
		"TTL":   strconv.Itoa(ttl),
	}

	if rec.ID != "" {
		params["RecordId"] = rec.ID
		if err := a.call(ctx, "UpdateDomainRecord", params, nil); err != nil {
			// 新值和旧值完全相同时阿里云会报这个错，对我们来说是"已达成目标"
			if strings.Contains(err.Error(), "DomainRecordDuplicate") {
				return rec, nil
			}
			return rec, err
		}
		return rec, nil
	}

	params["DomainName"] = strings.TrimSuffix(rec.Zone, ".")
	var res struct {
		RecordId string `json:"RecordId"`
	}
	if err := a.call(ctx, "AddDomainRecord", params, &res); err != nil {
		return rec, err
	}
	rec.ID = res.RecordId
	return rec, nil
}
