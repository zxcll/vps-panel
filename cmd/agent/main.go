// vps-agent 是节点探针：定期把网卡累计计数器报给面板，并执行面板下发的指令。
//
// 它不在本地记账。所有流量统计都由面板做差值累加，
// 所以这台机器怎么重启，面板上的账本都不会丢也不会重。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/zxcll/vps-panel/internal/agent"
)

// defaultForwardState 是转发状态文件的默认位置。
// 放在 /var/lib 而不是 /etc：它是运行期状态（计数水位、当前规则），不是配置。
const defaultForwardState = "/var/lib/vps-agent/forward.json"

func main() {
	var (
		server   = flag.String("server", envOr("VPS_AGENT_SERVER", ""), "面板地址，如 wss://panel.example.com")
		secret   = flag.String("secret", envOr("VPS_AGENT_SECRET", ""), "节点密钥")
		iface    = flag.String("iface", envOr("VPS_AGENT_IFACE", ""), "统计哪块网卡，留空自动识别默认路由网卡")
		allIf    = flag.Bool("all-ifaces", envBool("VPS_AGENT_ALL_IFACES", false), "汇总所有物理网卡（多网卡机器用）")
		interval = flag.Duration("interval", envDuration("VPS_AGENT_INTERVAL", 30*time.Second), "上报间隔")
		mode     = flag.String("mode", envOr("VPS_AGENT_MODE", "auto"), "通信方式: auto|ws|http")
		allowEx  = flag.Bool("allow-exec", envBool("VPS_AGENT_ALLOW_EXEC", true), "是否允许面板下发自定义命令")
		allowFwd = flag.Bool("allow-forward", envBool("VPS_AGENT_ALLOW_FORWARD", true), "是否允许面板下发端口转发规则")
		fwdState = flag.String("forward-state", envOr("VPS_AGENT_FORWARD_STATE", defaultForwardState), "转发状态文件路径，重启后靠它恢复规则和计数")
		fwdPool  = flag.Int("forward-pool", envInt("VPS_AGENT_FORWARD_POOL", 4), "用户态转发每个端口的预连接数，0 表示不预连接")
		fake     = flag.String("fake-traffic", envOr("VPS_AGENT_FAKE_TRAFFIC", ""), "调试用：伪造流量速率，如 50MB/s")
		logLevel = flag.String("log-level", envOr("VPS_AGENT_LOG_LEVEL", "info"), "日志级别: debug|info|warn|error")
		showVer  = flag.Bool("version", false, "打印版本后退出")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("vps-agent", agent.Version)
		return
	}

	log := newLogger(*logLevel)

	a, err := agent.New(agent.Config{
		Server:          *server,
		Secret:          *secret,
		Iface:           *iface,
		AllIfaces:       *allIf,
		ReportInterval:  *interval,
		Mode:            *mode,
		AllowExec:       *allowEx,
		AllowForward:    *allowFwd,
		ForwardState:    *fwdState,
		ForwardPoolSize: *fwdPool,
		FakeTraffic:     *fake,
		Log:             log,
	})
	if err != nil {
		log.Error("探针启动失败", "err", err)
		fmt.Fprintln(os.Stderr, "\n用法示例：")
		fmt.Fprintln(os.Stderr, "  vps-agent --server wss://panel.example.com --secret <节点密钥>")
		fmt.Fprintln(os.Stderr, "\n也可以用环境变量 VPS_AGENT_SERVER / VPS_AGENT_SECRET。")
		os.Exit(1)
	}

	// systemd 停止服务时发 SIGTERM。收到后探针会补报最后一次流量再退出，
	// 这样"重启机器"这个动作本身不会造成流量丢失。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := a.Run(ctx); err != nil {
		log.Error("探针异常退出", "err", err)
		os.Exit(1)
	}
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
