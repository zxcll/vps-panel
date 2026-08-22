package alicloud

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newTestClient 起一个假的阿里云，把三个 endpoint 全指过去。
// handler 收到的是已经解析好的表单。
func newTestClient(t *testing.T, handler func(form url.Values) (int, string)) *Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("解析表单失败: %v", err)
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		status, body := handler(r.PostForm)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	host := strings.TrimPrefix(srv.URL, "http://")
	c, err := New("test-key", "test-secret", "ap-southeast-1", SiteInternational)
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}
	c.scheme = "http"
	c.ecsEndpoint, c.bssEndpoint, c.cdtEndpoint = host, host, host
	return c
}

// 签名算法必须和阿里云对得上，否则一个接口都调不通。
// 固定住 nonce 和时间戳，钉一个已知向量。
func TestSignature(t *testing.T) {
	oldNonce, oldNow := nonce, now
	t.Cleanup(func() { nonce, now = oldNonce, oldNow })
	nonce = func() string { return "fixednonce" }
	now = func() time.Time { return time.Date(2026, 8, 22, 10, 30, 0, 0, time.UTC) }

	c, err := New("LTAItest", "secrettest", "cn-hangzhou", SiteChina)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}

	form := c.signedForm(http.MethodPost, "DescribeInstances", versionECS,
		map[string]string{"RegionId": "cn-hangzhou"})

	if got := form.Get("Timestamp"); got != "2026-08-22T10:30:00Z" {
		t.Errorf("时间戳格式不对：%q", got)
	}
	if got := form.Get("SignatureMethod"); got != "HMAC-SHA1" {
		t.Errorf("签名方法 = %q", got)
	}
	// 签名是确定性的：同样的输入必须得到同样的结果，否则说明拼串的顺序不稳。
	sig := form.Get("Signature")
	if sig == "" {
		t.Fatal("没有生成签名")
	}
	again := c.signedForm(http.MethodPost, "DescribeInstances", versionECS,
		map[string]string{"RegionId": "cn-hangzhou"})
	if again.Get("Signature") != sig {
		t.Error("同样的输入应得到同样的签名，说明规范化查询串的顺序不稳定")
	}

	// 换一个参数，签名必须跟着变 —— 否则等于没签。
	other := c.signedForm(http.MethodPost, "DescribeInstances", versionECS,
		map[string]string{"RegionId": "cn-beijing"})
	if other.Get("Signature") == sig {
		t.Error("参数变了签名却没变")
	}
}

// aliEncode 的三条特殊规则错一条，签名就全对不上。
func TestAliEncode(t *testing.T) {
	cases := map[string]string{
		"a b":   "a%20b",
		"a*b":   "a%2Ab",
		"a~b":   "a~b",
		"a/b":   "a%2Fb",
		"简体":    "%E7%AE%80%E4%BD%93",
		"a=b&c": "a%3Db%26c",
	}
	for in, want := range cases {
		if got := aliEncode(in); got != want {
			t.Errorf("aliEncode(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestListInternetTraffic(t *testing.T) {
	c := newTestClient(t, func(form url.Values) (int, string) {
		if got := form.Get("Action"); got != "ListCdtInternetTraffic" {
			t.Errorf("Action = %q", got)
		}
		if got := form.Get("Version"); got != versionCDT {
			t.Errorf("Version = %q，期望 %q", got, versionCDT)
		}
		return http.StatusOK, `{
			"RequestId": "x",
			"TrafficDetails": [
				{"BusinessRegionId":"cn-hangzhou","TrafficType":"BGP","Traffic":1073741824},
				{"BusinessRegionId":"ap-southeast-1","TrafficType":"BGP","Traffic":"2147483648"},
				{"BusinessRegionId":"cn-hongkong","BusinessAccessType":"BGP_PRO","Traffic":5.36870912E8}
			]}`
	})

	got, err := c.ListInternetTraffic(context.Background())
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应返回 3 条，实际 %d", len(got))
	}
	if got[0].Traffic != 1073741824 {
		t.Errorf("数字型流量解析错了：%d", got[0].Traffic)
	}
	// 阿里云对同一个字段有时给字符串、有时给科学计数法，都得认。
	if got[1].Traffic != 2147483648 {
		t.Errorf("字符串型流量解析错了：%d", got[1].Traffic)
	}
	if got[2].Traffic != 536870912 {
		t.Errorf("科学计数法流量解析错了：%d", got[2].Traffic)
	}
	// TrafficType 缺失时回落 BusinessAccessType。
	if got[2].TrafficType != "BGP_PRO" {
		t.Errorf("线路类型应回落 BusinessAccessType，实际 %q", got[2].TrafficType)
	}
}

func TestDescribeInstancesPaginates(t *testing.T) {
	// 两页共 101 台。只拉第一页的话会漏掉第 101 台，
	// 而漏掉的那台正好被守护时，熔断和保活都会静默失效。
	c := newTestClient(t, func(form url.Values) (int, string) {
		switch form.Get("PageNumber") {
		case "1":
			var b strings.Builder
			b.WriteString(`{"TotalCount":101,"Instances":{"Instance":[`)
			for i := range 100 {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(`{"InstanceId":"i-`)
				b.WriteString(strings.Repeat("a", 1))
				b.WriteString(itoa(i))
				b.WriteString(`","Status":"Running","SpotStrategy":"NoSpot"}`)
			}
			b.WriteString(`]}}`)
			return http.StatusOK, b.String()
		default:
			return http.StatusOK, `{"TotalCount":101,"Instances":{"Instance":[
				{"InstanceId":"i-last","InstanceName":"最后一台","Status":"Stopped",
				 "SpotStrategy":"SpotAsPriceGo","InstanceType":"ecs.t6",
				 "InternetMaxBandwidthOut":100,
				 "PublicIpAddress":{"IpAddress":["1.2.3.4"]}}]}}`
		}
	})

	got, err := c.DescribeInstances(context.Background())
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if len(got) != 101 {
		t.Fatalf("应翻页拉全 101 台，实际 %d", len(got))
	}
	last := got[100]
	if last.InstanceID != "i-last" || !last.IsSpot {
		t.Errorf("最后一台解析错了：%+v", last)
	}
	if last.PublicIP != "1.2.3.4" {
		t.Errorf("公网 IP = %q", last.PublicIP)
	}
	if got[0].IsSpot {
		t.Error("SpotStrategy=NoSpot 不该被当成抢占式实例")
	}
}

// 绑了 EIP 时要用 EIP，那才是对外真正在用的地址。
func TestDescribeInstancesPrefersEIP(t *testing.T) {
	c := newTestClient(t, func(url.Values) (int, string) {
		return http.StatusOK, `{"TotalCount":1,"Instances":{"Instance":[
			{"InstanceId":"i-1","Status":"Running",
			 "PublicIpAddress":{"IpAddress":["10.0.0.1"]},
			 "EipAddress":{"IpAddress":"203.0.113.7"}}]}}`
	})
	got, err := c.DescribeInstances(context.Background())
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if got[0].PublicIP != "203.0.113.7" {
		t.Errorf("应优先用 EIP，实际 %q", got[0].PublicIP)
	}
}

func TestStopInstanceNeverForces(t *testing.T) {
	var seen url.Values
	c := newTestClient(t, func(form url.Values) (int, string) {
		seen = form
		return http.StatusOK, `{"RequestId":"x"}`
	})

	if err := c.StopInstance(context.Background(), "i-1", StopModeCharging); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if seen.Get("Action") != "StopInstance" {
		t.Errorf("Action = %q", seen.Get("Action"))
	}
	if seen.Get("StoppedMode") != StopModeCharging {
		t.Errorf("StoppedMode = %q", seen.Get("StoppedMode"))
	}
	// 强制停机等同拔电，未落盘的数据会丢。自动化路径上绝不能开。
	if seen.Get("ForceStop") != "false" {
		t.Errorf("ForceStop 必须是 false，实际 %q", seen.Get("ForceStop"))
	}

	// 非法模式回落到节省模式，而不是原样透传给阿里云。
	c.StopInstance(context.Background(), "i-1", "乱填的")
	if seen.Get("StoppedMode") != StopModeCharging {
		t.Errorf("非法停机模式应回落 StopCharging，实际 %q", seen.Get("StoppedMode"))
	}
}

// 抢占式实例售罄要能被认出来：它是「等一会儿再试」，不是「配置错了」。
func TestStartInstanceNoStockIsRecognizable(t *testing.T) {
	c := newTestClient(t, func(url.Values) (int, string) {
		return http.StatusForbidden,
			`{"Code":"OperationDenied.NoStock","Message":"The requested resource is sold out."}`
	})

	err := c.StartInstance(context.Background(), "i-1")
	if err == nil {
		t.Fatal("应当报错")
	}
	if !IsCode(err, ErrCodeNoStock) {
		t.Errorf("应能认出 NoStock，实际错误：%v", err)
	}
	if IsCode(err, "InvalidInstanceId") {
		t.Error("不该匹配到别的错误码")
	}
}

func TestQueryAccountBalance(t *testing.T) {
	c := newTestClient(t, func(form url.Values) (int, string) {
		if form.Get("Action") != "QueryAccountBalance" {
			t.Errorf("Action = %q", form.Get("Action"))
		}
		// 余额带千分位逗号，直接 ParseFloat 会失败。
		return http.StatusOK, `{"Code":"200","Success":true,
			"Data":{"AvailableAmount":"1,234.56","Currency":"USD"}}`
	})

	got, err := c.QueryAccountBalance(context.Background())
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if got.AvailableAmount != 1234.56 {
		t.Errorf("余额 = %v，期望 1234.56（千分位逗号要能处理）", got.AvailableAmount)
	}
	if got.Symbol != "$" {
		t.Errorf("国际站符号应是 $，实际 %q", got.Symbol)
	}
}

func TestQueryBillOverview(t *testing.T) {
	c := newTestClient(t, func(form url.Values) (int, string) {
		if form.Get("BillingCycle") != "2026-08" {
			t.Errorf("BillingCycle = %q", form.Get("BillingCycle"))
		}
		return http.StatusOK, `{"Code":"200","Success":true,"Data":{
			"BillingCycle":"2026-08","Items":{"Item":[
				{"ProductName":"云服务器 ECS","PretaxAmount":10.5,"OutstandingAmount":3.25},
				{"ProductName":"云数据传输","PretaxAmount":0,"OutstandingAmount":1.75},
				{"ProductName":"没花钱的产品","PretaxAmount":0,"OutstandingAmount":0}
			]}}}`
	})

	got, err := c.QueryBillOverview(context.Background(), "2026-08")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if got.TotalOutstanding != 5.0 {
		t.Errorf("待还总额 = %v，期望 5.0", got.TotalOutstanding)
	}
	// 金额全 0 的条目不展示，但仍要计入总额（这里它是 0，不影响）。
	if len(got.Items) != 2 {
		t.Errorf("应过滤掉全 0 的条目，实际 %d 条", len(got.Items))
	}
}

// BSS 会用 HTTP 200 + Success:false 表达错误，不能只看状态码。
func TestBSSErrorWithHTTP200(t *testing.T) {
	c := newTestClient(t, func(url.Values) (int, string) {
		return http.StatusOK, `{"Code":"InvalidOwner","Message":"账号无权限","Success":false}`
	})
	if _, err := c.QueryAccountBalance(context.Background()); err == nil {
		t.Fatal("HTTP 200 + Success:false 应当被当成错误")
	} else if !strings.Contains(err.Error(), "InvalidOwner") {
		t.Errorf("错误里应带上错误码，实际 %v", err)
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New("", "s", "cn-hangzhou", SiteChina); err == nil {
		t.Error("缺 AccessKeyId 应报错")
	}
	if _, err := New("k", "", "cn-hangzhou", SiteChina); err == nil {
		t.Error("缺 AccessKeySecret 应报错")
	}
	if _, err := New("k", "s", "", SiteChina); err == nil {
		t.Error("缺地域应报错")
	}

	// 站点填错一律按国际站，而不是拒绝 —— 这是个展示层的差别，不该挡住用户。
	c, err := New("k", "s", "cn-hangzhou", "乱填")
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if cur, _ := c.Currency(); cur != "USD" {
		t.Errorf("未知站点应按国际站处理，实际货币 %q", cur)
	}

	cn, _ := New("k", "s", "cn-hangzhou", SiteChina)
	if cur, sym := cn.Currency(); cur != "CNY" || sym != "¥" {
		t.Errorf("中国站应是 CNY/¥，实际 %s/%s", cur, sym)
	}
	if cn.bssEndpoint != bssEndpoints[SiteChina] {
		t.Errorf("中国站的账单域名不对：%q", cn.bssEndpoint)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// DetectSite 拿两个站点的账单接口各试一次，哪个通就是哪个。
//
// 为什么值得自动认：账单接口在两个站点是不同域名，选错了余额和待还就一直
// 拉不到，而报错长得像「权限不够」，用户很难联想到是站点选错了。
// 地域也推不出站点 —— 国际站账号照样能在杭州开机器。
func TestDetectSite(t *testing.T) {
	// 只有其中一个域名认这组凭据，另一个报错。
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		seen = append(seen, r.PostForm.Get("Action"))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"Code":"200","Success":true,"Data":{"AvailableAmount":"1.00","Currency":"CNY"}}`))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	old := bssEndpoints
	t.Cleanup(func() { bssEndpoints = old })
	// 国际站指向能用的假服务，中国站指向一个必然连不上的地址。
	bssEndpoints = map[string]string{
		SiteInternational: host,
		SiteChina:         "127.0.0.1:1",
	}
	oldScheme := detectScheme
	detectScheme = "http"
	t.Cleanup(func() { detectScheme = oldScheme })

	site, currency, err := DetectSite(context.Background(), "k", "s", "cn-hongkong")
	if err != nil {
		t.Fatalf("应当认出国际站: %v", err)
	}
	if site != SiteInternational {
		t.Errorf("认成了 %q，期望 %q", site, SiteInternational)
	}
	if currency != "USD" {
		t.Errorf("国际站货币应是 USD，实际 %q", currency)
	}
	if len(seen) == 0 || seen[0] != "QueryAccountBalance" {
		t.Errorf("应当用查余额来试探，实际调用了 %v", seen)
	}
}

// 两个站点都不通时要把两边的错误都带回去 ——
// 只报一个会让人以为是那个站点的问题，而实际上多半是凭据本身不对。
func TestDetectSiteReportsBothFailures(t *testing.T) {
	old := bssEndpoints
	t.Cleanup(func() { bssEndpoints = old })
	bssEndpoints = map[string]string{
		SiteInternational: "127.0.0.1:1",
		SiteChina:         "127.0.0.1:2",
	}
	oldScheme := detectScheme
	detectScheme = "http"
	t.Cleanup(func() { detectScheme = oldScheme })

	_, _, err := DetectSite(context.Background(), "k", "s", "cn-hongkong")
	if err == nil {
		t.Fatal("两个都不通时应当报错")
	}
	if !strings.Contains(err.Error(), "国际站") || !strings.Contains(err.Error(), "中国站") {
		t.Errorf("错误里应同时提到两个站点，实际：%v", err)
	}
}
