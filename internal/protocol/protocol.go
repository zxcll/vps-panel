// Package protocol 定义面板与探针之间的报文格式。
// 面板和探针都编译自这个包，改协议只需改一处。
//
// 转发规则的线上格式直接复用 forward.Rule（数据面本来就要按那个结构干活），
// 不再抄一份平行的结构体 —— 两份定义迟早会漂移，而漂移的表现是
// "面板下发了但探针没生效"，极难排查。
package protocol

import "github.com/zxcll/vps-panel/internal/forward"

// Version 是协议版本。探针与面板大版本不一致时，面板会在事件日志里提示。
const Version = 1

// HeaderSecret 是探针鉴权用的 HTTP 头。
const HeaderSecret = "X-Node-Secret"

// Report 是探针上报的一次快照。
//
// 关键点：Rx/Tx 是网卡的**累计**计数器原值，不是增量。
// 面板负责做差值累加——这样即使探针重启、面板重启、报文重复，账本也不会错乱。
type Report struct {
	// BootID 取自 /proc/sys/kernel/random/boot_id，机器重启后必变。
	// 面板靠它识别"网卡计数器已归零"。
	BootID string `json:"boot_id"`
	// Iface 是被统计的网卡名。
	Iface string `json:"iface"`
	// Rx/Tx 是该网卡自开机以来的累计收发字节。
	Rx int64 `json:"rx"`
	Tx int64 `json:"tx"`

	// 以下是展示用的机器状态，不参与计费。
	Uptime     int64   `json:"uptime"`      // 秒
	Load1      float64 `json:"load1"`       // 1 分钟负载
	CPUPercent float64 `json:"cpu_percent"` // 0~100
	MemTotal   int64   `json:"mem_total"`
	MemUsed    int64   `json:"mem_used"`
	DiskTotal  int64   `json:"disk_total"`
	DiskUsed   int64   `json:"disk_used"`

	// TS 是探针本地时间（Unix 秒），仅供排查时钟偏移，账本一律用面板时间。
	TS int64 `json:"ts"`
	// AgentVersion 便于面板提示探针需要升级。
	AgentVersion string `json:"agent_version"`
	// ProtoVersion 是协议版本。
	ProtoVersion int `json:"proto_version"`
	// Final 标记这是关机/退出前的最后一次上报，用来把最后一个上报间隔内的流量补上。
	Final bool `json:"final"`

	// Forwards 是各条转发规则的累计计数，没配转发时为空。
	// 老探针不会带这个字段，面板按空处理即可，不能因此报错。
	Forwards []ForwardSample `json:"forwards,omitempty"`
}

// ForwardSample 是一跳转发的累计流量计数。
//
// 和 Report.Rx/Tx 一样，这里是**累计值**不是增量，面板负责做差值累加。
// Epoch 在这里扮演 BootID 的角色：探针的转发状态文件丢失或重建时会换一个新值，
// 面板据此判断"计数器已归零"，把本次读数整个计入。
//
// 计数按 HopID 索引而不是端口：规则删掉再建同一个端口是新的 HopID，
// 计数从头开始，不会把两条规则的流量串到一起。
//
// 重要：转发计数**不进节点配额账本**。节点账本只认网卡计数器，
// 转发流量在网卡上进出各一遍本来就已经算进去了；这里的数字只用于分账展示。
type ForwardSample struct {
	HopID     int64  `json:"hop_id"`
	Epoch     string `json:"epoch"`
	BytesUp   int64  `json:"up"`
	BytesDown int64  `json:"down"`
}

// 帧类型。
const (
	FrameReport  = "report"  // 探针 → 面板：流量快照
	FrameAck     = "ack"     // 面板 → 探针：收到
	FrameCommand = "command" // 面板 → 探针：下发指令
	FrameResult  = "result"  // 探针 → 面板：指令执行结果
	FramePing    = "ping"    // 面板 → 探针：保活
	FramePong    = "pong"    // 探针 → 面板：保活应答

	FrameApplyRuleset = "apply_ruleset" // 面板 → 探针：下发该节点的完整转发规则集
	FrameApplyAck     = "apply_ack"     // 探针 → 面板：规则集落地结果
)

// ApplyRuleset 是一个节点的**完整**转发规则集，全量覆盖语义：
// 探针收到后让本机状态与之完全一致，没在里面的规则一律撤掉。
//
// 之所以按节点全量下发而不是逐条增删：nftables 本来就是删表重建，
// 全量语义天然幂等，探针重连、面板重启、漏了一条指令都能靠下一次下发自愈。
type ApplyRuleset struct {
	// Rev 是面板给这一版规则集的编号，探针在 ack 里原样带回，用于对上号。
	Rev   string         `json:"rev"`
	Rules []forward.Rule `json:"rules"`
}

// ApplyAck 是探针对 ApplyRuleset 的回执。
// OK 为真时 Error 必须为空，反之亦然 —— OK 是判定用的信号，Error 只是给人看的原因。
type ApplyAck struct {
	Rev   string `json:"rev"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Warning 用于"规则生效了但有隐患"的情况，比如没检测到 nftables、
	// 或者本机防火墙可能拦截。不影响 OK。
	Warning string `json:"warning,omitempty"`
}

// Frame 是 WebSocket 上的通用信封。
type Frame struct {
	Type    string         `json:"type"`
	Report  *Report        `json:"report,omitempty"`
	Command *Command       `json:"command,omitempty"`
	Result  *CommandResult `json:"result,omitempty"`
	Message string         `json:"message,omitempty"`

	ApplyRuleset *ApplyRuleset `json:"apply_ruleset,omitempty"`
	ApplyAck     *ApplyAck     `json:"apply_ack,omitempty"`
}

// 指令类型。
const (
	CmdShutdown = "shutdown" // 关机
	CmdExec     = "exec"     // 执行自定义命令（如停止代理服务）
	CmdReport   = "report"   // 要求立即上报一次
)

type Command struct {
	ID         string `json:"id"`
	Cmd        string `json:"cmd"`
	Script     string `json:"script,omitempty"`
	TimeoutSec int    `json:"timeout_sec,omitempty"`
	// Reason 会写进探针日志，方便事后查"这台机器为什么被关了"。
	Reason string `json:"reason,omitempty"`
}

type CommandResult struct {
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// AckPayload 是面板对上报的应答，顺带把服务端认定的用量回传给探针，
// 探针可以打印出来，方便在节点上直接看到"面板认为我用了多少"。
type AckPayload struct {
	BilledBytes int64  `json:"billed_bytes"`
	QuotaBytes  int64  `json:"quota_bytes"`
	Status      string `json:"status"`
}
