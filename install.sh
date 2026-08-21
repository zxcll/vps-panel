#!/usr/bin/env bash
#
# VPS 流量面板 —— 一键安装与管理脚本
#
# 用法：
#   交互菜单：  bash install.sh
#   直接执行：  bash install.sh panel install
#              bash install.sh agent install --server wss://panel.example.com --secret xxx
#              bash install.sh agent restart
#
# 面板端和节点端都用这一个脚本管理，运行时先选要操作哪一端。
#
set -u

# ============================================================================
# 配置变量（发布到 GitHub 后按需修改，或用同名环境变量覆盖）
# ============================================================================

# GITHUB_REPO 是仓库地址，格式 用户名/仓库名。
# 脚本会从 https://github.com/$GITHUB_REPO/releases 下载预编译二进制。
GITHUB_REPO="${GITHUB_REPO:-zxcll/vps-panel}"

# GITHUB_RELEASE_TAG 指定要装的版本，latest 表示最新正式版。
GITHUB_RELEASE_TAG="${GITHUB_RELEASE_TAG:-latest}"

# GITHUB_PROXY 是加速前缀。国内机器直连 GitHub 经常超时，
# 可以填 https://ghfast.top/ 之类的镜像前缀（末尾要带斜杠）。留空表示直连。
GITHUB_PROXY="${GITHUB_PROXY:-}"

# 也可以完全绕过 GitHub，直接指定二进制的下载地址前缀。
# 留空时按 GITHUB_REPO 拼出 Releases 地址。
DOWNLOAD_BASE="${DOWNLOAD_BASE:-}"

# ---- 安装路径 ----
PANEL_BIN="${PANEL_BIN:-/usr/local/bin/vps-panel}"
PANEL_DATA="${PANEL_DATA:-/var/lib/vps-panel}"
PANEL_ENV="${PANEL_ENV:-/etc/vps-panel.env}"
PANEL_SERVICE="vps-panel"

AGENT_BIN="${AGENT_BIN:-/usr/local/bin/vps-agent}"
AGENT_ENV="${AGENT_ENV:-/etc/vps-agent.env}"
# 转发状态目录：探针靠它在重启后恢复规则、接上流量计数
AGENT_STATE_DIR="${AGENT_STATE_DIR:-/var/lib/vps-agent}"
AGENT_SERVICE="vps-agent"

SYSTEMD_DIR="/etc/systemd/system"

# ============================================================================
# 输出辅助
# ============================================================================

if [ -t 1 ]; then
    C_RED='\033[31m'; C_GRN='\033[32m'; C_YEL='\033[33m'
    C_BLU='\033[36m'; C_BLD='\033[1m'; C_END='\033[0m'
else
    C_RED=''; C_GRN=''; C_YEL=''; C_BLU=''; C_BLD=''; C_END=''
fi

info()  { printf "%b==>%b %s\n" "$C_BLU" "$C_END" "$*"; }
ok()    { printf "%b ✓%b %s\n" "$C_GRN" "$C_END" "$*"; }
warn()  { printf "%b ! %b %s\n" "$C_YEL" "$C_END" "$*"; }
err()   { printf "%b ✗%b %s\n" "$C_RED" "$C_END" "$*" >&2; }
die()   { err "$*"; exit 1; }
title() { printf "\n%b%s%b\n" "$C_BLD" "$*" "$C_END"; }

# ============================================================================
# 环境检查
# ============================================================================

need_root() {
    [ "$(id -u)" = "0" ] || die "需要 root 权限，请用 sudo 运行：sudo bash $0 $*"
}

need_systemd() {
    command -v systemctl >/dev/null 2>&1 \
        || die "没有找到 systemctl。本脚本依赖 systemd 管理服务。"
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)  echo amd64 ;;
        aarch64|arm64) echo arm64 ;;
        armv7l|armv6l) echo arm ;;
        i386|i686)     echo 386 ;;
        *) die "不支持的 CPU 架构：$(uname -m)" ;;
    esac
}

# fetch URL 输出文件 —— 优先 curl，没有就用 wget
fetch() {
    local url="$1" out="$2"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --connect-timeout 15 --retry 2 -o "$out" "$url"
    elif command -v wget >/dev/null 2>&1; then
        wget -q --timeout=15 --tries=2 -O "$out" "$url"
    else
        die "机器上既没有 curl 也没有 wget，无法下载。先装一个：apt install curl 或 yum install curl"
    fi
}

# download_binary 名称 架构 输出路径
download_binary() {
    local name="$1" arch="$2" out="$3" url

    if [ -n "$DOWNLOAD_BASE" ]; then
        url="${DOWNLOAD_BASE%/}/${name}-linux-${arch}"
    elif [ "$GITHUB_RELEASE_TAG" = "latest" ]; then
        url="https://github.com/${GITHUB_REPO}/releases/latest/download/${name}-linux-${arch}"
    else
        url="https://github.com/${GITHUB_REPO}/releases/download/${GITHUB_RELEASE_TAG}/${name}-linux-${arch}"
    fi
    [ -n "$GITHUB_PROXY" ] && url="${GITHUB_PROXY%/}/${url}"

    info "下载 ${name}（linux/${arch}）"
    printf "    %s\n" "$url"

    local tmp
    tmp="$(mktemp)"
    if ! fetch "$url" "$tmp"; then
        rm -f "$tmp"
        err "下载失败。可能的原因："
        err "  · GITHUB_REPO 还是默认值，没改成你自己的仓库"
        err "  · 该版本还没有发布 ${name}-linux-${arch} 这个文件"
        err "  · 网络连不上 GitHub —— 试试设置加速前缀："
        err "      GITHUB_PROXY=https://ghfast.top/ bash $0"
        return 1
    fi

    # 简单校验：ELF 文件头是 0x7F 'E' 'L' 'F'
    if ! head -c 4 "$tmp" | grep -q 'ELF'; then
        rm -f "$tmp"
        err "下载到的不是可执行文件（多半是 404 页面），请检查仓库和版本号"
        return 1
    fi

    chmod 755 "$tmp"
    # 先下到临时文件再原子替换，升级时不会出现"下到一半的二进制"
    mv -f "$tmp" "$out"
    ok "已安装到 $out"
}

# download_agents 把各架构探针下到面板的数据目录。
#
# 这一步以前漏了，导致面板装好后节点端一键安装必然 404 ——
# 面板的 /agent/download 就是从这个目录读文件的。
download_agents() {
    local dir="$PANEL_DATA/agents" got=0 failed=""
    mkdir -p "$dir"

    info "下载各架构探针二进制（节点端一键安装要用）"
    for arch in amd64 arm64 arm 386; do
        if download_binary agent "$arch" "$dir/vps-agent-linux-$arch" 2>/dev/null; then
            got=$((got + 1))
        else
            failed="$failed $arch"
        fi
    done

    if [ "$got" -gt 0 ]; then
        ok "已就绪 $got 个架构的探针（$dir）"
        [ -n "$failed" ] && warn "以下架构没下到，用到时再补：$failed"
        return 0
    fi

    echo
    err "一个架构都没下到，节点端的一键安装命令会失败。"
    err "常见原因和对策："
    err "  1. 仓库还没发布 Release —— 打个 tag 触发构建："
    err "       git tag v1.0.0 && git push origin v1.0.0"
    err "  2. 连不上 GitHub —— 换成加速前缀重试："
    err "       GITHUB_PROXY=https://ghfast.top/ sudo bash $0 panel agents"
    err "  3. 自己编译后放进去（需要 Go 1.26）："
    err "       git clone https://github.com/$GITHUB_REPO && cd vps-panel && make agents"
    err "       sudo cp data/agents/* $dir/"
    return 1
}

# detect_public_ip 探测本机公网 IP，用来给"面板对外地址"提供一个能用的默认值。
detect_public_ip() {
    local ip u
    for u in https://api.ipify.org https://ifconfig.me/ip https://ipinfo.io/ip; do
        ip="$(curl -fsS --connect-timeout 5 "$u" 2>/dev/null | tr -d "[:space:]")"
        case "$ip" in
            *[0-9].*[0-9].*[0-9].*[0-9]) printf "%s" "$ip"; return 0 ;;
        esac
    done
    return 1
}

# normalize_url 补全协议前缀。
#
# 不带协议的地址会原样传给探针，而探针只能靠猜。猜成 https 连明文面板
# 就是 "server gave HTTP response to HTTPS client"。在这里补齐最省事。
normalize_url() {
    local u="$1"
    u="${u%/}"
    case "$u" in
        http://*|https://*) printf "%s" "$u" ;;
        *) printf "http://%s" "$u" ;;
    esac
}

svc_exists() { [ -f "$SYSTEMD_DIR/$1.service" ]; }

svc_active() { systemctl is-active --quiet "$1" 2>/dev/null; }

# ask 提示 默认值 —— 读一行输入，回车用默认值
ask() {
    local prompt="$1" default="${2:-}" reply
    if [ -n "$default" ]; then
        printf "%s [%s]: " "$prompt" "$default" >&2
    else
        printf "%s: " "$prompt" >&2
    fi
    read -r reply </dev/tty || reply=""
    [ -z "$reply" ] && reply="$default"
    printf '%s' "$reply"
}

confirm() {
    local reply
    printf "%s [y/N]: " "$1" >&2
    read -r reply </dev/tty || reply=""
    case "$reply" in [yY]|[yY][eE][sS]) return 0 ;; *) return 1 ;; esac
}

# ============================================================================
# 面板端
# ============================================================================

panel_write_unit() {
    cat > "$SYSTEMD_DIR/$PANEL_SERVICE.service" <<EOF
[Unit]
Description=VPS Traffic Panel
Documentation=https://github.com/${GITHUB_REPO}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=$PANEL_ENV
ExecStart=$PANEL_BIN
Restart=always
RestartSec=5
WorkingDirectory=$PANEL_DATA

# 面板保存着各节点的 SSH 凭据，收紧一下权限面
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=true
ReadWritePaths=$PANEL_DATA

[Install]
WantedBy=multi-user.target
EOF
}

panel_install() {
    need_root
    need_systemd
    local arch upgrade=0
    arch="$(detect_arch)"

    svc_exists "$PANEL_SERVICE" && upgrade=1

    title "$([ $upgrade = 1 ] && echo '升级面板端' || echo '安装面板端')"

    download_binary panel "$arch" "$PANEL_BIN" || return 1

    mkdir -p "$PANEL_DATA"
    chmod 750 "$PANEL_DATA"

    # 面板要把探针分发给各节点，这些二进制必须先落到数据目录里。
    # 下不到不算致命——面板本身能跑，事后用菜单补即可。
    download_agents || true

    if [ "$upgrade" = 1 ]; then
        info "保留已有配置：$PANEL_ENV"
    else
        local listen base_url pubip port default_base
        echo
        echo "  面板保存着各节点的 SSH 凭据，直接暴露公网风险很高。"
        echo "  推荐监听 127.0.0.1，前面用 Nginx 反代 + HTTPS。"
        echo "  如果暂时没有反代，填 0.0.0.0:8080 先跑起来也行。"
        echo
        listen="$(ask '  监听地址' '0.0.0.0:8080')"

        # 猜一个各节点真正连得上的地址。监听地址里的 0.0.0.0 / 127.0.0.1
        # 对别的机器毫无意义，直接拿它当默认值等于给用户挖坑。
        port="${listen##*:}"
        [ "$port" = "$listen" ] && port=8080
        printf "  正在探测本机公网 IP…"
        pubip="$(detect_public_ip)" && printf " %s\n" "$pubip" || printf " 没探到\n"
        if [ -n "$pubip" ]; then
            default_base="http://$pubip:$port"
        else
            default_base=""
        fi

        echo
        echo "  下面这个地址要让各节点的探针访问得到，会写进一键安装命令。"
        echo "  有域名 + HTTPS 就填 https://panel.example.com，"
        echo "  没有的话填 http://公网IP:$port 即可。"
        while :; do
            base_url="$(normalize_url "$(ask '  面板对外地址' "$default_base")")"
            case "$base_url" in
                *0.0.0.0*|*127.0.0.1*|*localhost*)
                    warn "「$base_url」只有本机能访问，其它 VPS 上的探针连不上，请换成公网 IP 或域名"
                    continue ;;
                http://*|https://*) ;;
                *) warn "地址不合法，请重填"; continue ;;
            esac
            break
        done
        ok "面板对外地址：$base_url"

        umask 077
        cat > "$PANEL_ENV" <<EOF
# VPS 流量面板配置。改完执行 systemctl restart $PANEL_SERVICE 生效。
PANEL_LISTEN=$listen
PANEL_DATA_DIR=$PANEL_DATA
PANEL_BASE_URL=$base_url

# 加密 SSH / DNS 凭据的主密钥。留空则自动在数据目录下生成 master.key。
# 这个密钥丢了，库里所有凭据都解不开，只能重填。
# PANEL_MASTER_KEY=
EOF
        chmod 600 "$PANEL_ENV"
        ok "配置已写入 $PANEL_ENV"
    fi

    panel_write_unit
    systemctl daemon-reload
    systemctl enable "$PANEL_SERVICE" >/dev/null 2>&1
    systemctl restart "$PANEL_SERVICE"

    sleep 2
    if svc_active "$PANEL_SERVICE"; then
        ok "面板已启动"
    else
        err "面板启动失败，日志如下："
        journalctl -u "$PANEL_SERVICE" -n 30 --no-pager
        return 1
    fi

    if [ "$upgrade" = 0 ]; then
        title "初始管理员密码"
        echo "  下面这条信息只在首次启动时打印一次，请立刻保存："
        echo
        journalctl -u "$PANEL_SERVICE" --no-pager 2>/dev/null \
            | grep -A6 '面板初始化完成' | sed 's/^/  /' || true
        echo
        echo "  没看到的话，执行： journalctl -u $PANEL_SERVICE | grep -A6 初始化"
    fi

    echo
    ok "面板端就绪。接着到浏览器里打开面板 → 新建节点 → 复制安装命令去各台 VPS 执行。"
}

panel_uninstall() {
    need_root
    title "卸载面板端"

    confirm "  确定要卸载面板吗？" || { info "已取消"; return 0; }

    systemctl disable --now "$PANEL_SERVICE" >/dev/null 2>&1
    rm -f "$SYSTEMD_DIR/$PANEL_SERVICE.service"
    systemctl daemon-reload
    rm -f "$PANEL_BIN"
    ok "程序和服务已删除"

    echo
    warn "数据目录 $PANEL_DATA 里有流量账本、节点配置和加密后的 SSH 凭据。"
    if confirm "  一并删除数据目录吗？（不可恢复）"; then
        rm -rf "$PANEL_DATA" "$PANEL_ENV"
        ok "数据已删除"
    else
        info "已保留 $PANEL_DATA，重新安装后可以直接接上"
    fi
}

# ============================================================================
# 节点端（探针）
# ============================================================================

# agent_forward_deps 装端口转发需要的系统工具。
#
# 缺了它们探针照样跑（流量监控不受影响），只是转发规则下发时会失败，
# 所以这里全程尽力而为，装不上只提醒不中断安装。
agent_forward_deps() {
    local missing=""
    command -v nft >/dev/null 2>&1 || missing="$missing nftables"
    command -v tc  >/dev/null 2>&1 || missing="$missing iproute2"
    [ -z "$missing" ] && return 0

    info "安装端口转发依赖：$missing"
    if command -v apt-get >/dev/null 2>&1; then
        DEBIAN_FRONTEND=noninteractive apt-get update -qq >/dev/null 2>&1 || true
        # shellcheck disable=SC2086
        DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends $missing >/dev/null 2>&1 || true
    elif command -v dnf >/dev/null 2>&1; then
        # shellcheck disable=SC2086
        dnf install -y -q $missing >/dev/null 2>&1 || true
    elif command -v yum >/dev/null 2>&1; then
        # shellcheck disable=SC2086
        yum install -y -q $missing >/dev/null 2>&1 || true
    elif command -v apk >/dev/null 2>&1; then
        # shellcheck disable=SC2086
        apk add --no-cache $missing >/dev/null 2>&1 || true
    fi

    # 精简镜像上模块常常在盘里但没自动加载，装完补一下。
    command -v nft >/dev/null 2>&1 && modprobe nf_tables >/dev/null 2>&1 || true

    if command -v nft >/dev/null 2>&1 && command -v tc >/dev/null 2>&1; then
        ok "端口转发依赖已就绪"
    else
        warn "端口转发依赖没装全（缺$missing），流量监控不受影响；"
        warn "要用转发功能的话请手动安装后重启 $AGENT_SERVICE。"
    fi
}

agent_write_unit() {
    cat > "$SYSTEMD_DIR/$AGENT_SERVICE.service" <<EOF
[Unit]
Description=VPS Traffic Panel Agent
Documentation=https://github.com/${GITHUB_REPO}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=$AGENT_ENV
ExecStart=$AGENT_BIN
Restart=always
RestartSec=5

# 停止前留点时间给探针补报最后一段流量，尽量少丢账
TimeoutStopSec=15
KillSignal=SIGTERM

[Install]
WantedBy=multi-user.target
EOF
}

agent_install() {
    need_root
    need_systemd

    local server="" secret="" iface="" arch upgrade=0
    # 支持从命令行直接传参，方便批量部署
    while [ $# -gt 0 ]; do
        case "$1" in
            --server) server="${2:-}"; shift 2 ;;
            --secret) secret="${2:-}"; shift 2 ;;
            --iface)  iface="${2:-}";  shift 2 ;;
            *) shift ;;
        esac
    done

    svc_exists "$AGENT_SERVICE" && upgrade=1
    arch="$(detect_arch)"

    title "$([ $upgrade = 1 ] && echo '升级节点端' || echo '安装节点端')"

    # 升级时如果没给新参数，沿用已有配置
    if [ "$upgrade" = 1 ] && [ -z "$server" ] && [ -z "$secret" ]; then
        info "保留已有配置：$AGENT_ENV"
    else
        if [ -z "$server" ] || [ -z "$secret" ]; then
            echo
            echo "  这两项在面板上「节点管理 → 安装」里能看到。"
            echo
            [ -z "$server" ] && server="$(ask '  面板地址（如 https://panel.example.com 或 http://1.2.3.4:8080）')"
            [ -z "$secret" ] && secret="$(ask '  节点密钥')"
        fi
        [ -n "$server" ] || die "面板地址不能为空"
        # 补全协议。少了它探针只能靠猜，猜错就是
        # "server gave HTTP response to HTTPS client"。
        server="$(normalize_url "$server")"
        [ -n "$secret" ] || die "节点密钥不能为空"

        umask 077
        {
            echo "# 节点探针配置。改完执行 systemctl restart $AGENT_SERVICE 生效。"
            echo "VPS_AGENT_SERVER=$server"
            echo "VPS_AGENT_SECRET=$secret"
            [ -n "$iface" ] && echo "VPS_AGENT_IFACE=$iface"
            echo "# 上报间隔。这个值决定了机器突然断电时最多丢多少流量。"
            echo "VPS_AGENT_INTERVAL=30s"
            echo "# 是否允许面板下发自定义命令。设为 false 则只接受关机指令。"
            echo "VPS_AGENT_ALLOW_EXEC=true"
            echo "# 是否允许面板下发端口转发规则。转发会改本机防火墙，"
            echo "# 不用这个功能的话建议设为 false。"
            echo "VPS_AGENT_ALLOW_FORWARD=true"
        } > "$AGENT_ENV"
        chmod 600 "$AGENT_ENV"
        ok "配置已写入 $AGENT_ENV"
    fi

    # 优先从面板下载探针 —— 面板一定和探针版本匹配，也不受 GitHub 网络影响
    local from_panel=0
    if [ -f "$AGENT_ENV" ]; then
        local s p
        s="$(grep -E '^VPS_AGENT_SERVER=' "$AGENT_ENV" | cut -d= -f2-)"
        p="$(printf '%s' "$s" | sed -e 's#^wss://#https://#' -e 's#^ws://#http://#')"
        local sec
        sec="$(grep -E '^VPS_AGENT_SECRET=' "$AGENT_ENV" | cut -d= -f2-)"
        if [ -n "$p" ] && [ -n "$sec" ]; then
            info "尝试从面板下载探针：$p"
            local tmp
            tmp="$(mktemp)"
            if command -v curl >/dev/null 2>&1 &&
               curl -fsSL --connect-timeout 10 -H "X-Node-Secret: $sec" \
                    "$p/agent/download?arch=$arch" -o "$tmp" 2>/dev/null &&
               head -c 4 "$tmp" | grep -q 'ELF'; then
                chmod 755 "$tmp"
                mv -f "$tmp" "$AGENT_BIN"
                ok "已从面板安装到 $AGENT_BIN"
                from_panel=1
            else
                rm -f "$tmp"
                info "面板没提供二进制，改从 GitHub 下载"
            fi
        fi
    fi
    [ "$from_panel" = 0 ] && { download_binary agent "$arch" "$AGENT_BIN" || return 1; }

    agent_forward_deps
    mkdir -p "$AGENT_STATE_DIR" && chmod 750 "$AGENT_STATE_DIR"

    agent_write_unit
    systemctl daemon-reload
    systemctl enable "$AGENT_SERVICE" >/dev/null 2>&1
    systemctl restart "$AGENT_SERVICE"

    sleep 2
    if svc_active "$AGENT_SERVICE"; then
        ok "探针已启动"
        echo
        journalctl -u "$AGENT_SERVICE" -n 6 --no-pager | sed 's/^/    /'
        echo
        ok "面板上大约 30 秒内会显示这个节点在线。"
    else
        err "探针启动失败，日志如下："
        journalctl -u "$AGENT_SERVICE" -n 30 --no-pager
        return 1
    fi
}

agent_uninstall() {
    need_root
    title "卸载节点端"

    confirm "  确定要卸载探针吗？" || { info "已取消"; return 0; }

    systemctl disable --now "$AGENT_SERVICE" >/dev/null 2>&1
    rm -f "$SYSTEMD_DIR/$AGENT_SERVICE.service"
    systemctl daemon-reload
    rm -f "$AGENT_BIN" "$AGENT_ENV"
    rm -rf "$AGENT_STATE_DIR"

    # 探针正常退出时会自己清掉转发规则，但如果它上次是被 kill -9 的，
    # 规则还留在内核里。这里兜一手，免得卸载完还在转发。
    if command -v nft >/dev/null 2>&1; then
        nft delete table inet vps_forward >/dev/null 2>&1 || true
    fi

    ok "探针已完全移除"
    echo
    info "该节点的流量账本还留在面板上，需要的话去面板里手动删除节点。"
}

# ============================================================================
# 通用的服务操作
# ============================================================================

svc_start()   { need_root; systemctl start "$1"   && ok "$2 已启动"; }
svc_stop()    { need_root; systemctl stop "$1"    && ok "$2 已停止"; }
svc_restart() { need_root; systemctl restart "$1" && ok "$2 已重启"; }

svc_status() {
    local svc="$1" label="$2"
    title "$label 运行状态"
    if ! svc_exists "$svc"; then
        warn "尚未安装"
        return 0
    fi
    systemctl status "$svc" --no-pager -l 2>&1 | head -20
}

svc_logs() {
    local svc="$1"
    need_root
    info "按 Ctrl+C 退出日志"
    journalctl -u "$svc" -n 100 -f --no-pager
}

# agents_state_text 报告探针二进制是否就位。
# 缺了的话节点端一键安装会 404，值得在菜单上直接看到。
agents_state_text() {
    local n
    n="$(ls "$PANEL_DATA/agents"/vps-agent-linux-* 2>/dev/null | wc -l)"
    if [ "$n" -gt 0 ]; then
        printf "%b已就绪 %s 个架构%b" "$C_GRN" "$n" "$C_END"
    else
        printf "%b缺失（选 9 下载）%b" "$C_RED" "$C_END"
    fi
}

svc_state_text() {
    local svc="$1"
    if ! svc_exists "$svc"; then
        printf "%b未安装%b" "$C_YEL" "$C_END"
    elif svc_active "$svc"; then
        printf "%b运行中%b" "$C_GRN" "$C_END"
    else
        printf "%b已停止%b" "$C_RED" "$C_END"
    fi
}

edit_config() {
    local file="$1"
    [ -f "$file" ] || die "配置文件不存在：$file（先执行安装）"
    "${EDITOR:-vi}" "$file"
    echo
    info "改完记得重启服务让配置生效"
}

# ============================================================================
# 交互菜单
# ============================================================================

menu_side() {
    while :; do
        clear 2>/dev/null || true
        cat <<EOF

$(printf "%b" "$C_BLD")  VPS 流量面板 · 安装管理$(printf "%b" "$C_END")
  仓库：https://github.com/${GITHUB_REPO}

  面板端  $(svc_state_text "$PANEL_SERVICE")     节点端  $(svc_state_text "$AGENT_SERVICE")

  要操作哪一端？

    1) 面板端（跑面板的那台机器，一套系统只装一个）
    2) 节点端（被监控的 VPS，每台都要装）

    0) 退出

EOF
        case "$(ask '  请选择' '')" in
            1) menu_panel ;;
            2) menu_agent ;;
            0|q|Q) exit 0 ;;
            *) warn "无效选项"; sleep 1 ;;
        esac
    done
}

menu_panel() {
    while :; do
        clear 2>/dev/null || true
        cat <<EOF

$(printf "%b" "$C_BLD")  面板端管理$(printf "%b" "$C_END")   当前状态：$(svc_state_text "$PANEL_SERVICE")   探针二进制：$(agents_state_text)

    1) 安装 / 升级
    2) 启动
    3) 停止
    4) 重启
    5) 查看状态
    6) 查看日志
    7) 修改配置
    8) 查看初始管理员密码
    9) 下载 / 更新探针二进制（节点端一键安装用）
   10) 卸载

    0) 返回上级

EOF
        case "$(ask '  请选择' '')" in
            1) panel_install ;;
            2) svc_start   "$PANEL_SERVICE" "面板" ;;
            3) svc_stop    "$PANEL_SERVICE" "面板" ;;
            4) svc_restart "$PANEL_SERVICE" "面板" ;;
            5) svc_status  "$PANEL_SERVICE" "面板端" ;;
            6) svc_logs    "$PANEL_SERVICE" ;;
            7) edit_config "$PANEL_ENV" ;;
            8) title "初始管理员密码"
               journalctl -u "$PANEL_SERVICE" --no-pager 2>/dev/null \
                   | grep -A6 '面板初始化完成' | sed 's/^/  /' \
                   || warn "没找到。密码只在首次启动时打印，改过密码后这条记录也不再有意义。" ;;
            9) need_root; download_agents ;;
            10) panel_uninstall ;;
            0|q|Q) return ;;
            *) warn "无效选项"; sleep 1; continue ;;
        esac
        echo
        ask '  按回车继续' '' >/dev/null
    done
}

menu_agent() {
    while :; do
        clear 2>/dev/null || true
        cat <<EOF

$(printf "%b" "$C_BLD")  节点端管理$(printf "%b" "$C_END")   当前状态：$(svc_state_text "$AGENT_SERVICE")

    1) 安装 / 升级
    2) 启动
    3) 停止
    4) 重启
    5) 查看状态
    6) 查看日志
    7) 修改配置（换面板地址、密钥、统计网卡）
    8) 查看本机网卡与当前流量计数
    9) 卸载

    0) 返回上级

EOF
        case "$(ask '  请选择' '')" in
            1) agent_install ;;
            2) svc_start   "$AGENT_SERVICE" "探针" ;;
            3) svc_stop    "$AGENT_SERVICE" "探针" ;;
            4) svc_restart "$AGENT_SERVICE" "探针" ;;
            5) svc_status  "$AGENT_SERVICE" "节点端" ;;
            6) svc_logs    "$AGENT_SERVICE" ;;
            7) edit_config "$AGENT_ENV" ;;
            8) show_ifaces ;;
            9) agent_uninstall ;;
            0|q|Q) return ;;
            *) warn "无效选项"; sleep 1; continue ;;
        esac
        echo
        ask '  按回车继续' '' >/dev/null
    done
}

# show_ifaces 打印网卡和累计收发字节，方便确认探针统计的是不是对的那块网卡
show_ifaces() {
    title "本机网卡"
    local default_if
    default_if="$(awk '$2 == "00000000" { print $1; exit }' /proc/net/route 2>/dev/null)"
    [ -n "$default_if" ] && echo "  默认路由网卡：$default_if（探针不指定 --iface 时统计的就是它）" || \
        warn "  没有找到默认路由"
    echo
    printf "  %-14s %18s %18s\n" "网卡" "累计入站(字节)" "累计出站(字节)"
    awk 'NR>2 {
        gsub(/:/, "", $1);
        printf "  %-14s %18s %18s\n", $1, $2, $10
    }' /proc/net/dev
    echo
    echo "  注：这些是开机以来的累计值。面板做的是差值累加，机器重启后账本不会丢。"
}

# ============================================================================
# 入口
# ============================================================================

usage() {
    cat <<EOF
VPS 流量面板 —— 安装管理脚本

用法：
  bash $0                                    进入交互菜单
  bash $0 panel <动作>                       操作面板端
  bash $0 agent <动作> [选项]                操作节点端

动作：
  install     安装或升级
  agents      下载/更新探针二进制（仅面板端；节点一键安装依赖它）
  start       启动
  stop        停止
  restart     重启
  status      查看状态
  logs        跟踪日志
  uninstall   卸载

节点端安装选项（不传则交互式询问）：
  --server <地址>    面板地址，如 wss://panel.example.com
  --secret <密钥>    节点密钥
  --iface  <网卡>    指定统计网卡，默认自动识别默认路由网卡

环境变量：
  GITHUB_REPO         仓库地址，默认 $GITHUB_REPO
  GITHUB_RELEASE_TAG  版本号，默认 latest
  GITHUB_PROXY        加速前缀，如 https://ghfast.top/
  DOWNLOAD_BASE       直接指定二进制下载地址前缀，绕过 GitHub

示例：
  # 装面板
  sudo bash $0 panel install

  # 批量装探针（不进菜单）
  sudo bash $0 agent install --server wss://panel.example.com --secret abc123

  # 国内机器走加速
  GITHUB_PROXY=https://ghfast.top/ sudo bash $0 agent install
EOF
}

main() {
    if [ $# -eq 0 ]; then
        need_systemd
        menu_side
        return
    fi

    local side="$1"; shift
    local action="${1:-}"; [ $# -gt 0 ] && shift

    case "$side" in
        panel)
            case "$action" in
                install)   panel_install ;;
                agents)    need_root; download_agents ;;
                start)     svc_start   "$PANEL_SERVICE" "面板" ;;
                stop)      svc_stop    "$PANEL_SERVICE" "面板" ;;
                restart)   svc_restart "$PANEL_SERVICE" "面板" ;;
                status)    svc_status  "$PANEL_SERVICE" "面板端" ;;
                logs)      svc_logs    "$PANEL_SERVICE" ;;
                uninstall) panel_uninstall ;;
                *) usage; exit 1 ;;
            esac ;;
        agent)
            case "$action" in
                install)   agent_install "$@" ;;
                start)     svc_start   "$AGENT_SERVICE" "探针" ;;
                stop)      svc_stop    "$AGENT_SERVICE" "探针" ;;
                restart)   svc_restart "$AGENT_SERVICE" "探针" ;;
                status)    svc_status  "$AGENT_SERVICE" "节点端" ;;
                logs)      svc_logs    "$AGENT_SERVICE" ;;
                uninstall) agent_uninstall ;;
                *) usage; exit 1 ;;
            esac ;;
        -h|--help|help) usage ;;
        *) usage; exit 1 ;;
    esac
}

main "$@"
