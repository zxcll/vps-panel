// Package protocol 定义面板与探针之间的报文格式。
// 面板和探针都编译自这个包，改协议只需改一处。
package protocol

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
}

// 帧类型。
const (
	FrameReport  = "report"  // 探针 → 面板：流量快照
	FrameAck     = "ack"     // 面板 → 探针：收到
	FrameCommand = "command" // 面板 → 探针：下发指令
	FrameResult  = "result"  // 探针 → 面板：指令执行结果
	FramePing    = "ping"    // 面板 → 探针：保活
	FramePong    = "pong"    // 探针 → 面板：保活应答
)

// Frame 是 WebSocket 上的通用信封。
type Frame struct {
	Type    string         `json:"type"`
	Report  *Report        `json:"report,omitempty"`
	Command *Command       `json:"command,omitempty"`
	Result  *CommandResult `json:"result,omitempty"`
	Message string         `json:"message,omitempty"`
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
