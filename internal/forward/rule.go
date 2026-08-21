// Package forward 是端口转发的数据面：把一份规则集落到内核（nftables DNAT + tc 限速）
// 和用户态（内嵌 TCP relay）两个后端上，并把两边的流量计数合并成统一口径。
//
// 两种转发模式可以逐条规则混用：
//
//   - 内核态（默认）：nftables DNAT，零拷贝，TCP / UDP / TCP+UDP 全支持
//   - 用户态：每一跳单独建 TCP 连接，规避多跳串联时 TCP-over-TCP 的拥塞叠加；
//     只支持 TCP，但带预连接池，能省掉与目标握手的往返
//
// 重要：这里产出的流量计数**不进节点配额账本**。节点账本只认网卡累计计数器
// （/proc/net/dev），转发流量在网卡上进出各一遍，本来就已经计入了；
// 这里的 per-rule 计数只用于「哪条规则用了多少」的分账展示。
// 详见 internal/quota.ForwardShare 的换算说明。
package forward

import (
	"fmt"
	"net"
	"strings"

	"github.com/zxcll/vps-panel/internal/resolver"
)

const (
	// TableName 是本项目独占的 nftables 表名。整张表由面板全量管理，
	// 每次 apply 都是先删表再重建，所以不会和别的工具打架。
	TableName = "vps_forward"
	// TableFamily 用 inet 而不是 ip，这样一张表同时管 IPv4 和 IPv6。
	TableFamily = "inet"

	ModeKernel    = "kernel"
	ModeUserspace = "userspace"

	ProtoTCP  = "tcp"
	ProtoUDP  = "udp"
	ProtoBoth = "tcp+udp"
)

// Rule 是数据面认识的一条转发规则，也是面板下发给探针的线上格式。
//
// 注意 HopID 是**跳**的 ID 而不是规则 ID：一条多跳规则在每台机器上各占一跳，
// 每一跳独立监听、独立计数。用它做计数的主键，规则删了再建同端口也不会串账。
type Rule struct {
	// HopID 由面板分配，探针原样带回计数上报里。
	HopID int64 `json:"hop_id"`

	Proto      string `json:"proto"`
	ListenPort int    `json:"listen_port"`

	// DestHost 和 DestIP 二选一或同时存在：填了域名就由探针定期重解析，
	// 解析结果写进 DestIP。两个都有时 DestIP 是当前生效的解析值。
	DestHost string `json:"dest_host,omitempty"`
	DestIP   string `json:"dest_ip,omitempty"`
	DestPort int    `json:"dest_port"`

	// Mode 空串等价于 ModeKernel —— 见 EffectiveMode。
	Mode string `json:"mode,omitempty"`

	// BandwidthMbps 是这条规则的限速，0 表示不限。
	// 内核态靠 nft 打 fwmark + tc HTB 分类，用户态靠令牌桶，两边都是
	// 上下行**共享**一个桶：中转的两个方向都是本机出方向，tc 只能限出方向，
	// 用户态跟着对齐，免得同一个数字在两种模式下含义不一样。
	BandwidthMbps int `json:"bandwidth_mbps,omitempty"`

	// Label 只用于日志和 nft 注释，数据面逻辑一律不读它。
	Label string `json:"label,omitempty"`
}

// EffectiveMode 归一化转发模式。空串或无法识别的值一律当内核态，
// 这样老探针的 state 文件、老面板的下发（没有 mode 字段）都退回原有行为。
// 这是「默认值」的唯一出处，别的地方不要再判空。
func (r Rule) EffectiveMode() string {
	if r.Mode == ModeUserspace {
		return ModeUserspace
	}
	return ModeKernel
}

// ShapeMark 返回这条规则的 fwmark，不限速时返回 0。
//
// 0x10000 的偏移是为了和别的组件划清界限：监听端口不会超过 0xFFFF，
// 所以高 16 位是 0x0001 的 mark 一定是我们打的。restore_mark 链只恢复
// 这个区间内的 mark，不会去抢别人（策略路由、其他 QoS）设的 ct mark。
func (r Rule) ShapeMark() uint32 {
	if r.BandwidthMbps <= 0 {
		return 0
	}
	return uint32(0x10000 | r.ListenPort)
}

// Target 返回这条规则当前生效的目标地址。DestIP 为空（域名还没解析出来）时返回空串。
func (r Rule) Target() string {
	if r.DestIP == "" {
		return ""
	}
	return net.JoinHostPort(r.DestIP, fmt.Sprintf("%d", r.DestPort))
}

// Display 是给日志和事件用的一行描述。
func (r Rule) Display() string {
	dest := r.DestIP
	if r.DestHost != "" {
		if r.DestIP != "" {
			dest = fmt.Sprintf("%s(%s)", r.DestHost, r.DestIP)
		} else {
			dest = r.DestHost
		}
	}
	s := fmt.Sprintf("%s/%d → %s:%d [%s]",
		strings.ToUpper(r.Proto), r.ListenPort, dest, r.DestPort, r.EffectiveMode())
	if r.BandwidthMbps > 0 {
		s += fmt.Sprintf(" 限速 %dMbps", r.BandwidthMbps)
	}
	return s
}

// Validate 校验一条规则。错误消息面向用户，会原样显示在面板上。
func Validate(r Rule) error {
	switch r.Proto {
	case ProtoTCP, ProtoUDP, ProtoBoth:
	default:
		return fmt.Errorf("协议必须是 tcp、udp 或 tcp+udp，收到 %q", r.Proto)
	}
	if r.ListenPort < 1 || r.ListenPort > 65535 {
		return fmt.Errorf("监听端口 %d 不合法，应在 1-65535 之间", r.ListenPort)
	}
	if r.DestPort < 1 || r.DestPort > 65535 {
		return fmt.Errorf("目标端口 %d 不合法，应在 1-65535 之间", r.DestPort)
	}
	if r.DestHost == "" && r.DestIP == "" {
		return fmt.Errorf("目标必须填 IP 或域名")
	}
	if r.DestIP != "" && net.ParseIP(r.DestIP) == nil {
		return fmt.Errorf("目标 IP %q 格式非法", r.DestIP)
	}
	if r.DestHost != "" && !resolver.PlausibleHostname(r.DestHost) {
		return fmt.Errorf("目标域名 %q 格式非法", r.DestHost)
	}
	switch r.Mode {
	case "", ModeKernel, ModeUserspace:
	default:
		return fmt.Errorf("转发模式必须是 kernel 或 userspace，收到 %q", r.Mode)
	}
	if r.Mode == ModeUserspace && r.Proto == ProtoUDP {
		return fmt.Errorf("UDP 不支持用户态转发，请改用内核态")
	}
	if r.BandwidthMbps < 0 {
		return fmt.Errorf("限速 %d 不合法，应为正整数或 0（不限）", r.BandwidthMbps)
	}
	return nil
}

// IsIPv6 判断地址是不是 IPv6 字面量。
func IsIPv6(addr string) bool {
	ip := net.ParseIP(addr)
	return ip != nil && ip.To4() == nil
}

// IsLoopback 判断目标是不是本机回环。回环目标要走 redirect / input 链，
// 和普通转发的 DNAT / forward 链是两套写法，渲染和计数都得分开处理。
func IsLoopback(addr string) bool {
	ip := net.ParseIP(addr)
	return ip != nil && ip.IsLoopback()
}

// splitProtos 把 tcp+udp 展开成具体协议。渲染计数链时要按具体协议逐条写，
// 因为 nftables 没法在 l4proto 集合下给 ct proto-dst 定类型，
// 老版本 nft 会把端口序列化成无类型的十六进制，计数就再也读不回来了。
func splitProtos(proto string) []string {
	if proto == ProtoBoth {
		return []string{ProtoTCP, ProtoUDP}
	}
	return []string{proto}
}
