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
	// ActionCDTStop 通过关联的阿里云 CDT 实例做节省关机（StopCharging）。
	//
	// 比 SSH / 探针关机强的地方有两点：停机之后不再收实例费；
	// 走的是阿里云 API，探针死了、SSH 连不上也照样能关。
	// 前提是这个节点关联了 CDT 实例。
	ActionCDTStop = "cdt_stop"
)

// 节点状态。
const (
	StatusUnknown  = "unknown"
	StatusOnline   = "online"
	StatusOffline  = "offline"
	StatusExceeded = "exceeded" // 流量超额（可能已被关机）
	StatusStopped  = "stopped"  // 被面板主动关停
	// StatusPlannedStop 是「计划内停机」：面板通过阿里云 CDT 按计划把它停了
	// （定时关机、流量熔断），不是故障。
	//
	// 单独一个状态而不是复用 stopped，是因为两者的后续处理不一样：
	// stopped 是「超额动作真把机器关了」，机器要人去服务商后台开；
	// planned_stop 是面板自己停的，面板自己会在该开的时候开回来。
	// 而且它明确不该触发掉线告警和域名切换 —— 那是把计划当成事故。
	StatusPlannedStop = "planned_stop"
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

	// CDTInstanceID 关联到哪个阿里云 CDT 实例（cdt_instances.id），0 = 未关联。
	//
	// 关联之后：CDT 按计划停这台机器时，面板知道那是计划内的，
	// 不发掉线告警、默认也不切域名；节点流量跑满时还可以走 CDT 的节省关机。
	CDTInstanceID int64 `json:"cdt_instance_id"`

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
	ID         int64  `json:"id"`
	ProviderID int64  `json:"provider_id"`
	Zone       string `json:"zone"`
	Name       string `json:"name"`
	RecordType string `json:"record_type"`
	TTL        int    `json:"ttl"`
	Proxied    bool   `json:"proxied"`
	Strategy   string `json:"strategy"`

	SwitchOnExceed  bool `json:"switch_on_exceed"`
	SwitchOnOffline bool `json:"switch_on_offline"`
	SwitchOnWarn    bool `json:"switch_on_warn"`
	// SwitchOnPlannedStop 决定「计划内停机」算不算切换理由。
	//
	// 默认关：计划内停机是面板自己安排的，不该当成故障去切解析。
	// 但代价要知道 —— 不切的话，定时关机期间域名会一直指着一台关着的机器。
	// 想让它照常切走就打开这个。
	SwitchOnPlannedStop bool `json:"switch_on_planned_stop"`

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

// --- 端口转发 ---

// 转发协议。
const (
	ForwardProtoTCP  = "tcp"
	ForwardProtoUDP  = "udp"
	ForwardProtoBoth = "tcp+udp"
)

// 转发模式。
const (
	// ForwardModeKernel 是 nftables DNAT，零拷贝，TCP/UDP 全支持。
	ForwardModeKernel = "kernel"
	// ForwardModeUserspace 是探针内嵌的 TCP relay：每跳单独建连接，
	// 规避多跳串联时 TCP-over-TCP 的拥塞叠加，代价是只支持 TCP。
	ForwardModeUserspace = "userspace"
)

// ForwardNode 是节点的转发相关配置，和 nodes 表分开存。
type ForwardNode struct {
	NodeID int64 `json:"node_id"`
	// RelayHost 是其他节点访问本节点用的地址。留空时回落 nodes.ipv4。
	RelayHost   string    `json:"relay_host"`
	RelayHostV6 string    `json:"relay_host_v6"`
	PortStart   int       `json:"port_start"`
	PortEnd     int       `json:"port_end"`
	Enabled     bool      `json:"enabled"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ForwardRule 是一条逻辑转发规则：从入口一路转到最终目标。
type ForwardRule struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Proto     string    `json:"proto"`
	DestHost  string    `json:"dest_host"`
	DestPort  int       `json:"dest_port"`
	Enabled   bool      `json:"enabled"`
	Remark    string    `json:"remark"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Hops 只在查询时填充，不是数据库列。按 position 升序。
	Hops []ForwardHop `json:"hops,omitempty"`
}

// ForwardHop 是规则在某台机器上的一跳。Position 从 0 开始，0 即入口。
type ForwardHop struct {
	ID         int64 `json:"id"`
	RuleID     int64 `json:"rule_id"`
	Position   int   `json:"position"`
	NodeID     int64 `json:"node_id"`
	ListenPort int   `json:"listen_port"`
	// Proto 是从规则冗余下来的，用于支撑 (node_id, listen_port, proto) 唯一约束。
	Proto         string `json:"proto"`
	Mode          string `json:"mode"`
	BandwidthMbps int    `json:"bandwidth_mbps"`

	// NodeName 只在查询时填充，不是数据库列。
	NodeName string `json:"node_name,omitempty"`
}

// ForwardCounter 是探针上报的一跳累计计数快照。
// Epoch 变化即代表探针侧计数器已归零，作用等同于 Counter 里的 BootID。
type ForwardCounter struct {
	HopID     int64
	Epoch     string
	LastUp    int64
	LastDown  int64
	UpdatedAt time.Time
}

// ForwardUsage 是一跳在当前账单周期内的累计用量。
//
// 注意：这份用量**不参与节点配额判定**。中转流量在网卡上进出各走一遍，
// 已经被节点账本算进去了，这里只用于分账展示。
type ForwardUsage struct {
	HopID      int64     `json:"hop_id"`
	CycleStart time.Time `json:"cycle_start"`
	UpBytes    int64     `json:"up_bytes"`
	DownBytes  int64     `json:"down_bytes"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ForwardHourlyPoint 是按小时聚合的转发流量增量。
type ForwardHourlyPoint struct {
	HourTS    time.Time `json:"hour_ts"`
	UpDelta   int64     `json:"up_delta"`
	DownDelta int64     `json:"down_delta"`
}

// --- 阿里云 CDT ---

// 阿里云站点。只影响账单接口的域名和记账货币。
const (
	CDTSiteChina         = "china"
	CDTSiteInternational = "international"
)

// 停机模式。
const (
	// CDTStopCharging 是节省模式：停机后不再收实例费，代价是公网 IP 可能变。
	CDTStopCharging = "StopCharging"
	// CDTStopKeepCharging 是普通停机：继续计费、保留资源和 IP。
	CDTStopKeepCharging = "KeepCharging"
)

// CDTAccount 是一组阿里云访问凭据加上它的守护策略。
//
// 这个结构体会直接 JSON 序列化给前端，所以**凭据密文不在里面**——
// 它单独用 Store.CDTAccountCred 取。
type CDTAccount struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	AccessKeyID string `json:"access_key_id"`
	RegionID    string `json:"region_id"`
	SiteType    string `json:"site_type"`

	// 两个免费额度池，单位 GB。阿里云就是分开算的，不是一个总额度。
	QuotaMainlandGB  float64 `json:"quota_mainland_gb"`
	QuotaOverseasGB  float64 `json:"quota_overseas_gb"`
	ThresholdPercent float64 `json:"threshold_percent"`
	// OutstandingThreshold 是待还金额熔断线，0 表示不启用。
	OutstandingThreshold float64 `json:"outstanding_threshold"`
	ShutdownMode         string  `json:"shutdown_mode"`

	KeepAlive     bool   `json:"keep_alive"`
	AutoStartTime string `json:"auto_start_time"`
	AutoStopTime  string `json:"auto_stop_time"`
	ScheduleTZ    string `json:"schedule_tz"`

	// SyncIntervalSec 是多久去阿里云查一次（秒）。流量、账单、实例状态、
	// 抢占式实例保活全都按它走。0 表示用默认值。
	//
	// 定时开关机**不受它影响**：那个是按墙上时钟的 HH:MM 触发的，
	// 检查间隔拉长会直接错过时间点，所以它固定一分钟看一次。
	SyncIntervalSec int `json:"sync_interval_sec"`

	// TrippedAt 非零表示这个账号已被面板熔断停机。
	TrippedAt       time.Time `json:"tripped_at"`
	TrippedReason   string    `json:"tripped_reason"`
	TrippedCycle    string    `json:"tripped_cycle"`
	NoStockNotified bool      `json:"nostock_notified"`

	LastSyncAt time.Time `json:"last_sync_at"`
	LastError  string    `json:"last_error"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Tripped 判断这个账号当前是不是处于熔断停机状态。
func (a *CDTAccount) Tripped() bool { return !a.TrippedAt.IsZero() }

// CDT 同步间隔的默认值与边界。
const (
	DefaultCDTSyncIntervalSec = 300
	// MinCDTSyncIntervalSec 是个防手滑的下限。每轮同步要打 3~4 次阿里云接口，
	// 设成一两秒会很快撞上人家的调用频率限制，还会把账号的 API 配额吃光。
	MinCDTSyncIntervalSec = 10
	MaxCDTSyncIntervalSec = 86400
)

// SyncInterval 返回这个账号实际生效的同步间隔，顺手做边界兜底。
func (a *CDTAccount) SyncInterval() time.Duration {
	sec := a.SyncIntervalSec
	if sec <= 0 {
		sec = DefaultCDTSyncIntervalSec
	}
	if sec < MinCDTSyncIntervalSec {
		sec = MinCDTSyncIntervalSec
	}
	if sec > MaxCDTSyncIntervalSec {
		sec = MaxCDTSyncIntervalSec
	}
	return time.Duration(sec) * time.Second
}

// CDTInstance 是一台 ECS 实例的快照。
type CDTInstance struct {
	ID           int64  `json:"id"`
	AccountID    int64  `json:"account_id"`
	InstanceID   string `json:"instance_id"`
	InstanceName string `json:"instance_name"`
	RegionID     string `json:"region_id"`
	ZoneID       string `json:"zone_id"`
	Status       string `json:"status"`
	PublicIP     string `json:"public_ip"`
	InstanceType string `json:"instance_type"`

	BandwidthMbps int  `json:"bandwidth_mbps"`
	IsSpot        bool `json:"is_spot"`
	// Guarded 表示这台实例受熔断 / 保活 / 定时开关机管辖。
	// 不打这个标记的实例，面板只看不动。
	Guarded bool `json:"guarded"`
	// PlannedStop 表示这台实例是**面板主动停的**（定时关机 / 流量熔断 / 手动停机）。
	//
	// 它存在的唯一理由是让保活别和定时关机打架：保活的职责是「被阿里云回收了
	// 就拉起来」，而不是「只要停着就拉起来」。少了这个标记，定时关机刚把机器停掉，
	// 保活下一轮就给拉回来了 —— 两个功能互相拆台。
	//
	// 面板主动开机（定时开机 / 账期恢复 / 手动开机），或观察到实例确实经历了
	// Stopped / Starting → Running 的启动过程时清掉。单独看到 Running 不够：
	// 关机刚下发时云端列表接口可能还会短暂返回旧状态。
	PlannedStop bool `json:"planned_stop"`

	LastSynced time.Time `json:"last_synced"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// CDTTraffic 是某账号某账期在一个业务地域上的出方向流量。
type CDTTraffic struct {
	AccountID        int64     `json:"account_id"`
	Cycle            string    `json:"cycle"`
	BusinessRegionID string    `json:"business_region_id"`
	TrafficType      string    `json:"traffic_type"`
	TrafficBytes     int64     `json:"traffic_bytes"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// CDTBill 是某账号某账期的余额与待还金额快照。
type CDTBill struct {
	AccountID       int64     `json:"account_id"`
	Cycle           string    `json:"cycle"`
	AvailableAmount float64   `json:"available_amount"`
	Outstanding     float64   `json:"outstanding"`
	Currency        string    `json:"currency"`
	Symbol          string    `json:"symbol"`
	UpdatedAt       time.Time `json:"updated_at"`
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
