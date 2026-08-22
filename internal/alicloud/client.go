// Package alicloud 是阿里云 OpenAPI 的最小客户端，只覆盖 CDT 流量、ECS 实例
// 和账单三块，手写签名不引 SDK（阿里云官方 SDK 会把依赖树拉大一个数量级，
// 而这里统共只用到七八个接口）。
//
// 这些接口都是「RPC 风格」的老式接口：参数拍平成查询串、按字典序拼起来、
// 用 HMAC-SHA1 签名。internal/dns/alidns.go 里已经有一份同样算法的实现，
// 但那份是 GET、私有、并且有单测钉死了它的行为。这里另写一份 POST 表单版：
// ECS 的 DescribeInstances 参数多，GET 容易撞上 URL 长度限制，而且阿里云
// 官方示例对这几个接口给的也是 POST。两份各自独立，谁也别去动谁。
package alicloud

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 站点。国际站和中国站的账单接口不是同一个域名，货币也不一样。
const (
	SiteChina         = "china"
	SiteInternational = "international"
)

// 各产品的接口版本。阿里云每个产品各有各的版本号，签名时必须带对。
const (
	versionECS = "2014-05-26"
	versionBSS = "2017-12-14"
	versionCDT = "2021-08-13"
)

// cdtEndpoint 是中心化的，不分地域 —— CDT 免费额度本来就是按账号算的。
const cdtEndpoint = "cdt.aliyuncs.com"

var bssEndpoints = map[string]string{
	SiteChina:         "business.aliyuncs.com",
	SiteInternational: "business.ap-southeast-1.aliyuncs.com",
}

// httpClient 带超时。阿里云偶尔会把连接挂住，没有超时会让后台同步循环卡死。
var httpClient = &http.Client{Timeout: 20 * time.Second}

// Client 是一个账号 + 一个地域的访问入口。
//
// 地域只影响 ECS：CDT 是账号级的，账单按站点走固定域名。
type Client struct {
	keyID  string
	secret string
	region string
	site   string

	// 这三个 endpoint 做成字段是为了单测能指向 httptest 服务器，
	// 和 internal/dns 里的做法一致。
	ecsEndpoint string
	bssEndpoint string
	cdtEndpoint string

	// scheme 单测里换成 http。
	scheme string
}

// New 构造一个客户端。site 传空按国际站处理（用这个面板的人绝大多数是国际站）。
func New(keyID, secret, region, site string) (*Client, error) {
	if strings.TrimSpace(keyID) == "" || strings.TrimSpace(secret) == "" {
		return nil, fmt.Errorf("阿里云访问凭据不完整：AccessKeyId 和 AccessKeySecret 都要填")
	}
	if strings.TrimSpace(region) == "" {
		return nil, fmt.Errorf("没有指定地域（RegionId）")
	}
	if site != SiteChina {
		site = SiteInternational
	}
	return &Client{
		keyID:       keyID,
		secret:      secret,
		region:      region,
		site:        site,
		ecsEndpoint: "ecs." + region + ".aliyuncs.com",
		bssEndpoint: bssEndpoints[site],
		cdtEndpoint: cdtEndpoint,
		scheme:      "https",
	}, nil
}

// Region 返回这个客户端绑定的地域。
func (c *Client) Region() string { return c.region }

// Currency 返回这个站点的记账货币和符号。
func (c *Client) Currency() (string, string) {
	if c.site == SiteChina {
		return "CNY", "¥"
	}
	return "USD", "$"
}

// aliEncode 实现阿里云要求的百分号编码：在标准 URL 编码基础上，
// 把 + 换成 %20、* 换成 %2A、%7E 还原成 ~。少一条签名就对不上。
func aliEncode(s string) string {
	e := url.QueryEscape(s)
	e = strings.ReplaceAll(e, "+", "%20")
	e = strings.ReplaceAll(e, "*", "%2A")
	e = strings.ReplaceAll(e, "%7E", "~")
	return e
}

// nonce 是防重放随机串。
var nonce = func() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 随机源不可用时退化成时间戳：签名仍然合法，只是防重放弱一些。
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(b)
}

// now 做成变量，单测里固定住时间戳。
var now = func() time.Time { return time.Now().UTC() }

// signedForm 拼出带签名的表单参数。
//
// 签名串的构造：METHOD & encode("/") & encode(按字典序拼好的规范化查询串)。
// 注意规范化查询串里的每个 key/value 都要先 aliEncode 一遍，然后整串再编码一次
// —— 这个「编码两次」是最容易写错的地方。
func (c *Client) signedForm(method, action, version string, extra map[string]string) url.Values {
	params := map[string]string{
		"Action":           action,
		"Version":          version,
		"Format":           "JSON",
		"AccessKeyId":      c.keyID,
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureVersion": "1.0",
		"SignatureNonce":   nonce(),
		"Timestamp":        now().Format("2006-01-02T15:04:05Z"),
	}
	for k, v := range extra {
		params[k] = v
	}

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

	stringToSign := method + "&" + aliEncode("/") + "&" + aliEncode(canonical.String())

	mac := hmac.New(sha1.New, []byte(c.secret+"&"))
	mac.Write([]byte(stringToSign))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	form.Set("Signature", signature)
	return form
}

// call 发一次请求并把响应解进 out。
func (c *Client) call(ctx context.Context, endpoint, action, version string, extra map[string]string, out any) error {
	form := c.signedForm(http.MethodPost, action, version, extra)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.scheme+"://"+endpoint+"/", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("请求阿里云 %s 接口: %w", action, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("读取阿里云 %s 响应: %w", action, err)
	}

	if err := checkAPIError(action, resp.StatusCode, raw); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("解析阿里云 %s 响应: %w", action, err)
	}
	return nil
}

// APIError 是阿里云返回的业务错误。单独成类型，是为了上层能认出
// NoStock 这类需要特殊处理的错误码，而不是去 strings.Contains 错误消息。
type APIError struct {
	Action  string
	Code    string
	Message string
	Status  int
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("阿里云 %s 返回 HTTP %d：%s", e.Action, e.Status, e.Message)
	}
	return fmt.Sprintf("阿里云 %s 返回错误 %s：%s", e.Action, e.Code, e.Message)
}

// IsCode 判断错误是不是某个阿里云错误码。前缀匹配 —— 阿里云的错误码常常带
// 后缀细分（比如 OperationDenied.NoStock），按前缀判断才抓得全。
func IsCode(err error, code string) bool {
	var e *APIError
	if !errors.As(err, &e) {
		return false
	}
	return e.Code == code || strings.HasPrefix(e.Code, code+".") || strings.HasSuffix(e.Code, "."+code)
}

// checkAPIError 把阿里云的错误响应翻成 *APIError。
//
// 阿里云这几个产品的错误表达不统一：ECS/CDT 用 HTTP 4xx + Code/Message，
// BSS 会用 HTTP 200 + Success:false + Code。两种都要认。
func checkAPIError(action string, status int, raw []byte) error {
	var body struct {
		Code    string `json:"Code"`
		Message string `json:"Message"`
		Success *bool  `json:"Success"`
	}
	_ = json.Unmarshal(raw, &body)

	if status >= 400 {
		return &APIError{Action: action, Code: body.Code, Message: body.Message, Status: status}
	}
	// BSS 风格：HTTP 200 但 Success=false。
	if body.Success != nil && !*body.Success {
		return &APIError{Action: action, Code: body.Code, Message: body.Message, Status: status}
	}
	// 少数接口 HTTP 200 也带非成功的 Code。"200"/"Success" 都算成功。
	if body.Success == nil && body.Code != "" && !isOKCode(body.Code) {
		return &APIError{Action: action, Code: body.Code, Message: body.Message, Status: status}
	}
	return nil
}

func isOKCode(code string) bool {
	switch code {
	case "200", "0", "Success", "True", "true":
		return true
	}
	return false
}
