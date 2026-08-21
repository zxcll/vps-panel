package dns

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 腾讯云 DNSPod 走的是腾讯云 API 3.0，签名算法 TC3-HMAC-SHA256。
// 这里手写签名，避免为了两个接口拖进整个腾讯云 SDK。
const (
	tcHost    = "dnspod.tencentcloudapi.com"
	tcService = "dnspod"
	tcVersion = "2021-03-23"
	tcAlgo    = "TC3-HMAC-SHA256"
)

type dnspod struct {
	secretID  string
	secretKey string
	// endpoint 是接口地址，单测里指向 httptest 服务器。
	endpoint string
}

func newDNSPod(c Credentials) (Provider, error) {
	if c.SecretID == "" || c.SecretKey == "" {
		return nil, fmt.Errorf("腾讯云 DNSPod 需要填写 SecretId 和 SecretKey")
	}
	return &dnspod{secretID: c.SecretID, secretKey: c.SecretKey, endpoint: "https://" + tcHost}, nil
}

func (d *dnspod) Type() string { return "dnspod" }

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// sign 按 TC3-HMAC-SHA256 生成 Authorization 头。
func (d *dnspod) sign(action, payload string, ts int64) string {
	date := time.Unix(ts, 0).UTC().Format("2006-01-02")

	// 1. 拼规范请求串
	canonicalHeaders := "content-type:application/json; charset=utf-8\n" +
		"host:" + tcHost + "\n" +
		"x-tc-action:" + strings.ToLower(action) + "\n"
	signedHeaders := "content-type;host;x-tc-action"
	canonicalRequest := strings.Join([]string{
		http.MethodPost,
		"/",
		"", // 查询串为空，参数全在 body 里
		canonicalHeaders,
		signedHeaders,
		sha256Hex(payload),
	}, "\n")

	// 2. 拼待签名字符串
	credentialScope := date + "/" + tcService + "/tc3_request"
	stringToSign := strings.Join([]string{
		tcAlgo,
		strconv.FormatInt(ts, 10),
		credentialScope,
		sha256Hex(canonicalRequest),
	}, "\n")

	// 3. 逐层派生签名密钥
	secretDate := hmacSHA256([]byte("TC3"+d.secretKey), date)
	secretService := hmacSHA256(secretDate, tcService)
	secretSigning := hmacSHA256(secretService, "tc3_request")
	signature := hex.EncodeToString(hmacSHA256(secretSigning, stringToSign))

	return fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		tcAlgo, d.secretID, credentialScope, signedHeaders, signature)
}

// tcEnvelope 是腾讯云统一响应信封。
type tcEnvelope struct {
	Response struct {
		Error *struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		} `json:"Error"`
		RequestID string `json:"RequestId"`
	} `json:"Response"`
}

func (d *dnspod) call(ctx context.Context, action string, params any, out any) error {
	payloadBytes, err := json.Marshal(params)
	if err != nil {
		return err
	}
	payload := string(payloadBytes)
	ts := time.Now().Unix()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		d.endpoint, bytes.NewReader(payloadBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Host", tcHost)
	req.Header.Set("X-TC-Action", action)
	req.Header.Set("X-TC-Version", tcVersion)
	req.Header.Set("X-TC-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("Authorization", d.sign(action, payload, ts))

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("请求腾讯云 DNSPod API: %w", err)
	}
	defer resp.Body.Close()

	var raw bytes.Buffer
	if _, err := raw.ReadFrom(resp.Body); err != nil {
		return fmt.Errorf("读取腾讯云响应: %w", err)
	}

	var env tcEnvelope
	if err := json.Unmarshal(raw.Bytes(), &env); err != nil {
		return fmt.Errorf("解析腾讯云响应(HTTP %d): %w", resp.StatusCode, err)
	}
	if env.Response.Error != nil {
		return fmt.Errorf("腾讯云 DNSPod 返回错误 %s: %s",
			env.Response.Error.Code, env.Response.Error.Message)
	}
	if out != nil {
		// 业务字段都在 Response 下面，再解一层
		var wrapper struct {
			Response json.RawMessage `json:"Response"`
		}
		if err := json.Unmarshal(raw.Bytes(), &wrapper); err != nil {
			return err
		}
		return json.Unmarshal(wrapper.Response, out)
	}
	return nil
}

func (d *dnspod) List(ctx context.Context, zone, name, rtype string) ([]Record, error) {
	sub := subDomain(name, zone)

	params := map[string]any{
		"Domain":    strings.TrimSuffix(zone, "."),
		"Subdomain": sub,
		"Limit":     uint64(100),
	}
	if rtype != "" {
		params["RecordType"] = rtype
	}

	var res struct {
		RecordList []struct {
			RecordId uint64 `json:"RecordId"`
			Name     string `json:"Name"`
			Type     string `json:"Type"`
			Value    string `json:"Value"`
			Line     string `json:"Line"`
			TTL      uint64 `json:"TTL"`
		} `json:"RecordList"`
	}

	if err := d.call(ctx, "DescribeRecordList", params, &res); err != nil {
		// 子域名下一条记录都没有时，DNSPod 直接报"无记录"错误而不是返回空列表。
		// 这在我们的场景里是正常情况（首次配置），当成空结果处理。
		if strings.Contains(err.Error(), "ResourceNotFound") ||
			strings.Contains(err.Error(), "无记录") {
			return []Record{}, nil
		}
		return nil, err
	}

	out := make([]Record, 0, len(res.RecordList))
	for _, r := range res.RecordList {
		out = append(out, Record{
			ID:      strconv.FormatUint(r.RecordId, 10),
			Zone:    zone,
			Name:    fqdn(r.Name, zone),
			Type:    r.Type,
			Content: r.Value,
			TTL:     int(r.TTL),
			Line:    r.Line,
		})
	}
	return out, nil
}

func (d *dnspod) Upsert(ctx context.Context, rec Record) (Record, error) {
	sub := subDomain(rec.Name, rec.Zone)
	ttl := rec.TTL
	if ttl <= 0 {
		ttl = 600
	}
	line := rec.Line
	if line == "" {
		line = "默认"
	}

	params := map[string]any{
		"Domain":     strings.TrimSuffix(rec.Zone, "."),
		"SubDomain":  sub,
		"RecordType": rec.Type,
		"RecordLine": line,
		"Value":      rec.Content,
		"TTL":        uint64(ttl),
	}

	if rec.ID != "" {
		id, err := strconv.ParseUint(rec.ID, 10, 64)
		if err != nil {
			return rec, fmt.Errorf("非法的 DNSPod 记录 ID %q: %w", rec.ID, err)
		}
		params["RecordId"] = id
		if err := d.call(ctx, "ModifyRecord", params, nil); err != nil {
			return rec, err
		}
		return rec, nil
	}

	var res struct {
		RecordId uint64 `json:"RecordId"`
	}
	if err := d.call(ctx, "CreateRecord", params, &res); err != nil {
		return rec, err
	}
	rec.ID = strconv.FormatUint(res.RecordId, 10)
	return rec, nil
}
