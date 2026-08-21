#!/usr/bin/env bash
#
# 本地端到端验收。跑完会明确告诉你每一条是否通过。
#
# 覆盖的场景：
#   1. 探针上报 → 面板账本累加
#   2. 机器重启（boot_id 变化）→ 账本继续累加而不是归零   ← 这是本项目最关键的一条
#   3. 面板重启 → 账本不丢
#   4. 流量达到预警线 → 产生预警事件
#   5. 流量耗尽 → 状态转为超额、写入事件
#   6. 手动清零 → 用量归零、历史周期归档、状态恢复
#   7. 单向计费口径（出入取大）确实按较大方向算
#
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PANEL="$ROOT/bin/panel"
AGENT="$ROOT/bin/vps-agent"
WORK="$(mktemp -d)"

# 挑一个确实空闲的端口。写死端口在共享环境里会撞上别人（比如 docker-proxy），
# 那种情况下健康检查会连到别人的服务上，症状非常难懂。
find_free_port() {
    local p
    for p in $(seq 18300 18400); do
        if ! (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then
            echo "$p"
            return 0
        fi
        exec 3<&- 2>/dev/null
    done
    return 1
}
PORT="${E2E_PORT:-$(find_free_port)}"
[ -n "$PORT" ] || { echo "找不到空闲端口" >&2; exit 1; }
BASE="http://127.0.0.1:$PORT"

PASS=0
FAIL=0

pass() { PASS=$((PASS + 1)); printf '  \033[32m✓\033[0m %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  \033[31m✗\033[0m %s\n' "$1"; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }
note() { printf '    %s\n' "$1"; }

cleanup() {
    [ -n "${AGENT_PID:-}" ] && kill "$AGENT_PID" 2>/dev/null
    [ -n "${PANEL_PID:-}" ] && kill "$PANEL_PID" 2>/dev/null
    wait 2>/dev/null
    rm -rf "$WORK"
}
trap cleanup EXIT

if [ ! -x "$PANEL" ] || [ ! -x "$AGENT" ]; then
    echo "找不到二进制，请先执行 make build" >&2
    exit 1
fi

start_panel() {
    "$PANEL" --listen "127.0.0.1:$PORT" --data-dir "$WORK/data" \
        --base-url "$BASE" --log-level warn >"$WORK/panel.log" 2>&1 &
    PANEL_PID=$!
    for _ in $(seq 1 60); do
        if ! kill -0 "$PANEL_PID" 2>/dev/null; then
            echo "面板进程已退出，日志：" >&2
            cat "$WORK/panel.log" >&2
            return 1
        fi
        if curl -fsS "$BASE/api/health" 2>/dev/null | grep -q '"status":"ok"'; then
            return 0
        fi
        sleep 0.2
    done
    echo "面板启动超时，日志：" >&2
    cat "$WORK/panel.log" >&2
    return 1
}

login() {
    # 首次启动时面板会把随机初始密码打到 stdout。密码是 16 位 base64url 字符。
    if [ -z "${PASSWORD:-}" ]; then
        PASSWORD=$(sed -n 's/^.*：\([A-Za-z0-9_-]\{16,\}\)[[:space:]]*$/\1/p' "$WORK/panel.log" | head -1)
    fi
    local pw="$PASSWORD"
    if [ -z "$pw" ]; then
        echo "没能从启动日志里解析出初始密码" >&2
        cat "$WORK/panel.log" >&2
        return 1
    fi
    TOKEN=$(curl -fsS -X POST "$BASE/api/auth/login" \
        -H 'Content-Type: application/json' \
        -d "{\"username\":\"admin\",\"password\":\"$pw\"}" | jq -r .token)
    [ -n "$TOKEN" ] && [ "$TOKEN" != "null" ]
}

# api METHOD PATH [BODY]
api() {
    local method="$1" path="$2" body="${3:-}"
    if [ -n "$body" ]; then
        curl -fsS -X "$method" "$BASE$path" \
            -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d "$body"
    else
        curl -fsS -X "$method" "$BASE$path" -H "Authorization: Bearer $TOKEN"
    fi
}

node_field() { api GET "/api/nodes/$1" | jq -r "$2"; }

start_agent() {
    local secret="$1" bootid="$2" rate="$3"
    VPS_AGENT_BOOT_ID="$bootid" "$AGENT" \
        --server "$BASE" --secret "$secret" \
        --fake-traffic "$rate" --interval 1s --log-level warn \
        >>"$WORK/agent.log" 2>&1 &
    AGENT_PID=$!
    # 从作业表里摘掉，免得后面 kill -9 时 bash 往终端刷 "Killed" 提示
    disown "$AGENT_PID" 2>/dev/null || true
}

stop_agent() {
    [ -n "${AGENT_PID:-}" ] && kill -9 "$AGENT_PID" 2>/dev/null
    AGENT_PID=""
    sleep 0.3
}

for dep in curl jq; do
    command -v "$dep" >/dev/null 2>&1 || { echo "缺少依赖: $dep" >&2; exit 1; }
done

# ---------------------------------------------------------------- 开始

echo "工作目录: $WORK"

step "1/7 启动面板并登录"
start_panel || exit 1
login || exit 1
pass "面板已启动，管理员登录成功"

step "2/7 创建节点并让探针上报"
# 配额 200MiB，单向口径（出入取大），预警线 50%
NODE=$(api POST /api/nodes '{
  "name":"e2e-节点A","remark":"端到端测试","ipv4":"192.0.2.10","ipv6":"",
  "quota_bytes":209715200,"billing_mode":"max","traffic_ratio":1,"warn_percent":50,
  "reset_day":1,"reset_tz":"UTC",
  "action_on_exceed":"none","exceed_command":"","auto_recover_on_reset":true,
  "ssh_host":"","ssh_port":22,"ssh_user":"root","ssh_auth":"password",
  "ssh_secret":"","ssh_key_pass":"","ssh_host_key":"",
  "ssh_use_sudo":false,"probe_port":0,"enabled":true}')
NID=$(echo "$NODE" | jq -r .id)
SECRET=$(echo "$NODE" | jq -r .secret)
[ -n "$NID" ] && [ "$NID" != "null" ] && pass "节点已创建 (id=$NID)" || { fail "创建节点失败"; exit 1; }

# 10MiB/s 的伪造流量：入站按这个速率涨，出站是它的一半
start_agent "$SECRET" "boot-round-1" "10MB/s"
sleep 4

RX1=$(node_field "$NID" .usage.rx_bytes)
TX1=$(node_field "$NID" .usage.tx_bytes)
STATUS1=$(node_field "$NID" .status)
note "累计 入站=$RX1 出站=$TX1 状态=$STATUS1"
[ "$RX1" -gt 0 ] && pass "面板已记到流量" || fail "面板没有记到任何流量"
[ "$STATUS1" = "online" ] && pass "节点状态为在线" || fail "节点状态是 $STATUS1，期望 online"

BILLED1=$(node_field "$NID" .quota_status.billed_bytes)
if [ "$BILLED1" = "$RX1" ] && [ "$RX1" -gt "$TX1" ]; then
    pass "单向口径生效：计费流量取了较大的入站方向"
else
    fail "单向口径不对：计费=$BILLED1 入站=$RX1 出站=$TX1"
fi

step "3/7 模拟机器重启（boot_id 变化，网卡计数器归零）"
stop_agent
RX_BEFORE=$(node_field "$NID" .usage.rx_bytes)
note "重启前账本入站=$RX_BEFORE"

# 换一个 boot_id 重新起，等价于机器重启：计数器从 0 重新开始
start_agent "$SECRET" "boot-round-2" "10MB/s"
sleep 4

RX_AFTER=$(node_field "$NID" .usage.rx_bytes)
note "重启后账本入站=$RX_AFTER"
if [ "$RX_AFTER" -gt "$RX_BEFORE" ]; then
    pass "重启后账本继续累加，没有归零（这是本项目的核心保证）"
else
    fail "重启后账本没有继续增长：$RX_BEFORE → $RX_AFTER"
fi

REBOOT_EVT=$(api GET "/api/events?node_id=$NID" | jq -r '[.[] | select(.message | test("检测到机器重启"))] | length')
[ "$REBOOT_EVT" -ge 1 ] && pass "事件日志里记录了重启检测" || fail "没有记录重启事件"

step "4/7 模拟面板重启"
stop_agent
RX_B4_PANEL=$(node_field "$NID" .usage.rx_bytes)
kill "$PANEL_PID" 2>/dev/null; wait "$PANEL_PID" 2>/dev/null; PANEL_PID=""
sleep 0.5
start_panel || exit 1
login || exit 1

RX_AFTER_PANEL=$(node_field "$NID" .usage.rx_bytes)
if [ "$RX_AFTER_PANEL" = "$RX_B4_PANEL" ]; then
    pass "面板重启后账本完好（$RX_AFTER_PANEL 字节）"
else
    fail "面板重启后账本变了：$RX_B4_PANEL → $RX_AFTER_PANEL"
fi

step "5/7 跑到预警线与配额上限"
# 用很高的伪造速率快速把 200MiB 配额吃掉
start_agent "$SECRET" "boot-round-3" "80MB/s"

EXCEEDED=""
for _ in $(seq 1 40); do
    sleep 1
    S=$(node_field "$NID" .status)
    if [ "$S" = "exceeded" ]; then EXCEEDED=yes; break; fi
done

if [ "$EXCEEDED" = "yes" ]; then
    pass "流量耗尽后节点状态转为 exceeded"
else
    fail "等待 40 秒仍未触发超额，当前状态 $(node_field "$NID" .status)，用量 $(node_field "$NID" .quota_status.billed_bytes)"
fi

WARN_EVT=$(api GET "/api/events?node_id=$NID&level=warn" | jq -r '[.[] | select(.type=="traffic_warn")] | length')
[ "$WARN_EVT" -ge 1 ] && pass "产生了流量预警事件" || fail "没有产生预警事件"

EXCEED_EVT=$(api GET "/api/events?node_id=$NID&level=error" | jq -r '[.[] | select(.type=="traffic_exceed")] | length')
[ "$EXCEED_EVT" -ge 1 ] && pass "产生了流量耗尽事件" || fail "没有产生耗尽事件"

# 超额动作设为 none，所以不应该有关机记录
ACT_EVT=$(api GET "/api/events?node_id=$NID" | jq -r '[.[] | select(.type=="action_run")] | length')
[ "$ACT_EVT" = "0" ] && pass "超额动作设为「不处理」时确实没动机器" || fail "不该执行任何动作，却有 $ACT_EVT 条动作记录"

step "6/7 探针心跳不会把超额状态刷回在线"
sleep 2
S=$(node_field "$NID" .status)
[ "$S" = "exceeded" ] && pass "超额状态没有被后续心跳覆盖" || fail "状态被刷成了 $S"

step "7/7 手动清零"
stop_agent
BEFORE_RESET=$(node_field "$NID" .quota_status.billed_bytes)
api POST "/api/nodes/$NID/reset" '{}' >/dev/null

AFTER_RX=$(node_field "$NID" .usage.rx_bytes)
AFTER_STATUS=$(node_field "$NID" .status)
CYCLES=$(api GET "/api/nodes/$NID/cycles" | jq -r 'length')
ARCHIVED=$(api GET "/api/nodes/$NID/cycles" | jq -r '.[0].billed_bytes')

[ "$AFTER_RX" = "0" ] && pass "用量已清零" || fail "清零后用量仍为 $AFTER_RX"
[ "$CYCLES" -ge 1 ] && pass "上一周期已归档（$CYCLES 条历史记录）" || fail "没有归档历史周期"
[ "$ARCHIVED" = "$BEFORE_RESET" ] && pass "归档的计费流量与清零前一致（$ARCHIVED）" \
    || fail "归档数据对不上：清零前 $BEFORE_RESET，归档 $ARCHIVED"
[ "$AFTER_STATUS" != "exceeded" ] && pass "超额限制已解除（状态 $AFTER_STATUS）" \
    || fail "清零后仍处于超额状态"

# 前端资源确实打进了二进制
if curl -fsS "$BASE/" | grep -q 'VPS 流量面板'; then
    pass "内嵌前端可正常访问"
else
    fail "内嵌前端访问失败"
fi

if curl -fsS "$BASE/agent/install.sh?secret=$SECRET" | grep -q 'systemctl'; then
    pass "一键安装脚本生成正常"
else
    fail "一键安装脚本生成失败"
fi

# ---------------------------------------------------------------- 汇总

printf '\n\033[1m结果：通过 %d 项，失败 %d 项\033[0m\n' "$PASS" "$FAIL"
if [ "$FAIL" -gt 0 ]; then
    echo
    echo "面板日志（末尾 40 行）："
    tail -40 "$WORK/panel.log"
    exit 1
fi
exit 0
