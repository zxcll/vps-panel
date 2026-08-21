// Package agent 是节点探针。
//
// 职责刻意压得很窄：定期把网卡累计计数器原样报给面板，执行面板下发的指令。
// 它不记账、不判断配额、不做任何决策——这些都在面板端。
// 好处是探针重启、崩溃、被杀，都不会让流量账本出错。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/zxcll/vps-panel/internal/agent/collector"
	"github.com/zxcll/vps-panel/internal/protocol"
)

// Version 是探针版本，上报给面板用于提示升级。
const Version = "1.0.0"

type Config struct {
	// Server 是面板地址，支持 wss:// ws:// https:// http:// 四种写法。
	Server string
	Secret string

	// Iface 指定统计哪块网卡，留空自动识别默认路由网卡。
	Iface     string
	AllIfaces bool

	// ReportInterval 是上报间隔。这个值决定了"机器突然断电时最多丢多少流量"，
	// 30 秒是流量精度和请求量之间的折中。
	ReportInterval time.Duration

	// Mode: auto（优先 WebSocket，失败降级 HTTP）/ ws / http
	Mode string

	// AllowExec 决定是否接受面板下发的自定义命令。
	// 关掉它就只允许关机指令，适合不完全信任面板的场景。
	AllowExec bool

	// FakeTraffic 是调试用的伪造流量速率（如 "50MB/s"）。
	// 本地端到端测试时不用真的跑满带宽就能验证配额和切换逻辑。
	FakeTraffic string

	Log *slog.Logger
}

const (
	ModeAuto = "auto"
	ModeWS   = "ws"
	ModeHTTP = "http"
)

type Agent struct {
	cfg  Config
	col  *collector.Collector
	log  *slog.Logger
	http *http.Client

	fake *fakeTraffic

	mu       sync.Mutex
	lastSnap collector.Snapshot
	haveSnap bool

	// flush 用来在关机前触发一次立即上报
	flush chan struct{}
}

func New(cfg Config) (*Agent, error) {
	if cfg.Secret == "" {
		return nil, errors.New("缺少节点密钥（--secret 或 VPS_AGENT_SECRET）")
	}
	if cfg.Server == "" {
		return nil, errors.New("缺少面板地址（--server 或 VPS_AGENT_SERVER）")
	}
	if cfg.ReportInterval <= 0 {
		cfg.ReportInterval = 30 * time.Second
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeAuto
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	a := &Agent{
		cfg:   cfg,
		col:   collector.New(cfg.Iface, cfg.AllIfaces),
		log:   cfg.Log,
		http:  &http.Client{Timeout: 20 * time.Second},
		flush: make(chan struct{}, 1),
	}

	if cfg.FakeTraffic != "" {
		f, err := newFakeTraffic(cfg.FakeTraffic, a.col.BootID())
		if err != nil {
			return nil, err
		}
		a.fake = f
		a.log.Warn("已启用伪造流量模式，上报的不是真实网卡数据", "rate", cfg.FakeTraffic)
	}

	return a, nil
}

// --- 地址推导 ---

func (a *Agent) wsEndpoint() string {
	s := strings.TrimRight(a.cfg.Server, "/")
	switch {
	case strings.HasPrefix(s, "https://"):
		s = "wss://" + strings.TrimPrefix(s, "https://")
	case strings.HasPrefix(s, "http://"):
		s = "ws://" + strings.TrimPrefix(s, "http://")
	case !strings.HasPrefix(s, "ws://") && !strings.HasPrefix(s, "wss://"):
		s = "wss://" + s
	}
	return s + "/agent/ws"
}

func (a *Agent) httpEndpoint() string {
	s := strings.TrimRight(a.cfg.Server, "/")
	switch {
	case strings.HasPrefix(s, "wss://"):
		s = "https://" + strings.TrimPrefix(s, "wss://")
	case strings.HasPrefix(s, "ws://"):
		s = "http://" + strings.TrimPrefix(s, "ws://")
	case !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://"):
		s = "https://" + s
	}
	return s + "/agent/report"
}

// --- 采集 ---

func (a *Agent) snapshot() (collector.Snapshot, error) {
	if a.fake != nil {
		return a.fake.snapshot(), nil
	}
	s, err := a.col.Collect()
	if err != nil {
		return s, err
	}
	a.mu.Lock()
	a.lastSnap, a.haveSnap = s, true
	a.mu.Unlock()
	return s, nil
}

func (a *Agent) buildReport(final bool) (protocol.Report, error) {
	s, err := a.snapshot()
	if err != nil {
		return protocol.Report{}, err
	}
	return protocol.Report{
		BootID:       s.BootID,
		Iface:        s.Iface,
		Rx:           s.Rx,
		Tx:           s.Tx,
		Uptime:       s.Uptime,
		Load1:        s.Load1,
		CPUPercent:   s.CPUPercent,
		MemTotal:     s.MemTotal,
		MemUsed:      s.MemUsed,
		DiskTotal:    s.DiskTotal,
		DiskUsed:     s.DiskUsed,
		TS:           time.Now().Unix(),
		AgentVersion: Version,
		ProtoVersion: protocol.Version,
		Final:        final,
	}, nil
}

// TriggerFlush 请求立刻上报一次。关机前调用，尽量把最后一段流量补上。
func (a *Agent) TriggerFlush() {
	select {
	case a.flush <- struct{}{}:
	default:
	}
}

// --- 主循环 ---

// Run 一直跑到 ctx 取消。
func (a *Agent) Run(ctx context.Context) error {
	a.log.Info("探针启动",
		"面板", a.cfg.Server, "上报间隔", a.cfg.ReportInterval,
		"模式", a.cfg.Mode, "版本", Version)

	// 先采一次，把网卡识别结果打出来，配错了能立刻发现
	if s, err := a.snapshot(); err != nil {
		a.log.Error("首次采集失败", "err", err)
	} else {
		a.log.Info("流量统计口径已确定",
			"网卡", s.Iface, "当前累计入站", s.Rx, "当前累计出站", s.Tx, "boot_id", s.BootID)
	}

	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			a.finalFlush()
			return nil
		default:
		}

		var err error
		switch a.cfg.Mode {
		case ModeHTTP:
			err = a.runHTTP(ctx)
		case ModeWS:
			err = a.runWS(ctx)
		default:
			err = a.runWS(ctx)
			if err != nil && ctx.Err() == nil {
				// WebSocket 连不上（面板前面的反代可能没开 Upgrade），
				// 降级用 HTTP 顶一阵子，之后再试 WebSocket
				a.log.Warn("WebSocket 不可用，暂时降级为 HTTP 上报", "err", err)
				httpCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				err = a.runHTTP(httpCtx)
				cancel()
				if ctx.Err() == nil {
					continue
				}
			}
		}

		if ctx.Err() != nil {
			a.finalFlush()
			return nil
		}
		if err != nil {
			a.log.Warn("与面板的连接中断，准备重连", "err", err, "等待", backoff)
		}

		select {
		case <-ctx.Done():
			a.finalFlush()
			return nil
		case <-time.After(backoff):
		}

		// 指数退避，封顶 60 秒：面板重启或网络长时间不通时别把日志刷爆
		backoff *= 2
		if backoff > 60*time.Second {
			backoff = 60 * time.Second
		}
	}
}

// finalFlush 在退出前尽最后努力报一次。
// 面板做的是差值累加，这一次上报能把"上次上报到现在"的流量补进账本。
func (a *Agent) finalFlush() {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	rep, err := a.buildReport(true)
	if err != nil {
		return
	}
	if err := a.postReport(ctx, rep); err != nil {
		a.log.Warn("退出前的最后一次上报失败，最多丢失一个上报周期的流量", "err", err)
		return
	}
	a.log.Info("退出前已完成最后一次上报")
}

// --- HTTP 通道 ---

func (a *Agent) runHTTP(ctx context.Context) error {
	t := time.NewTicker(a.cfg.ReportInterval)
	defer t.Stop()

	// 立刻报一次，别让面板等一个完整周期才看到节点上线
	if err := a.reportOnce(ctx); err != nil {
		a.log.Warn("上报失败", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-a.flush:
			if err := a.reportOnce(ctx); err != nil {
				a.log.Warn("上报失败", "err", err)
			}
		case <-t.C:
			if err := a.reportOnce(ctx); err != nil {
				a.log.Warn("上报失败", "err", err)
			}
		}
	}
}

func (a *Agent) reportOnce(ctx context.Context) error {
	rep, err := a.buildReport(false)
	if err != nil {
		return err
	}
	return a.postReport(ctx, rep)
}

func (a *Agent) postReport(ctx context.Context, rep protocol.Report) error {
	body, err := json.Marshal(rep)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.httpEndpoint(), strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(protocol.HeaderSecret, a.cfg.Secret)

	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("面板拒绝了节点密钥，请检查配置或到面板重新获取安装命令")
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("面板返回 HTTP %d", resp.StatusCode)
	}

	var ack protocol.AckPayload
	if json.NewDecoder(resp.Body).Decode(&ack) == nil {
		a.logAck(ack)
	}
	return nil
}

// logAck 把面板认定的用量打到日志里。
// 在节点上直接 journalctl 就能看到"面板认为我用了多少"，排查口径分歧很有用。
func (a *Agent) logAck(ack protocol.AckPayload) {
	if ack.QuotaBytes > 0 {
		pct := float64(ack.BilledBytes) / float64(ack.QuotaBytes) * 100
		a.log.Debug("上报完成",
			"面板计费流量", ack.BilledBytes, "配额", ack.QuotaBytes,
			"已用", fmt.Sprintf("%.1f%%", pct), "状态", ack.Status)
		return
	}
	a.log.Debug("上报完成", "面板计费流量", ack.BilledBytes, "状态", ack.Status)
}

// --- WebSocket 通道 ---

func (a *Agent) runWS(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	ws, _, err := websocket.Dial(dialCtx, a.wsEndpoint(), &websocket.DialOptions{
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		HTTPHeader: http.Header{protocol.HeaderSecret: []string{a.cfg.Secret}},
	})
	if err != nil {
		return fmt.Errorf("连接面板: %w", err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "")

	ws.SetReadLimit(256 * 1024)
	a.log.Info("已通过 WebSocket 连上面板")

	sessCtx, cancelSess := context.WithCancel(ctx)
	defer cancelSess()

	// 出站队列 + 单一写 goroutine：websocket 不允许并发写
	out := make(chan protocol.Frame, 8)
	errCh := make(chan error, 2)

	go func() {
		errCh <- a.wsWriteLoop(sessCtx, ws, out)
	}()

	go func() {
		errCh <- a.wsReadLoop(sessCtx, ws, out)
	}()

	select {
	case <-sessCtx.Done():
		return sessCtx.Err()
	case err := <-errCh:
		return err
	}
}

func (a *Agent) wsWriteLoop(ctx context.Context, ws *websocket.Conn, out chan protocol.Frame) error {
	send := func(f protocol.Frame) error {
		b, err := json.Marshal(f)
		if err != nil {
			return err
		}
		wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return ws.Write(wctx, websocket.MessageText, b)
	}

	report := func(final bool) error {
		rep, err := a.buildReport(final)
		if err != nil {
			a.log.Warn("采集失败", "err", err)
			return nil // 采集失败不该拆连接，下个周期再试
		}
		return send(protocol.Frame{Type: protocol.FrameReport, Report: &rep})
	}

	if err := report(false); err != nil {
		return err
	}

	t := time.NewTicker(a.cfg.ReportInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case f := <-out:
			if err := send(f); err != nil {
				return err
			}
		case <-a.flush:
			if err := report(true); err != nil {
				return err
			}
		case <-t.C:
			if err := report(false); err != nil {
				return err
			}
		}
	}
}

func (a *Agent) wsReadLoop(ctx context.Context, ws *websocket.Conn, out chan protocol.Frame) error {
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			continue
		}

		var f protocol.Frame
		if err := json.Unmarshal(data, &f); err != nil {
			a.log.Warn("面板发来的数据无法解析", "err", err)
			continue
		}

		switch f.Type {
		case protocol.FrameAck:
			var ack protocol.AckPayload
			if f.Message != "" && json.Unmarshal([]byte(f.Message), &ack) == nil {
				a.logAck(ack)
			}

		case protocol.FramePing:
			select {
			case out <- protocol.Frame{Type: protocol.FramePong}:
			default:
			}

		case protocol.FrameCommand:
			if f.Command == nil {
				continue
			}
			cmd := *f.Command
			// 单开 goroutine 执行，别把读循环堵住——
			// 读循环停了就收不到后续指令，也发不出心跳
			go func() {
				res := a.execute(ctx, cmd)
				select {
				case out <- protocol.Frame{Type: protocol.FrameResult, Result: &res}:
				case <-time.After(10 * time.Second):
					a.log.Warn("指令回执发送超时", "cmd_id", cmd.ID)
				}
			}()
		}
	}
}

// --- 伪造流量（调试用）---

type fakeTraffic struct {
	bootID    string
	rate      float64 // 字节/秒，双向各按这个速率涨
	start     time.Time
	baseRx    int64
	baseTx    int64
	iface     string
	restarted int
}

// newFakeTraffic 解析 "50MB/s"、"1.5GB/s"、"100KB" 这类速率写法。
func newFakeTraffic(spec, bootID string) (*fakeTraffic, error) {
	s := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(spec), "/s"))
	if s == "" {
		return nil, fmt.Errorf("伪造流量速率为空")
	}

	var num float64
	var unit string
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	if _, err := fmt.Sscanf(s[:i], "%f", &num); err != nil {
		return nil, fmt.Errorf("无法解析速率 %q", spec)
	}
	unit = strings.ToUpper(strings.TrimSpace(s[i:]))

	mult := map[string]float64{
		"": 1, "B": 1, "K": 1 << 10, "KB": 1 << 10, "KIB": 1 << 10,
		"M": 1 << 20, "MB": 1 << 20, "MIB": 1 << 20,
		"G": 1 << 30, "GB": 1 << 30, "GIB": 1 << 30,
	}[unit]
	if mult == 0 {
		return nil, fmt.Errorf("无法识别速率单位 %q", unit)
	}

	return &fakeTraffic{
		bootID: bootID,
		rate:   num * mult,
		start:  time.Now(),
		iface:  "fake0",
	}, nil
}

func (f *fakeTraffic) snapshot() collector.Snapshot {
	elapsed := time.Since(f.start).Seconds()
	grown := int64(math.Round(f.rate * elapsed))
	return collector.Snapshot{
		BootID:     f.bootID,
		Iface:      f.iface,
		Rx:         f.baseRx + grown,
		Tx:         f.baseTx + grown/2, // 出入不对称，方便验证"单向取大"口径
		Uptime:     int64(elapsed),
		Load1:      0.1,
		CPUPercent: 1,
		MemTotal:   1 << 30,
		MemUsed:    1 << 28,
	}
}
