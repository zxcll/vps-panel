package dns

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSubDomain(t *testing.T) {
	cases := []struct{ name, zone, want string }{
		{"us.example.com", "example.com", "us"},
		{"a.b.example.com", "example.com", "a.b"},
		{"example.com", "example.com", "@"},
		{"example.com.", "example.com", "@"},
		{"US.Example.COM", "example.com", "us"},
		{"", "example.com", "@"},
		{"other.org", "example.com", "other.org"}, // 对不上时原样返回，交给服务商报错
	}
	for _, tc := range cases {
		if got := subDomain(tc.name, tc.zone); got != tc.want {
			t.Errorf("subDomain(%q, %q) = %q，期望 %q", tc.name, tc.zone, got, tc.want)
		}
	}
}

func TestFQDN(t *testing.T) {
	cases := []struct{ sub, zone, want string }{
		{"us", "example.com", "us.example.com"},
		{"@", "example.com", "example.com"},
		{"", "example.com", "example.com"},
		{"a.b", "example.com", "a.b.example.com"},
	}
	for _, tc := range cases {
		if got := fqdn(tc.sub, tc.zone); got != tc.want {
			t.Errorf("fqdn(%q, %q) = %q，期望 %q", tc.sub, tc.zone, got, tc.want)
		}
	}
}

func TestNewProviderValidation(t *testing.T) {
	if _, err := New("bogus", Credentials{}); err == nil {
		t.Error("未知服务商类型应报错")
	}
	if _, err := New("cloudflare", Credentials{}); err == nil {
		t.Error("Cloudflare 缺 Token 应报错")
	}
	if _, err := New("dnspod", Credentials{SecretID: "x"}); err == nil {
		t.Error("DNSPod 缺 SecretKey 应报错")
	}
	if _, err := New("alidns", Credentials{AccessKeyID: "x"}); err == nil {
		t.Error("阿里云缺 AccessKeySecret 应报错")
	}
	if _, err := New("cloudflare", Credentials{APIToken: "t"}); err != nil {
		t.Errorf("凭据完整时不该报错: %v", err)
	}
}

func TestParseCredentials(t *testing.T) {
	c, err := ParseCredentials(`{"api_token":"abc","zone_id":"z1"}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.APIToken != "abc" || c.ZoneID != "z1" {
		t.Errorf("解析结果不对: %+v", c)
	}
	if _, err := ParseCredentials(""); err == nil {
		t.Error("空凭据应报错")
	}
	if _, err := ParseCredentials("not json"); err == nil {
		t.Error("非法 JSON 应报错")
	}
}

// --- Cloudflare ---

func TestCloudflareListAndUpsert(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")

		switch {
		case !strings.Contains(r.URL.Path, "/dns_records"):
			// 按域名查 Zone ID
			io.WriteString(w, `{"success":true,"errors":[],"result":[{"id":"zone-123","name":"example.com"}]}`)

		case r.Method == http.MethodGet:
			io.WriteString(w, `{"success":true,"errors":[],"result":[
				{"id":"rec-1","type":"A","name":"us.example.com","content":"1.1.1.1","ttl":60,"proxied":false}]}`)

		case r.Method == http.MethodPut:
			json.NewDecoder(r.Body).Decode(&gotBody)
			io.WriteString(w, `{"success":true,"errors":[],"result":
				{"id":"rec-1","type":"A","name":"us.example.com","content":"2.2.2.2","ttl":60,"proxied":false}}`)

		default:
			io.WriteString(w, `{"success":false,"errors":[{"code":7000,"message":"路由未匹配"}]}`)
		}
	}))
	defer srv.Close()

	cf := &cloudflare{token: "test-token", base: srv.URL, zoneCache: map[string]string{}}
	ctx := context.Background()

	recs, err := cf.List(ctx, "example.com", "us.example.com", "A")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != "rec-1" || recs[0].Content != "1.1.1.1" {
		t.Fatalf("List 结果不对: %+v", recs)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization 头 = %q，期望 Bearer test-token", gotAuth)
	}

	updated, err := cf.Upsert(ctx, Record{
		ID: "rec-1", Zone: "example.com", Name: "us.example.com",
		Type: "A", Content: "2.2.2.2", TTL: 60,
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("带 ID 的 Upsert 应走 PUT，实际 %s", gotMethod)
	}
	if !strings.HasSuffix(gotPath, "/dns_records/rec-1") {
		t.Errorf("请求路径 = %q，应指向 rec-1", gotPath)
	}
	if gotBody["content"] != "2.2.2.2" {
		t.Errorf("请求体 content = %v，期望 2.2.2.2", gotBody["content"])
	}
	if updated.Content != "2.2.2.2" {
		t.Errorf("返回记录 content = %q", updated.Content)
	}

	// zone ID 应该被缓存，第二次不再查 /zones
	gotPath = ""
	if _, err := cf.List(ctx, "example.com", "us.example.com", "A"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotPath, "/zones/zone-123/dns_records") {
		t.Errorf("第二次查询未复用缓存的 zone ID，路径 = %q", gotPath)
	}
}

// 开了 Cloudflare 代理时 TTL 必须提交为 1，否则接口报错。
func TestCloudflareProxiedForcesAutoTTL(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPut {
			json.NewDecoder(r.Body).Decode(&gotBody)
			io.WriteString(w, `{"success":true,"errors":[],"result":{"id":"rec-1","ttl":1}}`)
			return
		}
		io.WriteString(w, `{"success":true,"errors":[],"result":[{"id":"zone-1","name":"example.com"}]}`)
	}))
	defer srv.Close()

	cf := &cloudflare{token: "t", base: srv.URL, zoneCache: map[string]string{}}
	_, err := cf.Upsert(context.Background(), Record{
		ID: "rec-1", Zone: "example.com", Name: "a.example.com",
		Type: "A", Content: "3.3.3.3", TTL: 300, Proxied: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["ttl"] != float64(1) {
		t.Errorf("开启代理时提交的 TTL = %v，应为 1", gotBody["ttl"])
	}
}

func TestCloudflareAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"success":false,"errors":[{"code":9109,"message":"Token 权限不足"}]}`)
	}))
	defer srv.Close()

	cf := &cloudflare{token: "t", base: srv.URL, zoneCache: map[string]string{}}
	_, err := cf.List(context.Background(), "example.com", "a.example.com", "A")
	if err == nil {
		t.Fatal("API 报错时应返回错误")
	}
	if !strings.Contains(err.Error(), "9109") || !strings.Contains(err.Error(), "Token 权限不足") {
		t.Errorf("错误信息应包含服务商返回的原因，实际: %v", err)
	}
}

func TestCloudflareZoneNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"success":true,"errors":[],"result":[]}`)
	}))
	defer srv.Close()

	cf := &cloudflare{token: "t", base: srv.URL, zoneCache: map[string]string{}}
	_, err := cf.List(context.Background(), "notmine.com", "a.notmine.com", "A")
	if err == nil || !strings.Contains(err.Error(), "找不到域名") {
		t.Errorf("域名不在账号下时应给出明确提示，实际: %v", err)
	}
}

// --- 腾讯云 DNSPod ---

func TestDNSPodListAndUpsert(t *testing.T) {
	var gotAction, gotAuth string
	var gotPayload map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAction = r.Header.Get("X-TC-Action")
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotPayload)
		w.Header().Set("Content-Type", "application/json")

		switch gotAction {
		case "DescribeRecordList":
			io.WriteString(w, `{"Response":{"RequestId":"r1","RecordList":[
				{"RecordId":12345,"Name":"us","Type":"A","Value":"1.1.1.1","Line":"默认","TTL":600}]}}`)
		case "ModifyRecord":
			io.WriteString(w, `{"Response":{"RequestId":"r2","RecordId":12345}}`)
		case "CreateRecord":
			io.WriteString(w, `{"Response":{"RequestId":"r3","RecordId":67890}}`)
		default:
			io.WriteString(w, `{"Response":{"Error":{"Code":"InvalidAction","Message":"未知操作"}}}`)
		}
	}))
	defer srv.Close()

	d := &dnspod{secretID: "sid", secretKey: "skey", endpoint: srv.URL}
	ctx := context.Background()

	recs, err := d.List(ctx, "example.com", "us.example.com", "A")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != "12345" || recs[0].Name != "us.example.com" {
		t.Fatalf("List 结果不对: %+v", recs)
	}
	if gotPayload["Subdomain"] != "us" {
		t.Errorf("提交的 Subdomain = %v，期望 us", gotPayload["Subdomain"])
	}
	if !strings.HasPrefix(gotAuth, "TC3-HMAC-SHA256 Credential=sid/") {
		t.Errorf("Authorization 头格式不对: %q", gotAuth)
	}
	if !strings.Contains(gotAuth, "SignedHeaders=content-type;host;x-tc-action") {
		t.Errorf("Authorization 缺少 SignedHeaders: %q", gotAuth)
	}

	// 带 ID → 走 ModifyRecord
	if _, err := d.Upsert(ctx, Record{
		ID: "12345", Zone: "example.com", Name: "us.example.com",
		Type: "A", Content: "2.2.2.2", TTL: 600,
	}); err != nil {
		t.Fatalf("Upsert(修改): %v", err)
	}
	if gotAction != "ModifyRecord" {
		t.Errorf("带 ID 应调用 ModifyRecord，实际 %s", gotAction)
	}
	if gotPayload["Value"] != "2.2.2.2" {
		t.Errorf("提交的 Value = %v", gotPayload["Value"])
	}
	if gotPayload["RecordLine"] != "默认" {
		t.Errorf("未指定线路时应默认为「默认」，实际 %v", gotPayload["RecordLine"])
	}

	// 不带 ID → 走 CreateRecord
	created, err := d.Upsert(ctx, Record{
		Zone: "example.com", Name: "new.example.com", Type: "A", Content: "3.3.3.3",
	})
	if err != nil {
		t.Fatalf("Upsert(新建): %v", err)
	}
	if gotAction != "CreateRecord" {
		t.Errorf("不带 ID 应调用 CreateRecord，实际 %s", gotAction)
	}
	if created.ID != "67890" {
		t.Errorf("新建后的记录 ID = %q，期望 67890", created.ID)
	}
}

// 子域名一条记录都没有时，DNSPod 返回错误而不是空列表；这属于正常情况。
func TestDNSPodEmptyResultIsNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"Response":{"Error":{"Code":"ResourceNotFound.NoDataOfRecord","Message":"无记录"}}}`)
	}))
	defer srv.Close()

	d := &dnspod{secretID: "s", secretKey: "k", endpoint: srv.URL}
	recs, err := d.List(context.Background(), "example.com", "new.example.com", "A")
	if err != nil {
		t.Fatalf("子域名无记录不该报错: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("期望空结果，实际 %+v", recs)
	}
}

func TestDNSPodRealError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"Response":{"Error":{"Code":"AuthFailure.SignatureFailure","Message":"签名错误"}}}`)
	}))
	defer srv.Close()

	d := &dnspod{secretID: "s", secretKey: "k", endpoint: srv.URL}
	_, err := d.List(context.Background(), "example.com", "a.example.com", "A")
	if err == nil || !strings.Contains(err.Error(), "AuthFailure") {
		t.Errorf("真实错误应向上抛，实际: %v", err)
	}
}

// TC3 签名是确定性的：固定输入必须得到固定输出。
func TestDNSPodSignatureDeterministic(t *testing.T) {
	d := &dnspod{secretID: "AKIDz8krbsJ5", secretKey: "Gu5t9xGARNpq86cd98joQYCN3Cozk1qA"}
	a := d.sign("DescribeRecordList", `{"Domain":"example.com"}`, 1700000000)
	b := d.sign("DescribeRecordList", `{"Domain":"example.com"}`, 1700000000)
	if a != b {
		t.Error("相同输入的签名应完全一致")
	}
	// 换个 payload 签名必须变
	c := d.sign("DescribeRecordList", `{"Domain":"other.com"}`, 1700000000)
	if a == c {
		t.Error("payload 变化后签名应改变")
	}
	if !strings.Contains(a, "Credential=AKIDz8krbsJ5/2023-11-14/dnspod/tc3_request") {
		t.Errorf("凭据范围不对: %q", a)
	}
}

// --- 阿里云 DNS ---

func TestAlidnsListAndUpsert(t *testing.T) {
	var gotQuery url.Values

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")

		switch gotQuery.Get("Action") {
		case "DescribeSubDomainRecords":
			io.WriteString(w, `{"TotalCount":1,"DomainRecords":{"Record":[
				{"RecordId":"999","DomainName":"example.com","RR":"us","Type":"A","Value":"1.1.1.1","TTL":600,"Line":"default"}]}}`)
		case "UpdateDomainRecord":
			io.WriteString(w, `{"RecordId":"999","RequestId":"x"}`)
		case "AddDomainRecord":
			io.WriteString(w, `{"RecordId":"1000","RequestId":"y"}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"Code":"InvalidAction","Message":"未知操作"}`)
		}
	}))
	defer srv.Close()

	a := &alidns{keyID: "ak", secret: "sk", endpoint: srv.URL + "/"}
	ctx := context.Background()

	recs, err := a.List(ctx, "example.com", "us.example.com", "A")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != "999" || recs[0].Name != "us.example.com" {
		t.Fatalf("List 结果不对: %+v", recs)
	}
	if gotQuery.Get("SubDomain") != "us.example.com" {
		t.Errorf("提交的 SubDomain = %q", gotQuery.Get("SubDomain"))
	}
	if gotQuery.Get("Signature") == "" {
		t.Error("请求缺少 Signature 参数")
	}
	if gotQuery.Get("SignatureMethod") != "HMAC-SHA1" {
		t.Errorf("签名方法 = %q，期望 HMAC-SHA1", gotQuery.Get("SignatureMethod"))
	}

	if _, err := a.Upsert(ctx, Record{
		ID: "999", Zone: "example.com", Name: "us.example.com",
		Type: "A", Content: "2.2.2.2", TTL: 600,
	}); err != nil {
		t.Fatalf("Upsert(修改): %v", err)
	}
	if gotQuery.Get("Action") != "UpdateDomainRecord" {
		t.Errorf("带 ID 应调用 UpdateDomainRecord，实际 %s", gotQuery.Get("Action"))
	}
	if gotQuery.Get("RR") != "us" || gotQuery.Get("Value") != "2.2.2.2" {
		t.Errorf("提交参数不对: RR=%q Value=%q", gotQuery.Get("RR"), gotQuery.Get("Value"))
	}

	created, err := a.Upsert(ctx, Record{
		Zone: "example.com", Name: "new.example.com", Type: "A", Content: "3.3.3.3",
	})
	if err != nil {
		t.Fatalf("Upsert(新建): %v", err)
	}
	if gotQuery.Get("Action") != "AddDomainRecord" || gotQuery.Get("DomainName") != "example.com" {
		t.Errorf("新建参数不对: %v", gotQuery)
	}
	if created.ID != "1000" {
		t.Errorf("新建后 ID = %q，期望 1000", created.ID)
	}
}

// 新值与旧值相同时阿里云报 DomainRecordDuplicate，对我们来说目标已达成。
func TestAlidnsDuplicateIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"Code":"DomainRecordDuplicate","Message":"记录已存在"}`)
	}))
	defer srv.Close()

	a := &alidns{keyID: "ak", secret: "sk", endpoint: srv.URL + "/"}
	if _, err := a.Upsert(context.Background(), Record{
		ID: "1", Zone: "example.com", Name: "a.example.com", Type: "A", Content: "1.1.1.1",
	}); err != nil {
		t.Errorf("重复记录不该算失败: %v", err)
	}
}

func TestAliEncode(t *testing.T) {
	cases := map[string]string{
		"a b":   "a%20b",
		"a*b":   "a%2Ab",
		"a~b":   "a~b",
		"a/b":   "a%2Fb",
		"简体":    url.QueryEscape("简体"),
		"a=b&c": "a%3Db%26c",
	}
	for in, want := range cases {
		if got := aliEncode(in); got != want {
			t.Errorf("aliEncode(%q) = %q，期望 %q", in, got, want)
		}
	}
}
