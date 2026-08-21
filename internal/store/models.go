package store

import "time"

// 计费口径。用户所说的"单向流量"= BillingMax，取出站/入站中较大的那个。
const (
	BillingSum = "sum" // 双向：rx + tx
	BillingMax = "max" // 单向：max(rx, tx)
	BillingOut = "out" // 仅出站
	BillingIn  = "in"  // 仅入站
)

// 超额动作。
const (
	ActionNone          = "none"           // 什么都不做
	ActionDNSOnly       = "dns_only"       // 只切解析 + 告警，不动机器
	ActionShutdownAgent = "shutdown_agent" // 探针本地关机
	ActionShutdownSSH   = "shutdown_ssh"   // 面板通过 SSH 远程关机
	ActionCommand       = "command"        // 执行自定义命令（如停服务），不关机
)

// 节点状态。
const (
	StatusUnknown  = "unknown"
	StatusOnline   = "online"
	StatusOffline  = "offline"
	StatusExceeded = "exceeded" // 流量超额（可能已被关机）
	StatusStopped  = "stopped"  // 被面板主动关停
)

type Node struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Remark string `json:"remark"`
	Secret string `json:"secret"`
	IPv4   string `json:"ipv4"`
	IPv6   string `json:"ipv6"`

	QuotaBytes   int64   `json:"quota_bytes"`
	BillingMode  string  `json:"billing_mode"`
	TrafficRatio float64 `json:"traffic_ratio"`
	WarnPercent  int     `json:"warn_percent"`

	ResetDay   int       `json:"reset_day"`
	ResetTZ    string    `json:"reset_tz"`
	CycleStart time.Time `json:"cycle_start"`
	CycleEnd   time.Time `json:"cycle_end"`

	ActionOnExceed     string `json:"action_on_exceed"`
	ExceedCommand      string `json:"exceed_command"`
	AutoRecoverOnReset bool   `json:"auto_recover_on_reset"`

	SSHHost       string `json:"ssh_host"`
	SSHPort       int    `json:"ssh_port"`
	SSHUser       string `json:"ssh_user"`
	SSHAuth       string `json:"ssh_auth"`
	SSHSecretEnc  []byte `json:"-"`
	SSHKeyPassEnc []byte `json:"-"`
	SSHHostKey    string `json:"ssh_host_key"`
	SSHUseSudo    bool   `json:"ssh_use_sudo"`

	ProbePort int `json:"probe_port"`

	Status          string     `json:"status"`
	LastSeen        *time.Time `json:"last_seen"`
	Enabled         bool       `json:"enabled"`
	ExceedHandledAt *time.Time `json:"exceed_handled_at"`
	WarnHandledAt   *time.Time `json:"warn_handled_at"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// HasSSH 判断这个节点是否配全了走 SSH 的必要信息。
func (n *Node) HasSSH() bool {
	return n.SSHHost != "" && n.SSHUser != "" && len(n.SSHSecretEnc) > 0
}

// EffectiveProbePort 返回 TCP 拨测用的端口：优先 probe_port，其次 ssh_port。
func (n *Node) EffectiveProbePort() int {
	if n.ProbePort > 0 {
		return n.ProbePort
	}
	if n.SSHPort > 0 {
		return n.SSHPort
	}
	return 22
}

// ProbeHost 返回拨测目标地址：优先 ipv4，其次 ssh_host。
func (n *Node) ProbeHost() string {
	if n.IPv4 != "" {
		return n.IPv4
	}
	return n.SSHHost
}

// Counter 是探针上报的网卡累计计数器快照。
type Counter struct {
	NodeID    int64
	BootID    string
	Iface     string
	LastRx    int64
	LastTx    int64
	UpdatedAt time.Time
}

// Usage 是当前账单周期的累计用量（面板账本）。
type Usage struct {
	NodeID     int64     `json:"node_id"`
	CycleStart time.Time `json:"cycle_start"`
	RxBytes    int64     `json:"rx_bytes"`
	TxBytes    int64     `json:"tx_bytes"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// HourlyPoint 是按小时聚合的流量增量，用于画曲线。
type HourlyPoint struct {
	HourTS  time.Time `json:"hour_ts"`
	RxDelta int64     `json:"rx_delta"`
	TxDelta int64     `json:"tx_delta"`
}

// CycleRecord 是归档的历史账单周期。
type CycleRecord struct {
	ID          int64     `json:"id"`
	NodeID      int64     `json:"node_id"`
	CycleStart  time.Time `json:"cycle_start"`
	CycleEnd    time.Time `json:"cycle_end"`
	RxBytes     int64     `json:"rx_bytes"`
	TxBytes     int64     `json:"tx_bytes"`
	BilledBytes int64     `json:"billed_bytes"`
	QuotaBytes  int64     `json:"quota_bytes"`
	BillingMode string    `json:"billing_mode"`
	ArchivedAt  time.Time `json:"archived_at"`
}

// DNS 服务商类型。
const (
	ProviderCloudflare = "cloudflare"
	ProviderDNSPod     = "dnspod"
	ProviderAlidns     = "alidns"
)

type DNSProvider struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	CredEnc   []byte    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// 切换策略。
const (
	StrategyFailover = "failover" // 按优先级自动选主
	StrategyManual   = "manual"   // 只接受手动切换
)

type DNSRecord struct {
	ID          int64  `json:"id"`
	ProviderID  int64  `json:"provider_id"`
	Zone        string `json:"zone"`
	Name        string `json:"name"`
	RecordType  string `json:"record_type"`
	TTL         int    `json:"ttl"`
	Proxied     bool   `json:"proxied"`
	Strategy    string `json:"strategy"`

	SwitchOnExceed  bool `json:"switch_on_exceed"`
	SwitchOnOffline bool `json:"switch_on_offline"`
	SwitchOnWarn    bool `json:"switch_on_warn"`

	CurrentNodeID *int64     `json:"current_node_id"`
	CurrentValue  string     `json:"current_value"`
	LastSwitchAt  *time.Time `json:"last_switch_at"`
	LastError     string     `json:"last_error"`
	Enabled       bool       `json:"enabled"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`

	// Members 只在查询时填充，不是数据库列。
	Members []DNSMember `json:"members,omitempty"`
}

type DNSMember struct {
	RecordID int64  `json:"record_id"`
	NodeID   int64  `json:"node_id"`
	Priority int    `json:"priority"`
	NodeName string `json:"node_name,omitempty"`
}

// 事件级别。
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

type Event struct {
	ID        int64     `json:"id"`
	NodeID    *int64    `json:"node_id"`
	Type      string    `json:"type"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}
