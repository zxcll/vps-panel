// 端口转发页面：规则列表 + 链路编辑 + 节点的中继配置。
import { computed, onBeforeUnmount, onMounted, reactive, ref } from "vue";
import { api, fmtBytes, toast, FORWARD_MODES, FORWARD_PROTOCOLS } from "./api.js";
import { Modal } from "./components.js";

// ForwardEditor 是规则的新建/编辑弹窗，核心是那张跳序表。
const ForwardEditor = {
    components: { Modal },
    props: { rule: Object, nodes: Array },
    emits: ["close", "saved"],
    setup(props, { emit }) {
        const isEdit = computed(() => !!props.rule);

        const form = reactive({
            name: props.rule?.name || "",
            proto: props.rule?.proto || "tcp",
            dest_host: props.rule?.dest_host || "",
            dest_port: props.rule?.dest_port || 443,
            enabled: props.rule ? props.rule.enabled : true,
            remark: props.rule?.remark || "",
        });

        // 跳序在本地是一个普通数组，顺序即链路顺序，保存时按下标写 position。
        const hops = ref(
            (props.rule?.hops || []).map((h) => ({
                node_id: h.node_id,
                listen_port: h.listen_port,
                mode: h.mode || "kernel",
                bandwidth_mbps: h.bandwidth_mbps || 0,
            })),
        );

        const saving = ref(false);
        const err = ref("");
        const addSelect = ref("");

        // 一个节点在同一条链里只能出现一次，所以下拉里要把已选的排掉。
        const available = computed(() =>
            (props.nodes || []).filter((n) => !hops.value.some((h) => h.node_id === n.id)),
        );

        function nodeName(id) {
            const n = (props.nodes || []).find((x) => x.id === id);
            return n ? n.name : `节点 #${id}`;
        }

        function addHop(id) {
            if (!id) return;
            hops.value.push({
                node_id: Number(id),
                // 一律默认 0：由面板从该节点配置的端口范围里随机挑一个。
                // 想指定入口端口（比如客户端配置里已经写死了）就自己改成具体数字。
                listen_port: 0,
                mode: "kernel",
                bandwidth_mbps: 0,
            });
        }

        function removeHop(i) {
            hops.value.splice(i, 1);
        }

        function move(i, delta) {
            const j = i + delta;
            if (j < 0 || j >= hops.value.length) return;
            const arr = hops.value;
            [arr[i], arr[j]] = [arr[j], arr[i]];
        }

        // UDP 不支持用户态转发，切成 UDP 时把已选的用户态跳改回内核态，
        // 免得用户保存时才被后端拒掉。
        function onProtoChange() {
            if (form.proto !== "udp") return;
            let changed = false;
            for (const h of hops.value) {
                if (h.mode === "userspace") {
                    h.mode = "kernel";
                    changed = true;
                }
            }
            if (changed) toast("UDP 不支持用户态转发，已自动改回内核态", "info");
        }

        async function save() {
            err.value = "";
            const body = {
                name: form.name,
                proto: form.proto,
                dest_host: form.dest_host,
                dest_port: Number(form.dest_port),
                enabled: form.enabled,
                remark: form.remark,
                hops: hops.value.map((h) => ({
                    node_id: Number(h.node_id),
                    listen_port: Number(h.listen_port) || 0,
                    mode: h.mode,
                    bandwidth_mbps: Number(h.bandwidth_mbps) || 0,
                })),
            };
            saving.value = true;
            try {
                if (isEdit.value) await api(`/api/forwards/${props.rule.id}`, { method: "PUT", body });
                else await api("/api/forwards", { method: "POST", body });
                toast("已保存，正在下发到节点", "success");
                emit("saved");
            } catch (e) {
                err.value = e.message;
            } finally {
                saving.value = false;
            }
        }

        const canSave = computed(
            () => form.name.trim() && form.dest_host.trim() && form.dest_port && hops.value.length > 0,
        );

        return {
            form, hops, saving, err, save, isEdit, available, addSelect,
            addHop, removeHop, move, nodeName, canSave, onProtoChange,
            FORWARD_MODES, FORWARD_PROTOCOLS,
        };
    },
    template: `
        <Modal :title="isEdit ? '编辑转发规则' : '添加转发规则'" wide @close="$emit('close')">
            <div v-if="err" class="notice error">{{ err }}</div>

            <fieldset>
                <legend>基本信息</legend>
                <div class="field-row">
                    <div class="field">
                        <label>规则名称 *</label>
                        <input type="text" v-model="form.name" placeholder="香港中转">
                    </div>
                    <div class="field">
                        <label>协议</label>
                        <select v-model="form.proto" @change="onProtoChange">
                            <option v-for="p in FORWARD_PROTOCOLS" :key="p.value" :value="p.value">{{ p.label }}</option>
                        </select>
                    </div>
                </div>
                <div class="field-row">
                    <div class="field">
                        <label>最终目标地址 *</label>
                        <input type="text" v-model="form.dest_host" placeholder="1.2.3.4 或 target.example.com">
                        <div class="field-hint">填域名的话探针会定期重解析，目标 IP 变了自动跟上</div>
                    </div>
                    <div class="field">
                        <label>目标端口 *</label>
                        <input type="number" min="1" max="65535" v-model="form.dest_port">
                    </div>
                </div>
                <div class="field">
                    <label>备注</label>
                    <input type="text" v-model="form.remark" placeholder="选填，只给自己看">
                </div>
            </fieldset>

            <fieldset>
                <legend>链路（顺序即转发顺序，第一跳是入口）</legend>
                <div v-if="!hops.length" class="field-hint" style="margin-bottom:10px">
                    还没有选节点。至少加一个 —— 单跳就是「入口机器直接转到目标」，
                    多跳则是「入口 → 中转 → … → 目标」。
                </div>
                <div class="table-scroll" v-else>
                    <table>
                        <thead>
                            <tr>
                                <th style="width:52px">顺序</th>
                                <th>节点</th>
                                <th style="width:130px">监听端口</th>
                                <th style="width:180px">转发模式</th>
                                <th style="width:120px">限速</th>
                                <th style="width:170px">调整</th>
                            </tr>
                        </thead>
                        <tbody>
                            <tr v-for="(h, i) in hops" :key="h.node_id">
                                <td class="tabular">{{ i === 0 ? '入口' : i + 1 }}</td>
                                <td>{{ nodeName(h.node_id) }}</td>
                                <td>
                                    <input type="number" min="0" max="65535" v-model="h.listen_port"
                                           placeholder="0 = 自动分配">
                                </td>
                                <td>
                                    <select v-model="h.mode">
                                        <option v-for="m in FORWARD_MODES" :key="m.value" :value="m.value"
                                                :disabled="form.proto === 'udp' && m.value === 'userspace'">
                                            {{ m.label }}
                                        </option>
                                    </select>
                                </td>
                                <td>
                                    <input type="number" min="0" v-model="h.bandwidth_mbps" placeholder="0 = 不限">
                                </td>
                                <td>
                                    <div class="btn-row">
                                        <button class="btn small" :disabled="i === 0" @click="move(i, -1)">上移</button>
                                        <button class="btn small" :disabled="i === hops.length - 1" @click="move(i, 1)">下移</button>
                                        <button class="btn small danger" @click="removeHop(i)">移除</button>
                                    </div>
                                </td>
                            </tr>
                        </tbody>
                    </table>
                </div>
                <div class="field-row" style="margin-top:12px">
                    <div class="field" style="margin-bottom:0">
                        <select v-model="addSelect" @change="addHop(addSelect); addSelect = ''">
                            <option value="">＋ 添加一跳…</option>
                            <option v-for="n in available" :key="n.id" :value="n.id">{{ n.name }}</option>
                        </select>
                        <div class="field-hint">
                            <b>监听端口</b>填 0（默认）就由面板从该节点「转发设置」里配的端口范围内随机挑一个；
                            客户端配置里已经写死了某个端口的话，填具体数字。<br>
                            <b>内核态</b>：包在内核里改个目标地址就直接转走，不经过任何用户进程，
                            开销最低、速度最快，TCP/UDP 都支持。默认用它。<br>
                            <b>用户态</b>：探针自己收下连接、再单独建一条到下一跳的连接。
                            多跳串联时能避免 TCP 套 TCP 导致的拥塞叠加（表现是丢包时速度雪崩），
                            还带预连接池省掉一次握手往返。代价是要过一遍用户进程，只支持 TCP。
                            单跳一般用内核态；跨境多跳且线路不稳时，中间那几跳用用户态往往更快更稳。<br>
                            <b>限速</b>单位 Mbps，0 表示不限，只作用于出方向。
                        </div>
                    </div>
                </div>
                <label class="checkbox-label" style="margin-top:10px">
                    <input type="checkbox" v-model="form.enabled"> 启用这条规则
                </label>
            </fieldset>

            <div class="modal-actions">
                <button class="btn" @click="$emit('close')">取消</button>
                <button class="btn primary" :disabled="saving || !canSave" @click="save">
                    <span v-if="saving" class="spinner"></span>保存并下发
                </button>
            </div>
        </Modal>
    `,
};

// ForwardNodeEditor 编辑单个节点的转发配置：中继地址和自动分配端口范围。
const ForwardNodeEditor = {
    components: { Modal },
    props: { item: Object },
    emits: ["close", "saved"],
    setup(props, { emit }) {
        const form = reactive({
            relay_host: props.item.relay_host || "",
            relay_host_v6: props.item.relay_host_v6 || "",
            port_start: props.item.port_start || 20000,
            port_end: props.item.port_end || 29999,
            enabled: props.item.enabled,
        });
        const saving = ref(false);
        const err = ref("");

        async function save() {
            err.value = "";
            saving.value = true;
            try {
                await api(`/api/forwards/nodes/${props.item.node_id}`, {
                    method: "PUT",
                    body: {
                        relay_host: form.relay_host,
                        relay_host_v6: form.relay_host_v6,
                        port_start: Number(form.port_start),
                        port_end: Number(form.port_end),
                        enabled: form.enabled,
                    },
                });
                toast("已保存", "success");
                emit("saved");
            } catch (e) {
                err.value = e.message;
            } finally {
                saving.value = false;
            }
        }
        return { form, saving, err, save };
    },
    template: `
        <Modal :title="'转发设置 — ' + item.node_name" @close="$emit('close')">
            <div v-if="err" class="notice error">{{ err }}</div>

            <fieldset>
                <legend>中继地址</legend>
                <div class="field">
                    <label>其他节点访问本节点用的地址</label>
                    <input type="text" v-model="form.relay_host" :placeholder="item.effective_relay_host || '留空则用节点的 IPv4'">
                    <div class="field-hint">
                        多跳时上一跳要往这个地址发包。留空会依次回落到节点的 IPv4、SSH 地址。
                        探测地址走内网、中转要走公网时，在这里单独填公网地址。
                    </div>
                </div>
                <div class="field">
                    <label>IPv6 中继地址</label>
                    <input type="text" v-model="form.relay_host_v6" placeholder="选填">
                </div>
            </fieldset>

            <fieldset>
                <legend>自动分配端口范围</legend>
                <div class="field-row">
                    <div class="field">
                        <label>起始端口</label>
                        <input type="number" min="1" max="65535" v-model="form.port_start">
                    </div>
                    <div class="field">
                        <label>结束端口</label>
                        <input type="number" min="1" max="65535" v-model="form.port_end">
                    </div>
                </div>
                <div class="field-hint">
                    多跳链路的中间端口从这个范围里挑。目前已占用 {{ item.used_ports }} 个。
                    范围要避开机器上其他服务在用的端口。
                </div>
                <label class="checkbox-label" style="margin-top:10px">
                    <input type="checkbox" v-model="form.enabled"> 允许这个节点参与端口转发
                </label>
            </fieldset>

            <div class="modal-actions">
                <button class="btn" @click="$emit('close')">取消</button>
                <button class="btn primary" :disabled="saving" @click="save">
                    <span v-if="saving" class="spinner"></span>保存
                </button>
            </div>
        </Modal>
    `,
};

// ForwardTestResult 展示一次链路测试的结果。
// 逐段列出来，是因为链路不通时「哪一段断了」比「通不通」有用得多。
const ForwardTestResult = {
    components: { Modal },
    props: { report: Object },
    emits: ["close"],
    template: `
      <Modal :title="'链路测试 — ' + report.rule_name" wide @close="$emit('close')">
        <div class="notice" :class="report.ok ? '' : 'error'" style="margin-bottom:16px">
            {{ report.summary }}
        </div>

        <div class="table-scroll">
            <table>
                <thead>
                    <tr>
                        <th style="width:150px">从哪里发起</th>
                        <th>拨向</th>
                        <th style="width:110px">结果</th>
                        <th style="width:90px">耗时</th>
                    </tr>
                </thead>
                <tbody>
                    <tr v-for="leg in report.legs" :key="leg.position">
                        <td>第 {{ leg.position + 1 }} 跳<div class="node-meta">{{ leg.from }}</div></td>
                        <td class="mono">{{ leg.target || '—' }}</td>
                        <td>
                            <span class="badge" :class="leg.ok ? 'good' : 'critical'">
                                <span class="dot"></span>{{ leg.ok ? '通' : '不通' }}
                            </span>
                        </td>
                        <td class="tabular">{{ leg.latency_ms }} ms</td>
                    </tr>
                    <tr v-if="report.entry">
                        <td>面板 → 入口<div class="node-meta">仅供参考</div></td>
                        <td class="mono">{{ report.entry.target }}</td>
                        <td>
                            <span class="badge" :class="report.entry.ok ? 'good' : 'warning'">
                                <span class="dot"></span>{{ report.entry.ok ? '通' : '不通' }}
                            </span>
                        </td>
                        <td class="tabular">{{ report.entry.latency_ms }} ms</td>
                    </tr>
                </tbody>
            </table>
        </div>

        <template v-for="leg in report.legs" :key="'d' + leg.position">
            <div v-if="leg.error || (leg.diagnosis && leg.diagnosis.problems && leg.diagnosis.problems.length)"
                 class="notice error" style="margin-top:14px">
                <b>第 {{ leg.position + 1 }} 跳（{{ leg.from }}）</b>
                <div v-if="leg.error" style="margin-top:6px">拨号失败：{{ leg.error }}</div>
                <div v-for="(pb, i) in (leg.diagnosis ? leg.diagnosis.problems : [])" :key="i" style="margin-top:6px">
                    {{ pb }}
                </div>
            </div>
        </template>

        <fieldset style="margin-top:16px">
            <legend>各跳自检</legend>
            <div class="table-scroll">
                <table>
                    <thead>
                        <tr>
                            <th>节点</th><th style="width:110px">nftables</th>
                            <th style="width:130px">转发开关</th><th style="width:110px">端口监听</th>
                            <th style="width:100px">已生效规则</th><th>本机防火墙</th>
                        </tr>
                    </thead>
                    <tbody>
                        <tr v-for="leg in report.legs.filter(l => l.diagnosis)" :key="'x' + leg.position">
                            <td>{{ leg.from }}</td>
                            <td>{{ leg.diagnosis.nft_available ? '可用' : '缺失' }}</td>
                            <td>{{ leg.diagnosis.ip_forward ? '已开启' : '未开启' }}</td>
                            <td>{{ leg.diagnosis.listening ? '有' : '无' }}</td>
                            <td class="tabular">{{ leg.diagnosis.rule_count }}</td>
                            <td>{{ (leg.diagnosis.firewalls || []).join('、') || '未检测到' }}</td>
                        </tr>
                    </tbody>
                </table>
            </div>
            <div class="field-hint" style="margin-top:8px">
                内核态转发不需要监听进程，所以「端口监听」为「无」是正常的；用户态转发则必须有。
            </div>
        </fieldset>

        <div class="modal-actions">
            <button class="btn primary" @click="$emit('close')">知道了</button>
        </div>
      </Modal>
    `,
};

export const ForwardView = {
    components: { ForwardEditor, ForwardNodeEditor, ForwardTestResult },
    setup() {
        const rules = ref([]);
        const nodes = ref([]);
        const fwdNodes = ref([]);
        const loading = ref(true);
        const syncing = ref(false);
        const editRule = ref(null);
        const editNode = ref(null);
        const busy = ref(0);
        const testing = ref(0);
        const testReport = ref(null);
        let timer = null;

        async function load(silent = false) {
            if (!silent) loading.value = true;
            try {
                const [r, n, fn] = await Promise.all([
                    api("/api/forwards"),
                    api("/api/nodes"),
                    api("/api/forwards/nodes"),
                ]);
                rules.value = r || [];
                nodes.value = n || [];
                fwdNodes.value = fn || [];
            } catch (e) {
                if (!silent) toast(e.message, "error");
            } finally {
                loading.value = false;
            }
        }

        onMounted(() => {
            load();
            // 下发是异步的，回执要几秒才到，轮询让用户能看到状态变化。
            timer = setInterval(() => load(true), 8000);
        });
        onBeforeUnmount(() => clearInterval(timer));

        async function toggle(rule) {
            busy.value = rule.id;
            try {
                await api(`/api/forwards/${rule.id}/toggle`, { method: "POST", body: { enabled: !rule.enabled } });
                toast(rule.enabled ? "已停用，正在撤回规则" : "已启用，正在下发", "success");
                await load(true);
            } catch (e) {
                toast(e.message, "error");
            } finally {
                busy.value = 0;
            }
        }

        async function remove(rule) {
            if (!window.confirm(`删除转发规则「${rule.name}」吗？\n节点上的监听会一并撤掉，已产生的流量统计也会清除。`)) return;
            try {
                await api(`/api/forwards/${rule.id}`, { method: "DELETE" });
                toast("已删除", "success");
                await load(true);
            } catch (e) {
                toast(e.message, "error");
            }
        }

        async function syncAll() {
            syncing.value = true;
            try {
                const res = await api("/api/forwards/sync", { method: "POST", body: {} });
                if (res.problems && res.problems.length) {
                    toast(`已下发，但有 ${res.problems.length} 条规则没生效`, "error");
                } else {
                    toast("已重新下发到所有节点", "success");
                }
                await load(true);
            } catch (e) {
                toast(e.message, "error");
            } finally {
                syncing.value = false;
            }
        }

        async function runTest(rule) {
            testing.value = rule.id;
            try {
                testReport.value = await api(`/api/forwards/${rule.id}/test`, { method: "POST", body: {} });
            } catch (e) {
                toast(e.message, "error");
            } finally {
                testing.value = 0;
            }
        }

        function copy(text) {
            if (!text) return;
            navigator.clipboard?.writeText(text).then(
                () => toast("已复制：" + text, "success"),
                () => toast("复制失败，请手动选中", "error"),
            );
        }

        // 转发流量在网卡上进出各走一遍，双向计费口径下要算两倍。
        // 这个提示直接放在页面上，省得用户自己去对账。
        const totalShare = computed(() =>
            rules.value.reduce(
                (sum, r) => sum + (r.hop_views || []).reduce((s, h) => s + (h.node_share || 0), 0),
                0,
            ),
        );

        return {
            rules, nodes, fwdNodes, loading, syncing, editRule, editNode, busy,
            testing, testReport, runTest,
            load, toggle, remove, syncAll, copy, fmtBytes, totalShare,
        };
    },
    template: `
        <div>
            <div class="page-head">
                <div>
                    <h1>转发规则</h1>
                    <p>把入口机器的端口转到目标，中间可以串多台机器。规则改动会自动下发到相关节点。</p>
                </div>
                <div class="btn-row">
                    <button class="btn" @click="load()">刷新</button>
                    <button class="btn" :disabled="syncing" @click="syncAll">
                        <span v-if="syncing" class="spinner"></span>重新下发
                    </button>
                    <button class="btn primary" @click="editRule = { rule: null }">添加规则</button>
                </div>
            </div>

            <div class="notice" style="margin-bottom:16px">
                转发流量在中转机的网卡上要<b>进出各走一遍</b>，所以节点账本里它是按两份算的
                （双向计费口径下）。下表的「占用配额」列已经做过换算，可以直接和节点的计费流量对上。
                当前所有规则合计占用 <b>{{ fmtBytes(totalShare) }}</b>。
            </div>

            <div class="card">
                <div class="card-title"><span>规则列表</span></div>

                <div v-if="loading" class="empty">加载中…</div>
                <div v-else-if="!rules.length" class="empty">
                    还没有转发规则。点右上角「添加规则」，选一台机器当入口，填上要转到哪里就行。
                </div>
                <div v-else class="table-scroll">
                    <table>
                        <thead>
                            <tr>
                                <th>规则</th>
                                <th>入口地址</th>
                                <th>链路</th>
                                <th style="width:150px">本周期流量</th>
                                <th style="width:130px">占用配额</th>
                                <th style="width:210px">操作</th>
                            </tr>
                        </thead>
                        <tbody>
                            <tr v-for="r in rules" :key="r.id">
                                <td>
                                    <div class="node-name">
                                        {{ r.name }}
                                        <span class="badge" :class="r.enabled ? 'good' : 'muted'">
                                            <span class="dot"></span>{{ r.enabled ? '已启用' : '已停用' }}
                                        </span>
                                    </div>
                                    <div class="node-meta">
                                        {{ r.proto_label }} → {{ r.dest_host }}:{{ r.dest_port }}
                                        <template v-if="r.remark"> · {{ r.remark }}</template>
                                    </div>
                                    <div v-if="r.problem" class="node-meta" style="color:var(--critical)">
                                        未生效：{{ r.problem }}
                                    </div>
                                </td>
                                <td>
                                    <span v-if="r.entry_address" class="mono">{{ r.entry_address }}</span>
                                    <span v-else class="muted">入口节点没有可用地址</span>
                                </td>
                                <td>
                                    <div v-for="(h, i) in r.hop_views" :key="h.id" class="node-meta">
                                        {{ i + 1 }}. {{ h.node_name }}:{{ h.listen_port }}
                                        <span class="badge" :class="h.node_online ? 'good' : 'critical'">
                                            <span class="dot"></span>{{ h.node_online ? '在线' : '离线' }}
                                        </span>
                                        <span class="muted"> {{ h.mode_label }}</span>
                                        <span v-if="h.bandwidth_mbps" class="muted"> · 限 {{ h.bandwidth_mbps }}Mbps</span>
                                        <span v-if="h.target" class="muted"> → {{ h.target }}</span>
                                    </div>
                                </td>
                                <td class="tabular">
                                    ↑ {{ fmtBytes(r.total_up) }}<br>
                                    ↓ {{ fmtBytes(r.total_down) }}
                                </td>
                                <td class="tabular">
                                    {{ fmtBytes(r.hop_views.reduce((s, h) => s + (h.node_share || 0), 0)) }}
                                </td>
                                <td>
                                    <div class="btn-row">
                                        <button class="btn small" :disabled="testing === r.id" @click="runTest(r)">
                                            <span v-if="testing === r.id" class="spinner"></span>测试
                                        </button>
                                        <button class="btn small" :disabled="!r.entry_address" @click="copy(r.entry_address)">复制入口</button>
                                        <button class="btn small" :disabled="busy === r.id" @click="toggle(r)">
                                            {{ r.enabled ? '停用' : '启用' }}
                                        </button>
                                        <button class="btn small" @click="editRule = { rule: r }">编辑</button>
                                        <button class="btn small danger" @click="remove(r)">删除</button>
                                    </div>
                                </td>
                            </tr>
                        </tbody>
                    </table>
                </div>
            </div>

            <div class="card">
                <div class="card-title"><span>节点转发设置</span></div>
                <div v-if="!fwdNodes.length" class="empty">还没有节点。先去「节点管理」添加机器。</div>
                <div v-else class="table-scroll">
                    <table>
                        <thead>
                            <tr>
                                <th>节点</th>
                                <th>中继地址</th>
                                <th style="width:170px">自动分配端口范围</th>
                                <th style="width:110px">已占用</th>
                                <th style="width:110px">状态</th>
                                <th style="width:100px">操作</th>
                            </tr>
                        </thead>
                        <tbody>
                            <tr v-for="fn in fwdNodes" :key="fn.node_id">
                                <td>{{ fn.node_name }}</td>
                                <td>
                                    <span v-if="fn.effective_relay_host" class="mono">{{ fn.effective_relay_host }}</span>
                                    <span v-else style="color:var(--critical)">没有可用地址，无法作为中转</span>
                                    <div v-if="!fn.relay_host && fn.effective_relay_host" class="node-meta">来自节点的 IP，未单独配置</div>
                                </td>
                                <td class="tabular">{{ fn.port_start }} – {{ fn.port_end }}</td>
                                <td class="tabular">{{ fn.used_ports }}</td>
                                <td>
                                    <span class="badge" :class="fn.enabled ? 'good' : 'muted'">
                                        <span class="dot"></span>{{ fn.enabled ? '允许转发' : '已关闭' }}
                                    </span>
                                </td>
                                <td>
                                    <button class="btn small" @click="editNode = fn">设置</button>
                                </td>
                            </tr>
                        </tbody>
                    </table>
                </div>
            </div>

            <ForwardEditor v-if="editRule" :rule="editRule.rule" :nodes="nodes"
                           @close="editRule = null" @saved="editRule = null; load()" />
            <ForwardNodeEditor v-if="editNode" :item="editNode"
                               @close="editNode = null" @saved="editNode = null; load()" />
            <ForwardTestResult v-if="testReport" :report="testReport" @close="testReport = null" />
        </div>
    `,
};
