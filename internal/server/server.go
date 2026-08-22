// Package server 提供面板的 HTTP 接口、探针接入通道和内嵌前端。
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zxcll/vps-panel/internal/action"
	"github.com/zxcll/vps-panel/internal/config"
	"github.com/zxcll/vps-panel/internal/crypto"
	"github.com/zxcll/vps-panel/internal/engine"
	"github.com/zxcll/vps-panel/internal/failover"
	"github.com/zxcll/vps-panel/internal/ingest"
	"github.com/zxcll/vps-panel/internal/notify"
	"github.com/zxcll/vps-panel/internal/store"
)

type Server struct {
	cfg        config.Config
	st         *store.Store
	cipher     *crypto.Cipher
	hub        *Hub
	ing        *ingest.Ingestor
	exec       *action.Executor
	fo         *failover.Manager
	eng        *engine.Engine
	notifier   *notify.Notifier
	log        *slog.Logger
	trustProxy bool
	// fwdSync 记着每个节点最近下发成功的转发规则版本，避免无谓重发。
	fwdSync *forwardSyncer
	// cdt 是阿里云 CDT 后台循环的状态：账号级动作锁 + 各档子任务的分频计时。
	cdt *cdtGuard
}

func New(
	cfg config.Config,
	st *store.Store,
	cipher *crypto.Cipher,
	hub *Hub,
	ing *ingest.Ingestor,
	exec *action.Executor,
	fo *failover.Manager,
	eng *engine.Engine,
	n *notify.Notifier,
	log *slog.Logger,
) *Server {
	return &Server{
		cfg: cfg, st: st, cipher: cipher, hub: hub, ing: ing,
		exec: exec, fo: fo, eng: eng, notifier: n, log: log,
		trustProxy: cfg.TrustProxyHeaders,
		fwdSync:    newForwardSyncer(),
		cdt:        newCDTGuard(),
	}
}

// Handler 组装全部路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// --- 免鉴权 ---
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// --- 探针通道（用节点 secret 鉴权，不是 JWT）---
	mux.HandleFunc("GET /agent/ws", s.handleAgentWS)
	mux.HandleFunc("POST /agent/report", s.handleAgentReport)
	mux.HandleFunc("GET /agent/install.sh", s.handleInstallScript)
	mux.HandleFunc("GET /agent/download", s.handleAgentDownload)

	// --- 需要登录 ---
	auth := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, s.requireAuth(h))
	}

	auth("GET /api/me", s.handleMe)
	auth("POST /api/auth/password", s.handleChangePassword)

	auth("GET /api/overview", s.handleOverview)

	auth("GET /api/nodes", s.handleListNodes)
	auth("POST /api/nodes", s.handleCreateNode)
	auth("GET /api/nodes/{id}", s.handleGetNode)
	auth("PUT /api/nodes/{id}", s.handleUpdateNode)
	auth("DELETE /api/nodes/{id}", s.handleDeleteNode)
	auth("POST /api/nodes/{id}/reset", s.handleResetNode)
	auth("POST /api/nodes/{id}/shutdown", s.handleShutdownNode)
	auth("POST /api/nodes/{id}/exec", s.handleExecNode)
	auth("POST /api/nodes/{id}/test-ssh", s.handleTestSSH)
	auth("POST /api/nodes/{id}/rotate-secret", s.handleRotateSecret)
	auth("GET /api/nodes/{id}/traffic", s.handleNodeTraffic)
	auth("GET /api/nodes/{id}/cycles", s.handleNodeCycles)
	auth("GET /api/nodes/{id}/install", s.handleNodeInstallInfo)

	auth("GET /api/dns/providers", s.handleListProviders)
	auth("POST /api/dns/providers", s.handleCreateProvider)
	auth("PUT /api/dns/providers/{id}", s.handleUpdateProvider)
	auth("DELETE /api/dns/providers/{id}", s.handleDeleteProvider)

	auth("GET /api/dns/records", s.handleListRecords)
	auth("POST /api/dns/records", s.handleCreateRecord)
	auth("PUT /api/dns/records/{id}", s.handleUpdateRecord)
	auth("DELETE /api/dns/records/{id}", s.handleDeleteRecord)
	auth("POST /api/dns/records/{id}/switch", s.handleSwitchRecord)
	auth("POST /api/dns/records/{id}/sync", s.handleSyncRecord)
	auth("GET /api/dns/plan", s.handleDNSPlan)

	// 转发规则。/api/forwards/sync 和 /api/forwards/{id} 不会打架：
	// ServeMux 取最具体的匹配，字面量段优先于通配符段。
	auth("GET /api/forwards", s.handleListForwards)
	auth("POST /api/forwards", s.handleCreateForward)
	auth("POST /api/forwards/sync", s.handleSyncForward)
	auth("GET /api/forwards/nodes", s.handleListForwardNodes)
	auth("PUT /api/forwards/nodes/{id}", s.handleSaveForwardNode)
	auth("GET /api/forwards/hops/{id}/traffic", s.handleForwardTraffic)
	auth("GET /api/forwards/{id}", s.handleGetForward)
	auth("PUT /api/forwards/{id}", s.handleUpdateForward)
	auth("DELETE /api/forwards/{id}", s.handleDeleteForward)
	auth("POST /api/forwards/{id}/toggle", s.handleToggleForward)
	auth("POST /api/forwards/{id}/test", s.handleTestForward)

	// 阿里云 CDT。字面量段优先于通配符段，所以 /accounts 和 /instances
	// 不会和 /{id} 打架。
	auth("GET /api/cdt/accounts", s.handleListCDTAccounts)
	auth("POST /api/cdt/accounts", s.handleCreateCDTAccount)
	auth("PUT /api/cdt/accounts/{id}", s.handleUpdateCDTAccount)
	auth("DELETE /api/cdt/accounts/{id}", s.handleDeleteCDTAccount)
	auth("POST /api/cdt/accounts/{id}/sync", s.handleSyncCDTAccount)
	auth("POST /api/cdt/accounts/{id}/test", s.handleTestCDTAccount)
	auth("GET /api/cdt/instances", s.handleListCDTInstances)
	auth("POST /api/cdt/instances/{id}/start", s.handleStartCDTInstance)
	auth("POST /api/cdt/instances/{id}/stop", s.handleStopCDTInstance)
	auth("POST /api/cdt/instances/{id}/guard", s.handleGuardCDTInstance)

	auth("GET /api/events", s.handleListEvents)
	auth("GET /api/settings", s.handleGetSettings)
	auth("PUT /api/settings", s.handleSaveSettings)

	// --- 前端 ---
	mux.Handle("/", s.staticHandler())

	return s.withRecovery(s.withLogging(mux))
}

// withRecovery 兜住 handler 里的 panic，别让一个接口把整个面板带崩。
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("请求处理发生 panic", "path", r.URL.Path, "panic", rec)
				writeError(w, http.StatusInternalServerError, "服务器内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)

		// 静态资源和探针心跳量太大，只在 debug 级别记录
		level := slog.LevelDebug
		if strings.HasPrefix(r.URL.Path, "/api/") && rw.status >= 400 {
			level = slog.LevelWarn
		}
		s.log.Log(r.Context(), level, "HTTP",
			"method", r.Method, "path", r.URL.Path,
			"status", rw.status, "dur", time.Since(start).Round(time.Millisecond))
	})
}

// statusWriter 记录响应状态码供日志使用。
//
// 它必须把 Hijacker / Flusher 透传下去：WebSocket 升级要靠 Hijack 接管连接，
// 包装器只要吞掉这个接口，探针就会一直握手失败并降级成 HTTP 轮询——
// 而且降级是静默的，很容易被当成"能用"而忽略过去。
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("底层 ResponseWriter 不支持 Hijack")
	}
	return h.Hijack()
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.NewResponseController 能拿到原始的 ResponseWriter。
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// --- 通用辅助 ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		if err := json.NewEncoder(w).Encode(v); err != nil {
			// 响应头已经发出去了，这里只能记日志
			return
		}
	}
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// decodeJSON 解析请求体。解析失败时已经写好响应，返回 false。
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误: "+err.Error())
		return false
	}
	return true
}

// pathID 取路径里的 {id} 参数。
func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "非法的 ID: "+raw)
		return 0, false
	}
	return id, true
}

// handleStoreErr 把存储层错误映射成 HTTP 响应。
func handleStoreErr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "记录不存在")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

// mustNode 读取路径 id 指向的节点，出错时已写好响应。
func (s *Server) mustNode(w http.ResponseWriter, r *http.Request) (*store.Node, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return nil, false
	}
	n, err := s.st.GetNode(r.Context(), id)
	if err != nil {
		handleStoreErr(w, err)
		return nil, false
	}
	return n, true
}

// actionContext 给关机/执行命令这类动作一个独立的超时，
// 不受浏览器断开连接影响 —— 关机指令发出去就得让它跑完。
func actionContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 90*time.Second)
}

// atoiDefault 解析整数查询参数，失败时用默认值。
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
