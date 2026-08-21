// 总览页与节点详情页。

import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import {
    api, fmtAgo, fmtBytes, fmtPercent, fmtRate, fmtTime, fmtUptime, toast,
} from "./api.js";
import { QuotaMeter, StatTile, StatusBadge, TrafficChart } from "./components.js";

// OverviewView 是首页：一屏看清所有节点在线情况和流量水位。
export const OverviewView = {
    components: { StatTile, StatusBadge, QuotaMeter },
    emits: ["open-node"],
    setup() {
        const data = ref(null);
        const loading = ref(true);
        const error = ref("");
        let timer = null;

        async function load(silent = false) {
            if (!silent) loading.value = true;
            try {
                data.value = await api("/api/overview");
                error.value = "";
            } catch (e) {
                error.value = e.message;
            } finally {
                loading.value = false;
            }
        }

        onMounted(() => {
            load();
            // 5 秒刷一次。数据都在面板内存/SQLite 里，这个频率很轻
            timer = setInterval(() => load(true), 5000);
        });
        onBeforeUnmount(() => clearInterval(timer));

        const summary = computed(() => data.value?.summary || {});
        const nodes = computed(() => data.value?.nodes || []);
        const events = computed(() => data.value?.recent_events || []);

        return {
            data, loading, error, summary, nodes, events, load,
            fmtBytes, fmtRate, fmtAgo, fmtTime, fmtPercent, fmtUptime,
        };
    },
    template: `
        <div>
            <div class="page-head">
                <div>
                    <h1>总览</h1>
                    <p>共 {{ summary.total || 0 }} 个节点 · 每 5 秒自动刷新</p>
                </div>
                <button class="btn" @click="load()">刷新</button>
            </div>

            <div v-if="error" class="notice error">{{ error }}</div>

            <div class="grid grid-stats" style="margin-bottom:16px">
                <StatTile label="在线节点" :value="(summary.online || 0) + ' / ' + (summary.total || 0)"
                          :sub="(summary.offline || 0) + ' 个离线'" />
                <StatTile label="流量超额" :value="summary.exceeded || 0"
                          :sub="(summary.warning || 0) + ' 个已达预警线'" />
                <StatTile label="当前总带宽" :value="fmtRate(summary.total_rate || 0)"
                          sub="所有在线节点上下行合计" />
                <StatTile label="本周期总用量"
                          :value="fmtBytes((summary.total_rx || 0) + (summary.total_tx || 0))"
                          :sub="'入 ' + fmtBytes(summary.total_rx || 0) + ' / 出 ' + fmtBytes(summary.total_tx || 0)" />
            </div>

            <div v-if="loading && !nodes.length" class="empty"><span class="spinner"></span> 加载中…</div>
            <div v-else-if="!nodes.length" class="empty">
                还没有添加节点。到「节点管理」新建一个，然后把探针装到 VPS 上。
            </div>

            <div v-else class="grid grid-nodes">
                <div v-for="n in nodes" :key="n.id" class="node-card">
                    <div class="node-card-head">
                        <div>
                            <div class="node-name" @click="$emit('open-node', n.id)">{{ n.name }}</div>
                            <div class="node-meta">{{ n.ipv4 || n.ipv6 || '未填写 IP' }}</div>
                        </div>
                        <StatusBadge :status="n.status" />
                    </div>

                    <QuotaMeter :used="n.quota_status.billed_bytes" :quota="n.quota_bytes"
                                :warn-percent="n.warn_percent"
                                :label="n.billing_mode_label" />

                    <div class="node-rates">
                        <span v-if="n.live && n.live.connected">
                            ↓ {{ fmtRate(n.live.rx_rate) }} &nbsp; ↑ {{ fmtRate(n.live.tx_rate) }}
                        </span>
                        <span v-else class="muted">探针未连接</span>
                    </div>

                    <div class="node-meta">
                        心跳 {{ fmtAgo(n.last_seen) }}
                        <template v-if="n.live && n.live.uptime"> · 已运行 {{ fmtUptime(n.live.uptime) }}</template>
                        <template v-if="n.cycle_end"> · {{ fmtTime(n.cycle_end) }} 清零</template>
                    </div>
                </div>
            </div>

            <div class="card" style="margin-top:16px">
                <div class="card-title">最近事件</div>
                <div class="table-scroll">
                    <table>
                        <thead><tr><th style="width:150px">时间</th><th style="width:70px">级别</th><th>内容</th></tr></thead>
                        <tbody>
                            <tr v-for="e in events" :key="e.id">
                                <td class="tabular muted">{{ fmtTime(e.created_at, true) }}</td>
                                <td>
                                    <span class="badge" :class="e.level === 'error' ? 'critical' : (e.level === 'warn' ? 'warning' : 'muted')">
                                        <span class="dot"></span>{{ e.level === 'error' ? '错误' : (e.level === 'warn' ? '警告' : '信息') }}
                                    </span>
                                </td>
                                <td style="white-space:pre-wrap">{{ e.message }}</td>
                            </tr>
                            <tr v-if="!events.length"><td colspan="3" class="muted">暂无事件</td></tr>
                        </tbody>
                    </table>
                </div>
            </div>
        </div>
    `,
};

// NodeDetailView 是单个节点的详情：流量曲线、历史周期、事件、快捷操作。
export const NodeDetailView = {
    components: { TrafficChart, QuotaMeter, StatusBadge, StatTile },
    props: { nodeId: Number },
    emits: ["back", "edit"],
    setup(props) {
        const node = ref(null);
        const traffic = ref({ points: [], bucket: "hour" });
        const cycles = ref([]);
        const events = ref([]);
        const install = ref(null);
        const range = ref("24h");
        const loading = ref(true);
        const busy = ref("");
        let timer = null;

        async function loadNode() {
            node.value = await api(`/api/nodes/${props.nodeId}`);
        }

        async function loadTraffic() {
            traffic.value = await api(`/api/nodes/${props.nodeId}/traffic?range=${range.value}`);
        }

        async function loadAll() {
            loading.value = true;
            try {
                await Promise.all([
                    loadNode(),
                    loadTraffic(),
                    api(`/api/nodes/${props.nodeId}/cycles`).then((v) => (cycles.value = v || [])),
                    api(`/api/events?node_id=${props.nodeId}&limit=60`).then((v) => (events.value = v || [])),
                ]);
            } catch (e) {
                toast(e.message, "error");
            } finally {
                loading.value = false;
            }
        }

        async function setRange(r) {
            range.value = r;
            try {
                await loadTraffic();
            } catch (e) {
                toast(e.message, "error");
            }
        }

        async function showInstall() {
            try {
                install.value = await api(`/api/nodes/${props.nodeId}/install`);
            } catch (e) {
                toast(e.message, "error");
            }
        }

        async function act(name, path, body, confirmMsg) {
            if (confirmMsg && !window.confirm(confirmMsg)) return;
            busy.value = name;
            try {
                const res = await api(path, { method: "POST", body: body || {} });
                toast(res?.message || "操作完成", "success");
                await loadNode();
            } catch (e) {
                toast(e.message, "error");
            } finally {
                busy.value = "";
            }
        }

        async function testSSH() {
            busy.value = "ssh";
            try {
                const res = await api(`/api/nodes/${props.nodeId}/test-ssh`, { method: "POST", body: {} });
                if (res.ok) {
                    toast(res.message + (res.host_key_saved ? "（已记录主机密钥）" : ""), "success", 6000);
                } else {
                    toast("SSH 测试失败：" + res.error, "error", 9000);
                }
                await loadNode();
            } catch (e) {
                toast(e.message, "error");
            } finally {
                busy.value = "";
            }
        }

        onMounted(() => {
            loadAll();
            timer = setInterval(() => loadNode().catch(() => {}), 5000);
        });
        onBeforeUnmount(() => clearInterval(timer));

        const live = computed(() => node.value?.live);

        return {
            node, traffic, cycles, events, install, range, loading, busy, live,
            setRange, showInstall, act, testSSH, loadAll,
            fmtBytes, fmtRate, fmtTime, fmtAgo, fmtPercent, fmtUptime,
        };
    },
    template: `
        <div v-if="node">
            <div class="page-head">
                <div>
                    <h1>
                        {{ node.name }}
                        <StatusBadge :status="node.status" style="vertical-align:4px;margin-left:6px" />
                    </h1>
                    <p>
                        {{ node.ipv4 || '—' }}
                        <template v-if="node.remark"> · {{ node.remark }}</template>
                        · 计费口径：{{ node.billing_mode_label }}
                        · 超额动作：{{ node.action_label }}
                    </p>
                </div>
                <div class="btn-row">
                    <button class="btn" @click="$emit('back')">返回</button>
                    <button class="btn" @click="showInstall">安装命令</button>
                    <button class="btn" @click="$emit('edit', node.id)">编辑</button>
                </div>
            </div>

            <div class="grid grid-stats" style="margin-bottom:16px">
                <StatTile label="本周期计费流量" :value="fmtBytes(node.quota_status.billed_bytes)"
                          :sub="node.quota_bytes > 0 ? '配额 ' + fmtBytes(node.quota_bytes) : '不限量'" />
                <StatTile label="入站 / 出站" compact
                          :value="fmtBytes(node.usage.rx_bytes) + ' / ' + fmtBytes(node.usage.tx_bytes)"
                          sub="原始网卡收发字节" />
                <StatTile label="当前速率"
                          :value="live && live.connected ? ('↓' + fmtRate(live.rx_rate)) : '—'"
                          :sub="live && live.connected ? ('↑' + fmtRate(live.tx_rate) + ' · 网卡 ' + (live.iface || '?')) : '探针未连接'" />
                <StatTile label="周期进度" :value="fmtPercent(node.cycle_progress * 100, 0)"
                          :sub="fmtTime(node.cycle_end) + ' 清零'" />
            </div>

            <!-- 转发占用只在这台机器真的在转发时才显示，别给没用转发的用户增加噪音。 -->
            <div v-if="node.forward_share > 0" class="notice" style="margin-bottom:16px">
                本周期的 {{ fmtBytes(node.quota_status.billed_bytes) }} 计费流量里，约
                <b>{{ fmtBytes(node.forward_share) }}</b> 来自端口转发，
                其余 <b>{{ fmtBytes(Math.max(0, node.quota_status.billed_bytes - node.forward_share)) }}</b>
                是本机自己产生的。
                <span class="muted">
                    中转流量在网卡上进出各走一遍，所以这个数字是转发规则计数的两倍（双向计费口径下）。
                </span>
            </div>

            <div class="card">
                <div class="card-title">
                    <span>流量曲线</span>
                    <div class="seg">
                        <button v-for="r in [['24h','24 小时'],['7d','7 天'],['30d','30 天'],['cycle','本周期']]"
                                :key="r[0]" :class="{ active: range === r[0] }" @click="setRange(r[0])">
                            {{ r[1] }}
                        </button>
                    </div>
                </div>
                <TrafficChart :points="traffic.points || []" :bucket="traffic.bucket" />
            </div>

            <div class="card">
                <div class="card-title">配额与操作</div>
                <QuotaMeter :used="node.quota_status.billed_bytes" :quota="node.quota_bytes"
                            :warn-percent="node.warn_percent" />
                <div class="btn-row" style="margin-top:16px">
                    <button class="btn" :disabled="busy === 'reset'"
                            @click="act('reset', '/api/nodes/' + node.id + '/reset', {}, '确定要立即清零该节点的流量吗？当前周期数据会被归档到历史记录。')">
                        立即清零流量
                    </button>
                    <button class="btn danger" :disabled="busy === 'shutdown'"
                            @click="act('shutdown', '/api/nodes/' + node.id + '/shutdown', {}, '确定要关闭「' + node.name + '」吗？\\n关机后需要你到服务商后台手动开机。')">
                        立即关机
                    </button>
                    <button class="btn" :disabled="busy === 'ssh' || !node.ssh_host" @click="testSSH">
                        测试 SSH 连接
                    </button>
                    <span v-if="busy" class="muted" style="align-self:center"><span class="spinner"></span> 执行中…</span>
                </div>
                <div v-if="!node.has_ssh_secret" class="field-hint" style="margin-top:10px">
                    这个节点还没配置 SSH 凭据。配上之后，即使探针进程已经挂掉，面板仍然能远程把机器关掉。
                </div>
                <div v-if="node.ssh_host_key" class="field-hint" style="margin-top:6px">
                    已记录 SSH 主机密钥。若这台机器重装过系统，需要在编辑里清空该字段后重新测试连接。
                </div>
            </div>

            <div class="card">
                <div class="card-title">历史账单周期</div>
                <div class="table-scroll">
                    <table>
                        <thead>
                            <tr>
                                <th>周期</th><th>口径</th>
                                <th class="num">入站</th><th class="num">出站</th>
                                <th class="num">计费流量</th><th class="num">配额</th>
                            </tr>
                        </thead>
                        <tbody>
                            <tr v-for="c in cycles" :key="c.id">
                                <td class="tabular">{{ fmtTime(c.cycle_start) }} → {{ fmtTime(c.cycle_end) }}</td>
                                <td class="muted">{{ c.billing_mode }}</td>
                                <td class="num">{{ fmtBytes(c.rx_bytes) }}</td>
                                <td class="num">{{ fmtBytes(c.tx_bytes) }}</td>
                                <td class="num">{{ fmtBytes(c.billed_bytes) }}</td>
                                <td class="num">{{ c.quota_bytes > 0 ? fmtBytes(c.quota_bytes) : '不限' }}</td>
                            </tr>
                            <tr v-if="!cycles.length"><td colspan="6" class="muted">还没有归档的历史周期</td></tr>
                        </tbody>
                    </table>
                </div>
            </div>

            <div class="card">
                <div class="card-title">该节点的事件</div>
                <div class="table-scroll" style="max-height:420px;overflow-y:auto">
                    <table>
                        <thead><tr><th style="width:150px">时间</th><th style="width:70px">级别</th><th>内容</th></tr></thead>
                        <tbody>
                            <tr v-for="e in events" :key="e.id">
                                <td class="tabular muted">{{ fmtTime(e.created_at, true) }}</td>
                                <td>
                                    <span class="badge" :class="e.level === 'error' ? 'critical' : (e.level === 'warn' ? 'warning' : 'muted')">
                                        <span class="dot"></span>{{ e.level === 'error' ? '错误' : (e.level === 'warn' ? '警告' : '信息') }}
                                    </span>
                                </td>
                                <td style="white-space:pre-wrap">{{ e.message }}</td>
                            </tr>
                            <tr v-if="!events.length"><td colspan="3" class="muted">暂无事件</td></tr>
                        </tbody>
                    </table>
                </div>
            </div>

            <div v-if="install" class="modal-mask" @click.self="install = null">
                <div class="modal">
                    <h2>在节点上安装探针</h2>
                    <p class="field-hint" style="margin-bottom:10px">
                        SSH 登录到这台 VPS，用 root 执行下面的命令。脚本会下载探针、注册 systemd 服务并启动。
                    </p>
                    <div class="code-block">{{ install.install_cmd }}</div>
                    <p class="field-hint" style="margin:14px 0 6px">卸载：</p>
                    <div class="code-block">{{ install.uninstall }}</div>
                    <p class="field-hint" style="margin:14px 0 6px">节点密钥（等同于该节点的上报凭证，不要外泄）：</p>
                    <div class="code-block">{{ install.secret }}</div>
                    <div class="modal-actions">
                        <button class="btn" @click="install = null">关闭</button>
                    </div>
                </div>
            </div>
        </div>
        <div v-else class="empty"><span class="spinner"></span> 加载中…</div>
    `,
};
