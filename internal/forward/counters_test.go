package forward

import "testing"

// nftJSON 是 `nft -j list table inet vps_forward` 的一份真实形状的输出，
// 覆盖了解析器要处理的全部情况：
//   - prerouting/postrouting 里的规则（必须被忽略，否则重复计数）
//   - account 链里 original/reply 两个方向
//   - tcp+udp 规则拆成的两组计数
//   - account_local / account_local_reply（回环目标，规则里没写 ct direction）
const nftJSON = `{"nftables": [
  {"metainfo": {"version": "1.0.6", "json_schema_version": 1}},
  {"table": {"family": "inet", "name": "vps_forward", "handle": 3}},
  {"chain": {"family": "inet", "table": "vps_forward", "name": "prerouting", "handle": 1}},
  {"rule": {"family": "inet", "table": "vps_forward", "chain": "prerouting", "handle": 4, "expr": [
    {"match": {"op": "==", "left": {"payload": {"protocol": "tcp", "field": "dport"}}, "right": 8443}},
    {"counter": {"packets": 999, "bytes": 999999}},
    {"dnat": {"addr": "1.2.3.4", "port": 443}}
  ]}},
  {"rule": {"family": "inet", "table": "vps_forward", "chain": "postrouting", "handle": 5, "expr": [
    {"counter": {"packets": 888, "bytes": 888888}},
    {"masquerade": null}
  ]}},
  {"rule": {"family": "inet", "table": "vps_forward", "chain": "account", "handle": 6, "expr": [
    {"match": {"op": "==", "left": {"meta": {"key": "l4proto"}}, "right": "tcp"}},
    {"match": {"op": "==", "left": {"ct": {"key": "proto-dst", "dir": "original"}}, "right": 8443}},
    {"match": {"op": "==", "left": {"ct": {"key": "direction"}}, "right": "original"}},
    {"counter": {"packets": 10, "bytes": 1000}}
  ]}},
  {"rule": {"family": "inet", "table": "vps_forward", "chain": "account", "handle": 7, "expr": [
    {"match": {"op": "==", "left": {"meta": {"key": "l4proto"}}, "right": "tcp"}},
    {"match": {"op": "==", "left": {"ct": {"key": "proto-dst", "dir": "original"}}, "right": 8443}},
    {"match": {"op": "==", "left": {"ct": {"key": "direction"}}, "right": "reply"}},
    {"counter": {"packets": 20, "bytes": 2000}}
  ]}},
  {"rule": {"family": "inet", "table": "vps_forward", "chain": "account", "handle": 8, "expr": [
    {"match": {"op": "==", "left": {"meta": {"key": "l4proto"}}, "right": "udp"}},
    {"match": {"op": "==", "left": {"ct": {"key": "proto-dst", "dir": "original"}}, "right": 7000}},
    {"match": {"op": "==", "left": {"ct": {"key": "direction"}}, "right": "original"}},
    {"counter": {"packets": 5, "bytes": 500}}
  ]}},
  {"rule": {"family": "inet", "table": "vps_forward", "chain": "account_local", "handle": 9, "expr": [
    {"match": {"op": "==", "left": {"meta": {"key": "l4proto"}}, "right": "tcp"}},
    {"match": {"op": "==", "left": {"ct": {"key": "proto-dst", "dir": "original"}}, "right": 8080}},
    {"counter": {"packets": 3, "bytes": 300}}
  ]}},
  {"rule": {"family": "inet", "table": "vps_forward", "chain": "account_local_reply", "handle": 10, "expr": [
    {"match": {"op": "==", "left": {"meta": {"key": "l4proto"}}, "right": "tcp"}},
    {"match": {"op": "==", "left": {"ct": {"key": "proto-dst", "dir": "original"}}, "right": 8080}},
    {"counter": {"packets": 4, "bytes": 400}}
  ]}}
]}`

func TestParseNftCountersIgnoresNonAccountChains(t *testing.T) {
	got, err := parseNftCounters([]byte(nftJSON))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// prerouting 和 postrouting 上也有 counter，但把它们算进来就是重复计数。
	for _, c := range got {
		if c.Bytes == 999999 || c.Bytes == 888888 {
			t.Errorf("解析结果里混进了 prerouting/postrouting 的计数: %+v", c)
		}
	}
	if len(got) != 5 {
		t.Fatalf("解析出 %d 条计数，期望 5 条：%+v", len(got), got)
	}
}

func TestParseNftCountersDirections(t *testing.T) {
	got, err := parseNftCounters([]byte(nftJSON))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	find := func(proto string, port int, dir string) (nftCounter, bool) {
		for _, c := range got {
			if c.Proto == proto && c.ListenPort == port && c.Direction == dir {
				return c, true
			}
		}
		return nftCounter{}, false
	}

	cases := []struct {
		proto string
		port  int
		dir   string
		bytes int64
		desc  string
	}{
		{"tcp", 8443, "original", 1000, "转发上行"},
		{"tcp", 8443, "reply", 2000, "转发下行"},
		{"udp", 7000, "original", 500, "tcp+udp 规则的 udp 那一半"},
		// 回环的两条链上规则里没写 ct direction，方向靠链名推断。
		{"tcp", 8080, "original", 300, "回环上行（按 account_local 链名定方向）"},
		{"tcp", 8080, "reply", 400, "回环下行（按 account_local_reply 链名定方向）"},
	}
	for _, tc := range cases {
		c, ok := find(tc.proto, tc.port, tc.dir)
		if !ok {
			t.Errorf("%s：没找到 %s/%d %s 的计数", tc.desc, tc.proto, tc.port, tc.dir)
			continue
		}
		if c.Bytes != tc.bytes {
			t.Errorf("%s：字节数 = %d，期望 %d", tc.desc, c.Bytes, tc.bytes)
		}
	}
}

func TestParseNftCountersOnEmptyTable(t *testing.T) {
	got, err := parseNftCounters([]byte(`{"nftables": [{"metainfo": {"version": "1.0.6"}}]}`))
	if err != nil {
		t.Fatalf("空表不该报错: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("空表应解析出 0 条计数，实际 %d 条", len(got))
	}
}

func TestParseNftCountersRejectsGarbage(t *testing.T) {
	if _, err := parseNftCounters([]byte("not json")); err == nil {
		t.Error("非法 JSON 应该报错")
	}
}

func TestParseL4ProtoAcceptsSetForm(t *testing.T) {
	// 单协议时 nft 输出裸字符串，集合时输出 {"set": [...]}。
	if got := parseL4Proto([]byte(`"tcp"`)); got != "tcp" {
		t.Errorf("裸字符串解析 = %q，期望 tcp", got)
	}
	if got := parseL4Proto([]byte(`{"set": ["tcp", "udp"]}`)); got != "tcp" {
		t.Errorf("集合形式解析 = %q，期望取第一个 tcp", got)
	}
}
