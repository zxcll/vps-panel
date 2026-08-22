// 节点管理：列表 + 新建/编辑表单。

import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from "vue";
import {
    api, BILLING_MODES, COMMON_TIMEZONES, EXCEED_ACTIONS,
    fmtAgo, fmtBytes, fmtRate, fmtTime, parseBytes, toast,
} from "./api.js";
import { Modal, QuotaMeter, StatusBadge } from "./components.js";

// blankNode 是新建节点时的默认值。默认「不处理」是刻意的：
// 关机不可逆，必须由用户显式选择才生效。
function blankNode() {
    return {
        name: "",
        remark: "",
        ipv4: "",
        ipv6: "",
        quota_text: "1TB",
        billing_mode: "sum",
        traffic_ratio: 1,
        warn_percent: 90,
        reset_day: 1,
        reset_tz: "Asia/Shanghai",
        action_on_exceed: "none",
        exceed_command: "",
        auto_recover_on_reset: true,
        ssh_host: "",
        ssh_port: 22,
        ssh_user: "root",
        ssh_auth: "password",
        ssh_secret: "",
        ssh_key_pass: "",
        ssh_host_key: "",
        ssh_use_sudo: false,
        probe_port: 0,
        enabled: true,
    };
}

export const NodeEditor = {
    components: { Modal },
    props: { node: Object },
    emits: ["close", "saved"],
    setup(props, { emit }) {
        const isEdit = computed(() => !!props.node);
        const form = reactive(blankNode());
        const saving = ref(false);
        const err = ref("");
        // secretTouched 区分"没改密码"和"把密码清空了"——
        // 编辑时不回显已存的凭据，用户不动就保持原样
        const secretTouched = ref(false);

        if (props.node) {
            const n = props.node;
            Object.assign(form, {
                name: n.name, remark: n.remark, ipv4: n.ipv4, ipv6: n.ipv6,
                quota_text: n.quota_bytes > 0 ? fmtBytes(n.quota_bytes) : "0",
                billing_mode: n.billing_mode,
                traffic_ratio: n.traffic_ratio,
                warn_percent: n.warn_percent,
                reset_day: n.reset_day,
                reset_tz: n.reset_tz,
                action_on_exceed: n.action_on_exceed,
                exceed_command: n.exceed_command,
                auto_recover_on_reset: n.auto_recover_on_reset,
                ssh_host: n.ssh_host, ssh_port: n.ssh_port, ssh_user: n.ssh_user,
                ssh_auth: n.ssh_auth, ssh_secret: "", ssh_key_pass: "",
                ssh_host_key: n.ssh_host_key,
                ssh_use_sudo: n.ssh_use_sudo,
                probe_port: n.probe_port,
                enabled: n.enabled,
            });
        }

        const quotaBytes = computed(() => parseBytes(form.quota_text));
        const quotaValid = computed(() => quotaBytes.value !== null);

        const modeHint = computed(
            () => BILLING_MODES.find((m) => m.value === form.billing_mode)?.hint || ""
        );
        const actionHint = computed(
            () => EXCEED_ACTIONS.find((a) => a.value === form.action_on_exceed)?.hint || ""
        );

        const needsSSH = computed(() => form.action_on_exceed === "shutdown_ssh");

        async function save() {
            err.value = "";
            if (!quotaValid.value) {
                err.value = "流量配额格式不对。可以写 500GB、1.5TB、0（表示不限量）";
                return;
            }

            const body = {
                name: form.name,
                remark: form.remark,
                ipv4: form.ipv4,
                ipv6: form.ipv6,
                quota_bytes: quotaBytes.value,
                billing_mode: form.billing_mode,
                traffic_ratio: Number(form.traffic_ratio) || 1,
                warn_percent: Number(form.warn_percent) || 0,
                reset_day: Number(form.reset_day) || 1,
                reset_tz: form.reset_tz,
                action_on_exceed: form.action_on_exceed,
                exceed_command: form.exceed_command,
                auto_recover_on_reset: form.auto_recover_on_reset,
                ssh_host: form.ssh_host,
                ssh_port: Number(form.ssh_port) || 22,
                ssh_user: form.ssh_user,
                ssh_auth: form.ssh_auth,
                ssh_host_key: form.ssh_host_key,
                ssh_use_sudo: form.ssh_use_sudo,
                probe_port: Number(form.probe_port) || 0,
                enabled: form.enabled,
                ssh_secret: null,
                ssh_key_pass: null,
            };

            // 新建时总是提交凭据；编辑时只有用户动过才提交
            if (!isEdit.value || secretTouched.value) {
                body.ssh_secret = form.ssh_secret;
                body.ssh_key_pass = form.ssh_key_pass;
            }

            saving.value = true;
            try {
                if (isEdit.value) {
                    await api(`/api/nodes/${props.node.id}`, { method: "PUT", body });
                    toast("节点已保存", "success");
                } else {
                    await api("/api/nodes", { method: "POST", body });
                    toast("节点已创建，接下来把探针装到机器上", "success");
                }
                emit("saved");
            } catch (e) {
                err.value = e.message;
            } finally {
                saving.value = false;
            }
        }

        return {
            form, isEdit, saving, err, save, quotaBytes, quotaValid,
            modeHint, actionHint, needsSSH, secretTouched,
            BILLING_MODES, EXCEED_ACTIONS, COMMON_TIMEZONES, fmtBytes,
        };
    },
    template: `
        <Modal :title="isEdit ? '编辑节点' : '新建节点'" wide @close="$emit('close')">
            <div v-if="err" class="notice error">{{ err }}</div>

            <fieldset>
                <legend>基本信息</legend>
                <div class="field-row">
                    <div class="field">
                        <label>节点名称 *</label>
                        <input type="text" v-model="form.name" placeholder="如：香港 CN2">
                    </div>
                    <div class="field">
                        <label>备注</label>
                        <input type="text" v-model="form.remark" placeholder="商家、到期时间等">
                    </div>
                </div>
                <div class="field-row">
                    <div class="field">
                        <label>IPv4 地址</label>
                        <input type="text" v-model="form.ipv4" placeholder="1.2.3.4">
                        <div class="field-hint">A 记录切换时会写这个地址</div>
                    </div>
                    <div class="field">
                        <label>IPv6 地址</label>
                        <input type="text" v-model="form.ipv6" placeholder="2001:db8::1">
                        <div class="field-hint">AAAA 记录切换时会写这个地址</div>
                    </div>
                </div>
                <label class="checkbox-label">
                    <input type="checkbox" v-model="form.enabled"> 启用该节点
                </label>
            </fieldset>

            <fieldset>
                <legend>流量配额</legend>
                <div class="field-row">
                    <div class="field">
                        <label>每周期配额</label>
                        <input type="text" v-model="form.quota_text" placeholder="1TB">
                        <div class="field-hint">
                            支持 500GB / 1.5TB 这类写法，填 0 表示不限量。
                            <template v-if="quotaValid && quotaBytes > 0">
                                实际为 {{ quotaBytes.toLocaleString() }} 字节
                            </template>
                            <span v-else-if="!quotaValid" style="color:var(--critical)">格式无法识别</span>
                        </div>
                    </div>
                    <div class="field">
                        <label>计费口径</label>
                        <select v-model="form.billing_mode">
                            <option v-for="m in BILLING_MODES" :key="m.value" :value="m.value">{{ m.label }}</option>
                        </select>
                        <div class="field-hint">{{ modeHint }}</div>
                    </div>
                </div>
                <div class="field-row">
                    <div class="field">
                        <label>计费倍率</label>
                        <input type="number" step="0.05" min="0.1" max="10" v-model="form.traffic_ratio">
                        <div class="field-hint">
                            用来校准口径差异。网卡统计和商家账单通常差几个百分点，
                            商家算得比你多就调大一点。默认 1.0
                        </div>
                    </div>
                    <div class="field">
                        <label>预警百分比</label>
                        <input type="number" min="0" max="100" v-model="form.warn_percent">
                        <div class="field-hint">用量达到该比例时告警，填 0 关闭预警</div>
                    </div>
                </div>
            </fieldset>

            <fieldset>
                <legend>清零周期</legend>
                <div class="field-row">
                    <div class="field">
                        <label>每月清零日</label>
                        <input type="number" min="1" max="31" v-model="form.reset_day">
                        <div class="field-hint">填 31 时，遇到小月会自动落到当月最后一天</div>
                    </div>
                    <div class="field">
                        <label>时区</label>
                        <select v-model="form.reset_tz">
                            <option v-for="tz in COMMON_TIMEZONES" :key="tz" :value="tz">{{ tz }}</option>
                        </select>
                        <div class="field-hint">按商家账单所在时区的当天 00:00 清零</div>
                    </div>
                </div>
                <label class="checkbox-label">
                    <input type="checkbox" v-model="form.auto_recover_on_reset">
                    新周期开始时自动解除超额限制
                </label>
            </fieldset>

            <fieldset>
                <legend>流量耗尽后</legend>
                <div class="field">
                    <label>执行动作</label>
                    <select v-model="form.action_on_exceed">
                        <option v-for="a in EXCEED_ACTIONS" :key="a.value" :value="a.value">{{ a.label }}</option>
                    </select>
                    <div class="field-hint">{{ actionHint }}</div>
                </div>
                <div v-if="form.action_on_exceed === 'command'" class="field">
                    <label>自定义命令</label>
                    <textarea v-model="form.exceed_command"
                              placeholder="systemctl stop xray"></textarea>
                    <div class="field-hint">优先通过探针执行；探针离线时自动改走 SSH</div>
                </div>
            </fieldset>

            <fieldset>
                <legend>SSH 连接{{ needsSSH ? '（必填）' : '（可选）' }}</legend>
                <p class="field-hint" style="margin:-4px 0 12px">
                    配好 SSH 后，即使探针进程已经挂掉，面板仍然能远程把机器关掉。
                    凭据在数据库里以 AES-256-GCM 加密存储。
                </p>
                <div class="field-row">
                    <div class="field">
                        <label>主机地址</label>
                        <input type="text" v-model="form.ssh_host" placeholder="留空则使用 IPv4">
                    </div>
                    <div class="field">
                        <label>端口</label>
                        <input type="number" min="1" max="65535" v-model="form.ssh_port">
                    </div>
                    <div class="field">
                        <label>用户名</label>
                        <input type="text" v-model="form.ssh_user">
                    </div>
                </div>
                <div class="field-row">
                    <div class="field">
                        <label>认证方式</label>
                        <select v-model="form.ssh_auth">
                            <option value="password">密码</option>
                            <option value="key">私钥</option>
                        </select>
                    </div>
                    <div class="field">
                        <label>{{ form.ssh_auth === 'key' ? '私钥内容' : '密码' }}</label>
                        <textarea v-if="form.ssh_auth === 'key'" v-model="form.ssh_secret"
                                  @input="secretTouched = true"
                                  :placeholder="isEdit ? '留空表示不修改已保存的私钥' : '粘贴 -----BEGIN OPENSSH PRIVATE KEY----- 开头的完整内容'"></textarea>
                        <input v-else type="password" v-model="form.ssh_secret"
                               @input="secretTouched = true"
                               :placeholder="isEdit ? '留空表示不修改已保存的密码' : ''">
                    </div>
                </div>
                <div v-if="form.ssh_auth === 'key'" class="field">
                    <label>私钥口令</label>
                    <input type="password" v-model="form.ssh_key_pass" @input="secretTouched = true"
                           placeholder="私钥没有口令则留空">
                </div>
                <div class="field-row">
                    <div class="field">
                        <label>已记录的主机密钥</label>
                        <input type="text" v-model="form.ssh_host_key" placeholder="首次连接时自动记录">
                        <div class="field-hint">
                            机器重装系统后主机密钥会变，面板会拒绝连接。
                            确认是自己重装的，就把这一栏清空后重新测试连接。
                        </div>
                    </div>
                    <div class="field">
                        <label>健康探测端口</label>
                        <input type="number" min="0" max="65535" v-model="form.probe_port">
                        <div class="field-hint">
                            探针失联时面板会拨这个端口确认机器是否还活着，
                            避免"探针挂了但机器没事"就把域名切走。填 0 则用 SSH 端口
                        </div>
                    </div>
                </div>
                <label class="checkbox-label">
                    <input type="checkbox" v-model="form.ssh_use_sudo"> 命令前加 sudo（非 root 用户登录时勾选）
                </label>
            </fieldset>

            <div class="modal-actions">
                <button class="btn" @click="$emit('close')">取消</button>
                <button class="btn primary" :disabled="saving || !form.name" @click="save">
                    <span v-if="saving" class="spinner"></span>{{ saving ? '保存中' : '保存' }}
                </button>
            </div>
        </Modal>
    `,
};

export const NodesView = {
    components: { NodeEditor, StatusBadge, QuotaMeter, Modal },
    emits: ["open-node"],
    setup() {
        const nodes = ref([]);
        const loading = ref(true);
        const editing = ref(null); // null=不显示, {}=新建, node=编辑
        const install = ref(null);
        // 探针版本信息：面板认为的最新版 + 每台机器当前跑的版本。
        const versions = ref({ latest_version: "", outdated_count: 0, nodes: [], missing_binaries: [] });
        const upgrading = ref(0);       // 正在升级的节点 id，0 表示没有
        const upgradingAll = ref(false);
        const upgradeReport = ref(null);
        let timer = null;

        async function load(silent = false) {
            if (!silent) loading.value = true;
            try {
                const [n, v] = await Promise.all([
                    api("/api/nodes"),
                    api("/api/nodes/versions").catch(() => null),
                ]);
                nodes.value = n || [];
                if (v) versions.value = v;
            } catch (e) {
                toast(e.message, "error");
            } finally {
                loading.value = false;
            }
        }

        // 按节点 id 索引版本信息，模板里直接查。
        const versionOf = computed(() => {
            const m = {};
            for (const row of versions.value.nodes || []) m[row.node_id] = row;
            return m;
        });

        // 面板上缺哪个架构的二进制，升级到那台机器一定会失败，得先说清楚。
        const missingBinaries = computed(() => versions.value.missing_binaries || []);

        async function upgrade(n) {
            const row = versionOf.value[n.id];
            const cur = row?.version || "未知";
            if (!window.confirm(
                `把节点「${n.name}」的探针从 ${cur} 升到 ${versions.value.latest_version} 吗？\n\n` +
                `探针会从面板下载新版二进制、校验能正常执行之后再替换自己，然后重启。\n` +
                `校验不过就不会动旧版本。升级期间这台机器的流量统计会短暂中断几秒。`)) return;

            upgrading.value = n.id;
            try {
                const res = await api(`/api/nodes/${n.id}/upgrade`, { method: "POST", body: { force: false } });
                toast(res.message || "已升级", res.ok ? "success" : "info", 9000);
            } catch (e) {
                toast(e.message, "error", 12000);
            } finally {
                upgrading.value = 0;
                // 探针重启要几秒，等一下再刷新才看得到新版本号。
                setTimeout(() => load(true), 6000);
            }
        }

        async function upgradeAll() {
            if (!window.confirm(
                `把所有在线节点的探针都升到 ${versions.value.latest_version} 吗？\n\n` +
                `已经是最新版的会自动跳过，不在线的也会跳过。\n` +
                `升级是一台一台做的，节点多的话要等一会儿。`)) return;

            upgradingAll.value = true;
            try {
                const res = await api("/api/nodes/upgrade", { method: "POST", body: { force: false } });
                upgradeReport.value = res;
            } catch (e) {
                toast(e.message, "error", 12000);
            } finally {
                upgradingAll.value = false;
                setTimeout(() => load(true), 6000);
            }
        }

        onMounted(() => {
            load();
            timer = setInterval(() => load(true), 8000);
        });
        onBeforeUnmount(() => clearInterval(timer));

        function create() {
            editing.value = { node: null };
        }

        function edit(n) {
            editing.value = { node: n };
        }

        async function onSaved() {
            editing.value = null;
            await load();
        }

        async function remove(n) {
            if (!window.confirm(`确定删除节点「${n.name}」吗？\n它的流量历史、事件记录会一并删除，且无法恢复。`)) return;
            try {
                await api(`/api/nodes/${n.id}`, { method: "DELETE" });
                toast("节点已删除", "success");
                await load();
            } catch (e) {
                toast(e.message, "error");
            }
        }

        async function showInstall(n) {
            try {
                install.value = { node: n, ...(await api(`/api/nodes/${n.id}/install`)) };
            } catch (e) {
                toast(e.message, "error");
            }
        }

        async function rotate(n) {
            if (!window.confirm(`重置「${n.name}」的密钥后，这台机器上的旧探针会立刻失效，需要重装。继续？`)) return;
            try {
                const res = await api(`/api/nodes/${n.id}/rotate-secret`, { method: "POST", body: {} });
                toast(res.message, "success", 7000);
                await load();
                await showInstall(n);
            } catch (e) {
                toast(e.message, "error");
            }
        }

        return {
            nodes, loading, editing, install, create, edit, onSaved, remove,
            showInstall, rotate, load, fmtBytes, fmtRate, fmtAgo, fmtTime,
            versions, versionOf, missingBinaries, upgrading, upgradingAll,
            upgradeReport, upgrade, upgradeAll,
        };
    },
    template: `
        <div>
            <div class="page-head">
                <div>
                    <h1>节点管理</h1>
                    <p>添加要监控的机器，设置流量配额、每月清零日，以及流量跑满后怎么处理</p>
                </div>
                <div class="btn-row">
                    <button class="btn" :disabled="upgradingAll || !versions.outdated_count"
                            @click="upgradeAll">
                        <span v-if="upgradingAll" class="spinner"></span>
                        升级探针<template v-if="versions.outdated_count">（{{ versions.outdated_count }} 台可升）</template>
                    </button>
                    <button class="btn" @click="load()">刷新</button>
                    <button class="btn primary" @click="create">新建节点</button>
                </div>
            </div>

            <div v-if="missingBinaries.length" class="notice error" style="margin-bottom:14px">
                面板上缺少这些架构的探针二进制：<b>{{ missingBinaries.join('、') }}</b>。
                对应架构的机器无法一键安装，也无法远程升级。
                在面板机器上执行 <code>sudo bash install.sh panel agents</code> 补上。
            </div>

            <div v-if="loading && !nodes.length" class="empty"><span class="spinner"></span> 加载中…</div>
            <div v-else-if="!nodes.length" class="card">
                <div class="empty">
                    还没有节点。点右上角「新建节点」，填好配额和清零日，<br>
                    保存后会给出一条安装命令，在 VPS 上执行就能把探针装好。
                </div>
            </div>

            <div v-else class="card">
                <div class="table-scroll">
                    <table>
                        <thead>
                            <tr>
                                <th>节点</th><th>状态</th><th>探针版本</th><th>本周期用量</th>
                                <th>计费口径</th><th>清零</th><th>超额动作</th><th>操作</th>
                            </tr>
                        </thead>
                        <tbody>
                            <tr v-for="n in nodes" :key="n.id">
                                <td>
                                    <div class="node-name" @click="$emit('open-node', n.id)">{{ n.name }}</div>
                                    <div class="node-meta">{{ n.ipv4 || n.ipv6 || '未填 IP' }}</div>
                                </td>
                                <td>
                                    <StatusBadge :status="n.status" />
                                    <div class="node-meta">{{ fmtAgo(n.last_seen) }}</div>
                                </td>
                                <td>
                                    <template v-if="versionOf[n.id] && versionOf[n.id].version">
                                        <span class="tabular">{{ versionOf[n.id].version }}</span>
                                        <div v-if="versionOf[n.id].outdated" class="node-meta">
                                            <span class="badge warning">
                                                <span class="dot"></span>可升到 {{ versions.latest_version }}
                                            </span>
                                        </div>
                                    </template>
                                    <span v-else class="muted">—</span>
                                </td>
                                <td style="min-width:200px">
                                    <QuotaMeter :used="n.quota_status.billed_bytes" :quota="n.quota_bytes"
                                                :warn-percent="n.warn_percent" label="已用" />
                                </td>
                                <td>{{ n.billing_mode_label }}</td>
                                <td class="tabular">
                                    每月 {{ n.reset_day }} 日
                                    <div class="node-meta">{{ n.reset_tz }}</div>
                                </td>
                                <td>{{ n.action_label }}</td>
                                <td>
                                    <div class="btn-row">
                                        <button class="btn small" @click="edit(n)">编辑</button>
                                        <button v-if="versionOf[n.id] && versionOf[n.id].online"
                                                class="btn small" :disabled="upgrading === n.id"
                                                @click="upgrade(n)">
                                            <span v-if="upgrading === n.id" class="spinner"></span>升级
                                        </button>
                                        <button class="btn small" @click="showInstall(n)">安装</button>
                                        <button class="btn small" @click="rotate(n)">换密钥</button>
                                        <button class="btn small danger" @click="remove(n)">删除</button>
                                    </div>
                                </td>
                            </tr>
                        </tbody>
                    </table>
                </div>
            </div>

            <NodeEditor v-if="editing" :node="editing.node"
                        @close="editing = null" @saved="onSaved" />

            <Modal v-if="upgradeReport" title="批量升级结果" wide @close="upgradeReport = null">
                <p class="field-hint" style="margin-bottom:12px">
                    目标版本 <b>{{ upgradeReport.latest_version }}</b>：
                    已升级 {{ upgradeReport.upgraded }} 台，
                    跳过 {{ upgradeReport.skipped }} 台，
                    失败 {{ upgradeReport.failed }} 台。
                </p>
                <div class="table-scroll">
                    <table>
                        <thead>
                            <tr>
                                <th>节点</th>
                                <th style="width:120px">版本变化</th>
                                <th style="width:90px">结果</th>
                                <th>说明</th>
                            </tr>
                        </thead>
                        <tbody>
                            <tr v-for="r in upgradeReport.results" :key="r.node_id">
                                <td>{{ r.node_name }}</td>
                                <td class="tabular">
                                    {{ r.from_version || '—' }} → {{ r.to_version }}
                                </td>
                                <td>
                                    <span class="badge" :class="r.skipped ? 'muted' : (r.ok ? 'good' : 'critical')">
                                        <span class="dot"></span>{{ r.skipped ? '跳过' : (r.ok ? '成功' : '失败') }}
                                    </span>
                                </td>
                                <td class="node-meta">{{ r.message || r.error }}</td>
                            </tr>
                        </tbody>
                    </table>
                </div>
                <p class="field-hint" style="margin-top:12px">
                    探针替换完二进制会立刻重启，所以「连接断了」不一定是失败 ——
                    过半分钟回来看这台机器的版本号有没有变上去。
                </p>
                <div class="modal-actions">
                    <button class="btn" @click="upgradeReport = null">关闭</button>
                </div>
            </Modal>

            <Modal v-if="install" :title="'在「' + install.node.name + '」上安装探针'" @close="install = null">
                <p class="field-hint" style="margin-bottom:10px">
                    SSH 登录到这台 VPS，用 root 执行：
                </p>
                <div class="code-block">{{ install.install_cmd }}</div>
                <p class="field-hint" style="margin:14px 0 6px">
                    脚本会把探针装到 /usr/local/bin/vps-agent，注册成 systemd 服务并设为开机自启。
                    装好后大约 30 秒内节点就会变成在线。
                </p>
                <p class="field-hint" style="margin:14px 0 6px">卸载：</p>
                <div class="code-block">{{ install.uninstall }}</div>
                <div class="modal-actions">
                    <button class="btn" @click="install = null">关闭</button>
                </div>
            </Modal>
        </div>
    `,
};
