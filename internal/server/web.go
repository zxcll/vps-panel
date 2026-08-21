package server

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/zxcll/vps-panel/web"
)

// staticHandler 提供内嵌的前端。
// 前端是纯 ES 模块 + 一份 Vue 运行时，没有构建步骤，go build 就能出完整二进制。
func (s *Server) staticHandler() http.Handler {
	sub := fs.FS(web.FS)
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := strings.TrimPrefix(cleanPath(r.URL.Path), "/")
		if clean == "" {
			clean = "index.html"
		}

		f, err := sub.Open(clean)
		if err != nil {
			// 前端是单页应用，未知路径一律回 index.html 交给前端路由
			if errors.Is(err, fs.ErrNotExist) {
				if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/agent/") {
					writeError(w, http.StatusNotFound, "接口不存在")
					return
				}
				r = r.Clone(r.Context())
				r.URL.Path = "/"
				serveIndex(w, r, sub)
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		f.Close()

		// JS 和 CSS 带上短缓存；index.html 不缓存，方便升级后立刻生效
		switch {
		case strings.HasSuffix(clean, ".js"), strings.HasSuffix(clean, ".css"):
			w.Header().Set("Cache-Control", "public, max-age=300")
		default:
			w.Header().Set("Cache-Control", "no-cache")
		}

		fileServer.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, sub fs.FS) {
	f, err := sub.Open("index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	io.Copy(w, f) //nolint:errcheck
}

// cleanPath 规整请求路径，挡掉 ../ 之类的穿越尝试。
func cleanPath(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return filepath.ToSlash(filepath.Clean(p))
}

// --- 探针二进制分发 ---

// agentBinaryPath 返回某个架构的探针二进制路径。
// 二进制放在数据目录下的 agents/ 里，由 make build 产出。
// 不内嵌进面板：两个架构加起来十几 MB，而且升级探针不该被迫重编面板。
func (s *Server) agentBinaryPath(arch string) string {
	return filepath.Join(s.cfg.DataDir, "agents", "vps-agent-linux-"+arch)
}

var supportedArch = map[string]bool{
	"amd64": true, "arm64": true, "arm": true, "386": true,
}

func (s *Server) handleAgentDownload(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authAgent(r); err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	arch := r.URL.Query().Get("arch")
	if arch == "" {
		arch = "amd64"
	}
	if !supportedArch[arch] {
		writeError(w, http.StatusBadRequest, "不支持的架构: "+arch)
		return
	}

	p := s.agentBinaryPath(arch)
	f, err := os.Open(p)
	if err != nil {
		s.log.Error("探针二进制缺失", "path", p, "err", err)
		writeError(w, http.StatusNotFound, fmt.Sprintf(
			"面板上找不到 %s 架构的探针二进制（%s）。请在面板机器上执行 make agents 生成后重试", arch, p))
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="vps-agent"`)
	http.ServeContent(w, r, "vps-agent", info.ModTime(), f)
}

// installScriptTemplate 是一键安装脚本。
//
// 几个刻意的设计：
//   - 自动识别架构，从面板下载对应二进制，不依赖外网
//   - 先下到临时文件再原子替换，升级时不会出现"下到一半的二进制"
//   - 生成 systemd unit 时把密钥写进单独的 EnvironmentFile（0600），
//     不放在 ExecStart 里，免得任何用户 ps aux 就能看到
const installScriptTemplate = `#!/bin/sh
set -e

PANEL="%s"
WS="%s"
SECRET="%s"
BIN=/usr/local/bin/vps-agent
ENVFILE=/etc/vps-agent.env
UNIT=/etc/systemd/system/vps-agent.service

if [ "$(id -u)" != "0" ]; then
    echo "请用 root 运行（前面加 sudo）" >&2
    exit 1
fi

case "$(uname -m)" in
    x86_64|amd64)   ARCH=amd64 ;;
    aarch64|arm64)  ARCH=arm64 ;;
    armv7l|armv6l)  ARCH=arm ;;
    i386|i686)      ARCH=386 ;;
    *) echo "不支持的 CPU 架构: $(uname -m)" >&2; exit 1 ;;
esac

echo "==> 从面板下载探针（$ARCH）"
TMP=$(mktemp)
if command -v curl >/dev/null 2>&1; then
    curl -fsSL -H "X-Node-Secret: $SECRET" "$PANEL/agent/download?arch=$ARCH" -o "$TMP"
elif command -v wget >/dev/null 2>&1; then
    wget -q --header="X-Node-Secret: $SECRET" "$PANEL/agent/download?arch=$ARCH" -O "$TMP"
else
    echo "机器上既没有 curl 也没有 wget，无法下载" >&2
    exit 1
fi

chmod 755 "$TMP"
mv -f "$TMP" "$BIN"

echo "==> 写入配置"
umask 077
cat > "$ENVFILE" <<EOF
VPS_AGENT_SERVER=$WS
VPS_AGENT_SECRET=$SECRET
EOF
chmod 600 "$ENVFILE"

echo "==> 注册 systemd 服务"
cat > "$UNIT" <<'EOF'
[Unit]
Description=VPS Traffic Panel Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/vps-agent.env
ExecStart=/usr/local/bin/vps-agent
Restart=always
RestartSec=5
# 关机前给探针一点时间做最后一次上报，尽量少丢流量
TimeoutStopSec=15
KillSignal=SIGTERM

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable vps-agent >/dev/null 2>&1 || true
systemctl restart vps-agent

echo
echo "==> 完成。查看运行状态："
echo "    systemctl status vps-agent"
echo "    journalctl -u vps-agent -f"
echo
echo "面板上大约 30 秒内会显示这个节点在线。"
`

func (s *Server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	node, err := s.authAgent(r)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, "# 安装脚本获取失败：%s\n# 请在面板的节点页面复制完整的安装命令\n", err.Error())
		return
	}

	base := s.baseURL(r)
	script := fmt.Sprintf(installScriptTemplate, base, wsURL(base), node.Secret)

	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, script) //nolint:errcheck
}
