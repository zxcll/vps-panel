package forward

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Counter 是数据面对外的统一计数口径，内核态和用户态都换算成这个形状。
//
// BytesUp 是「客户端 → 目标」方向（conntrack 的 original），
// BytesDown 是「目标 → 客户端」方向（reply）。
//
// 注意这是**累计值**而不是增量，而且会在每次 apply 时归零 —— 上层
// （agent/forward.go）负责把它转成单调递增的逻辑累计量再上报。
type Counter struct {
	HopID     int64 `json:"hop_id"`
	BytesUp   int64 `json:"up"`
	BytesDown int64 `json:"down"`
}

// nftCounter 是从 nft JSON 里读出来的一条原始计数，还没归并到 HopID 上。
type nftCounter struct {
	Proto      string
	ListenPort int
	Direction  string // "original" = 上行，"reply" = 下行
	Bytes      int64
}

// readNftCounters 读回本表所有计数链上的 counter。
// 表不存在等价于「一条规则都没有」，不算错误。
func readNftCounters() ([]nftCounter, error) {
	out, err := nftRun([]string{"-j", "list", "table", TableFamily, TableName}, "")
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "No such file or directory") || strings.Contains(msg, "does not exist") {
			return nil, nil
		}
		return nil, fmt.Errorf("读取 nftables 计数失败: %w", err)
	}
	return parseNftCounters([]byte(out))
}

// 下面这组类型只镜像 nft JSON 输出里我们真正要读的那几个字段。
// 用具体类型而不是 map[string]any 走一遍，是为了让格式变化在编译期/解析期暴露，
// 而不是变成一个到处判断类型断言的迷宫。

type nftDoc struct {
	Nftables []nftItem `json:"nftables"`
}

type nftItem struct {
	Rule *nftRule `json:"rule,omitempty"`
}

type nftRule struct {
	Chain string            `json:"chain"`
	Expr  []json.RawMessage `json:"expr"`
}

// nftExpr 是单个表达式对象的并集信封，每个元素最多只有一个字段非空。
type nftExpr struct {
	Match   *nftMatch      `json:"match,omitempty"`
	Counter *nftCounterVal `json:"counter,omitempty"`
}

type nftMatch struct {
	Left  nftMatchSide    `json:"left"`
	Right json.RawMessage `json:"right"`
}

type nftMatchSide struct {
	Meta *nftMeta `json:"meta,omitempty"`
	Ct   *nftCt   `json:"ct,omitempty"`
}

type nftMeta struct {
	Key string `json:"key"`
}

type nftCt struct {
	Key string `json:"key"`
	Dir string `json:"dir"`
}

type nftCounterVal struct {
	Bytes int64 `json:"bytes"`
}

// parseNftCounters 从 nft -j 的输出里挑出计数链上的规则。
// 只看 account* 三条链 —— prerouting 是 DNAT、postrouting 是 masquerade，
// 把它们算进来会重复计数。
func parseNftCounters(data []byte) ([]nftCounter, error) {
	var doc nftDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("解析 nftables JSON 失败: %w", err)
	}

	var out []nftCounter
	for _, item := range doc.Nftables {
		if item.Rule == nil {
			continue
		}
		switch item.Rule.Chain {
		case "account", "account_local", "account_local_reply":
		default:
			continue
		}

		var c nftCounter
		// account_local 挂在 input 链上，只可能是上行；
		// account_local_reply 挂在 output 上，只可能是下行。
		// 这两条链的规则里没写 ct direction，所以按链名定方向。
		switch item.Rule.Chain {
		case "account_local":
			c.Direction = "original"
		case "account_local_reply":
			c.Direction = "reply"
		}

		hasCounter := false
		for _, raw := range item.Rule.Expr {
			var expr nftExpr
			if err := json.Unmarshal(raw, &expr); err != nil {
				continue
			}
			if expr.Match != nil {
				extractMatch(expr.Match, &c)
			}
			if expr.Counter != nil {
				hasCounter = true
				c.Bytes = expr.Counter.Bytes
			}
		}
		if hasCounter && c.Proto != "" && c.ListenPort != 0 && c.Direction != "" {
			out = append(out, c)
		}
	}
	return out, nil
}

// extractMatch 从一条匹配表达式里挖出协议、监听端口和方向。
func extractMatch(m *nftMatch, c *nftCounter) {
	if m.Left.Meta != nil && m.Left.Meta.Key == "l4proto" {
		c.Proto = parseL4Proto(m.Right)
		return
	}
	if m.Left.Ct == nil {
		return
	}
	switch m.Left.Ct.Key {
	case "proto-dst":
		// nft 把端口输出成 JSON number。
		var port float64
		if json.Unmarshal(m.Right, &port) == nil {
			c.ListenPort = int(port)
		}
	case "direction":
		var dir string
		if json.Unmarshal(m.Right, &dir) == nil {
			c.Direction = dir
		}
	}
}

// parseL4Proto 读 `meta l4proto` 匹配的右值。
// 单协议时 nft 输出裸字符串 "tcp"，集合时输出 {"set":["tcp","udp"]}。
func parseL4Proto(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var set struct {
		Set []string `json:"set"`
	}
	if json.Unmarshal(raw, &set) == nil && len(set.Set) > 0 {
		return set.Set[0]
	}
	return ""
}
