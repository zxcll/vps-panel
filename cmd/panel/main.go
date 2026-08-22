// panel 是面板服务端：HTTP 接口 + 探针接入 + 后台调度 + 内嵌前端，单个二进制。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	// 把时区数据打进二进制。账单周期按节点自己的时区算边界，
	// 而精简镜像（scratch/alpine/distroless）里通常没有 /usr/share/zoneinfo。
	_ "time/tzdata"

	"github.com/zxcll/vps-panel/internal/action"
	"github.com/zxcll/vps-panel/internal/config"
	"github.com/zxcll/vps-panel/internal/crypto"
	"github.com/zxcll/vps-panel/internal/engine"
	"github.com/zxcll/vps-panel/internal/failover"
	"github.com/zxcll/vps-panel/internal/ingest"
	"github.com/zxcll/vps-panel/internal/notify"
	"github.com/zxcll/vps-panel/internal/server"
	"github.com/zxcll/vps-panel/internal/store"
)

func main() {
	var (
		cfgPath  = flag.String("config", "", "配置文件路径（yaml，可选）")
		listen   = flag.String("listen", "", "监听地址，覆盖配置文件")
		dataDir  = flag.String("data-dir", "", "数据目录，覆盖配置文件")
		baseURL  = flag.String("base-url", "", "探针访问面板的地址，如 https://panel.example.com")
		logLevel = flag.String("log-level", "info", "日志级别: debug|info|warn|error")
		showVer  = flag.Bool("version", false, "打印版本后退出")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("vps-panel", version)
		return
	}

	log := newLogger(*logLevel)

	if err := run(log, *cfgPath, *listen, *dataDir, *baseURL); err != nil {
		log.Error("面板启动失败", "err", err)
		os.Exit(1)
	}
}

const version = "1.3.1"

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

func run(log *slog.Logger, cfgPath, listen, dataDir, baseURL string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	// 命令行参数优先级最高
	if listen != "" {
		cfg.Listen = listen
	}
	if dataDir != "" {
		abs, err := filepath.Abs(dataDir)
		if err != nil {
			return err
		}
		cfg.DataDir = abs
	}
	if baseURL != "" {
		cfg.BaseURL = baseURL
	}
	// 命令行覆盖发生在 config.Load 之后，得再归一化一次，
	// 否则 --base-url 1.2.3.4:8080 这种写法会带着"没有协议"的地址
	// 一路传到探针那边，逼探针去猜。
	cfg.BaseURL = config.NormalizeBaseURL(cfg.BaseURL)

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("创建数据目录 %s: %w", cfg.DataDir, err)
	}

	key, err := cfg.ResolveMasterKey()
	if err != nil {
		return err
	}
	cipher, err := crypto.New(key)
	if err != nil {
		return err
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	initialPassword, err := server.Bootstrap(ctx, st)
	if err != nil {
		return err
	}

	// 组装各个部件。依赖关系：
	//   hub  ← 探针连接池，同时充当"下发指令"和"是否在线"的实现
	//   exec ← 编排探针通道与 SSH 通道
	//   fo   ← 健康判定与 DNS 切换
	//   eng  ← 后台调度：周期滚动、配额判定、触发动作
	hub := server.NewHub(log)
	notifier := notify.New(st, log)
	sshRunner := action.NewSSHRunner(cipher)
	exec := action.NewExecutor(st, sshRunner, hub)
	fo := failover.New(st, cipher, notifier, hub, log)
	eng := engine.New(st, exec, fo, notifier, log)
	ing := ingest.New(st)

	srv := server.New(cfg, st, cipher, hub, ing, exec, fo, eng, notifier, log)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// WriteTimeout 必须留空：探针的 WebSocket 是长连接，
		// 设了写超时会被周期性掐断。
		IdleTimeout: 120 * time.Second,
	}

	go eng.Run(ctx)
	// 转发规则的对账循环。改规则时会立刻下发一次，这个循环只负责
	// 把当时离线或下发失败的节点补上。
	go srv.RunForwardSync(ctx)
	// 阿里云 CDT：拉流量/账单/实例，并按配置做熔断停机、抢占式实例保活、
	// 定时开关机。没配账号时它每 30 秒空转一次，代价可以忽略。
	go srv.RunCDTSync(ctx)

	printBanner(log, cfg.Listen, cfg.DataDir, initialPassword)

	// 探针二进制缺失时，面板照常跑，但节点上的一键安装命令会 404。
	// 这个失败发生在另一台机器上，不在这里说一声用户很难联想到。
	if missing := srv.MissingAgentBinaries(); len(missing) > 0 {
		log.Warn("缺少探针二进制，节点端一键安装会失败",
			"缺失架构", strings.Join(missing, ", "),
			"存放目录", cfg.AgentsDir(),
			"补救", "在面板机器上执行 sudo bash install.sh panel agents")
	}
	if cfg.BaseURL == "" {
		log.Warn("没有配置 base_url，安装命令里的面板地址只能按请求头猜。" +
			"建议设置 PANEL_BASE_URL 或配置文件里的 base_url")
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("HTTP 服务异常退出: %w", err)
	case <-ctx.Done():
		log.Info("收到退出信号，正在关闭…")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("HTTP 服务关闭超时", "err", err)
	}
	log.Info("已退出")
	return nil
}

func printBanner(log *slog.Logger, listen, dataDir, initialPassword string) {
	log.Info("面板已启动", "监听", listen, "数据目录", dataDir, "版本", version)

	if initialPassword != "" {
		// 只有首次启动会打印。用醒目的分隔让它不会淹没在日志里。
		fmt.Fprintf(os.Stdout, `
================================================================
  面板初始化完成，管理员账号如下（这条信息只会显示这一次）

      用户名：admin
      密  码：%s

  登录后请到「设置 → 修改登录凭据」改成自己的密码。
================================================================

`, initialPassword)
	}

	if listen != "" && (listen[0] == ':' || listen[0] == '0') {
		log.Warn("面板正监听在所有网卡上。它保存着各节点的 SSH 凭据，" +
			"建议改成只监听 127.0.0.1 并用 Nginx 反代 + HTTPS 对外提供服务")
	}
}
