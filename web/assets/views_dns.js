// DNS 管理：服务商凭据、记录组（域名 → 候选节点优先级）、手动切换、切换演练。

import { computed, onMounted, reactive, ref } from "vue";
import { api, fmtTime, PROVIDER_TYPES, toast } from "./api.js";
import { Modal } from "./components.js";

// 各家凭据的字段不一样，这里用一张表描述，表单按类型渲染。
const CRED_FIELDS = {
    cloudflare: [
        { key: "api_token", label: "API Token", type: "password", required: true,
          hint: "在 Cloudflare「我的个人资料 → API 令牌」创建，权限需要包含「区域 → DNS → 编辑」" },
        { key: "zone_id", label: "Zone ID（可选）", type: "text",
          hint: "填了就跳过按域名查 Zone 的那次请求。Token 只授权了单个域名时建议填" },
    ],
    dnspod: [
        { key: "secret_id", label: "SecretId", type: "text", required: true,
          hint: "腾讯云访问管理 → API 密钥管理" },
        { key: "secret_key", label: "SecretKey", type: "password", required: true },
    ],
    alidns: [
        { key: "access_key_id", label: "AccessKeyId", type: "text", required: true,
          hint: "阿里云 RAM 访问控制 → 用户 → AccessKey" },
        { key: "access_key_secret", label: "AccessKeySecret", type: "password", required: true },
    ],
};

const ProviderEditor = {
    components: { Modal },
    props: { provider: Object },
    emits: ["close", "saved"],
    setup(props, { emit }) {
        const isEdit = computed(() => !!props.provider);
        const form = reactive({
            name: props.provider?.name || "",
            type: props.provider?.type || "cloudflare",
        });
        const cred = reactive({});
        const credTouched = ref(false);
        const saving = ref(false);
        const err = ref("");

        const fields = computed(() => CRED_FIELDS[form.type] || []);

        async function save() {
            err.value = "";
            const body = { name: form.name, type: form.type, credentials: null };

            // 编辑时没动凭据就不提交，后端会保留原值
            if (!isEdit.value || credTouched.value) {
                const c = {};
                for (const f of fields.value) {
                    if (cred[f.key]) c[f.key] = cred[f.key];
                }
                body.credentials = c;
            }

            saving.value = true;
            try {
                if (isEdit.value) {
                    await api(`/api/dns/providers/${props.provider.id}`, { method: "PUT", body });
                } else {
                    await api("/api/dns/providers", { method: "POST", body });
                }
                toast("已保存", "success");
                emit("saved");
            } catch (e) {
                err.value = e.message;
            } finally {
                saving.value = false;
            }
        }

        return { form, cred, credTouched, fields, saving, err, save, isEdit, PROVIDER_TYPES };
    },
    template: `
        <Modal :title="isEdit ? '编辑 DNS 服务商' : '添加 DNS 服务商'" @close="$emit('close')">
            <div v-if="err" class="notice error">{{ err }}</div>

            <div class="field">
                <label>名称 *</label>
                <input type="text" v-model="form.name" placeholder="如：主 Cloudflare 账号">
            </div>
            <div class="field">
                <label>服务商 *</label>
                <select v-model="form.type">
                    <option v-for="t in PROVIDER_TYPES" :key="t.value" :value="t.value">{{ t.label }}</option>
                </select>
            </div>

            <div v-for="f in fields" :key="f.key" class="field">
                <label>{{ f.label }}{{ f.required ? ' *' : '' }}</label>
                <input :type="f.type" v-model="cred[f.key]" @input="credTouched = true"
                       :placeholder="isEdit ? '留空表示不修改已保存的凭据' : ''">
                <div v-if="f.hint" class="field-hint">{{ f.hint }}</div>
            </div>

            <p class="field-hint">凭据保存时会用 AES-256-GCM 加密，保存前面板会先用它调一次接口验证可用性。</p>

            <div class="modal-actions">
                <button class="btn" @click="$emit('close')">取消</button>
                <button class="btn primary" :disabled="saving || !form.name" @click="save">
                    <span v-if="saving" class="spinner"></span>保存
                </button>
            </div>
        </Modal>
    `,
};

const RecordEditor = {
    components: { Modal },
    props: { record: Object, providers: Array, nodes: Array },
    emits: ["close", "saved"],
    setup(props, { emit }) {
        const isEdit = computed(() => !!props.record);
        const r = props.record;
        const form = reactive({
            provider_id: r?.provider_id || props.providers[0]?.id || 0,
            zone: r?.zone || "",
            name: r?.name || "",
            record_type: r?.record_type || "A",
            ttl: r?.ttl || 60,
            proxied: r?.proxied || false,
            strategy: r?.strategy || "failover",
            switch_on_exceed: r ? r.switch_on_exceed : true,
            switch_on_offline: r ? r.switch_on_offline : true,
            switch_on_warn: r ? r.switch_on_warn : false,
            enabled: r ? r.enabled : true,
        });

        // members 是候选节点列表，顺序即优先级
        const members = ref(
            (r?.members || []).map((m) => ({ node_id: m.node_id, priority: m.priority }))
        );
        const saving = ref(false);
        const err = ref("");

        const provider = computed(() => props.providers.find((p) => p.id === Number(form.provider_id)));
        const isCloudflare = computed(() => provider.value?.type === "cloudflare");

        const available = computed(() =>
            props.nodes.filter((n) => !members.value.some((m) => m.node_id === n.id))
        );

        function nodeName(id) {
            return props.nodes.find((n) => n.id === id)?.name || `#${id}`;
        }

        function nodeIP(id) {
            const n = props.nodes.find((x) => x.id === id);
            if (!n) return "";
            return form.record_type === "AAAA" ? n.ipv6 : n.ipv4;
        }

        function addMember(id) {
            if (!id) return;
            members.value.push({ node_id: Number(id), priority: (members.value.length + 1) * 10 });
        }

        function removeMember(i) {
            members.value.splice(i, 1);
            renumber();
        }

        function move(i, delta) {
            const j = i + delta;
            if (j < 0 || j >= members.value.length) return;
            const arr = members.value;
            [arr[i], arr[j]] = [arr[j], arr[i]];
            renumber();
        }

        // 优先级按当前顺序重排成 10、20、30…，用户不用手填数字
        function renumber() {
            members.value.forEach((m, i) => (m.priority = (i + 1) * 10));
        }

        async function save() {
            err.value = "";
            const body = {
                provider_id: Number(form.provider_id),
                zone: form.zone,
                name: form.name || form.zone,
                record_type: form.record_type,
                ttl: Number(form.ttl) || 60,
                proxied: form.proxied,
                strategy: form.strategy,
                switch_on_exceed: form.switch_on_exceed,
                switch_on_offline: form.switch_on_offline,
                switch_on_warn: form.switch_on_warn,
                enabled: form.enabled,
                members: members.value,
            };

            saving.value = true;
            try {
                if (isEdit.value) {
                    await api(`/api/dns/records/${props.record.id}`, { method: "PUT", body });
                } else {
                    await api("/api/dns/records", { method: "POST", body });
                }
                toast("已保存", "success");
                emit("saved");
            } catch (e) {
                err.value = e.message;
            } finally {
                saving.value = false;
            }
        }

        const addSelect = ref("");

        return {
            form, members, saving, err, save, isEdit, available, addSelect,
            addMember, removeMember, move, nodeName, nodeIP, isCloudflare,
        };
    },
    template: `
        <Modal :title="isEdit ? '编辑域名记录' : '添加域名记录'" wide @close="$emit('close')">
            <div v-if="err" class="notice error">{{ err }}</div>

            <fieldset>
                <legend>解析目标</legend>
                <div class="field-row">
                    <div class="field">
                        <label>DNS 服务商 *</label>
                        <select v-model="form.provider_id">
                            <option v-for="p in providers" :key="p.id" :value="p.id">
                                {{ p.name }}（{{ p.type_label }}）
                            </option>
                        </select>
                    </div>
                    <div class="field">
                        <label>记录类型</label>
                        <select v-model="form.record_type">
                            <option value="A">A（IPv4）</option>
                            <option value="AAAA">AAAA（IPv6）</option>
                        </select>
                    </div>
                </div>
                <div class="field-row">
                    <div class="field">
                        <label>主域名 *</label>
                        <input type="text" v-model="form.zone" placeholder="example.com">
                    </div>
                    <div class="field">
                        <label>完整记录名 *</label>
                        <input type="text" v-model="form.name" placeholder="us.example.com">
                        <div class="field-hint">留空则操作主域名本身（根记录）</div>
                    </div>
                    <div class="field">
                        <label>TTL（秒）</label>
                        <input type="number" min="1" v-model="form.ttl">
                        <div class="field-hint">故障切换场景建议 60 或更低</div>
                    </div>
                </div>
                <label v-if="isCloudflare" class="checkbox-label">
                    <input type="checkbox" v-model="form.proxied">
                    开启 Cloudflare 代理（小黄云）。开启后 TTL 强制为自动
                </label>
            </fieldset>

            <fieldset>
                <legend>候选节点（顺序即优先级，靠前的优先）</legend>
                <div v-if="!members.length" class="field-hint" style="margin-bottom:10px">
                    还没有候选节点。至少加一个，自动故障切换才有意义。
                </div>
                <div class="table-scroll" v-else>
                    <table>
                        <thead>
                            <tr><th style="width:60px">优先级</th><th>节点</th><th>解析值</th><th style="width:170px">调整</th></tr>
                        </thead>
                        <tbody>
                            <tr v-for="(m, i) in members" :key="m.node_id">
                                <td class="tabular">{{ i + 1 }}</td>
                                <td>{{ nodeName(m.node_id) }}</td>
                                <td class="mono">
                                    <span v-if="nodeIP(m.node_id)">{{ nodeIP(m.node_id) }}</span>
                                    <span v-else style="color:var(--critical)">该节点没填 {{ form.record_type }} 所需的 IP</span>
                                </td>
                                <td>
                                    <div class="btn-row">
                                        <button class="btn small" :disabled="i === 0" @click="move(i, -1)">上移</button>
                                        <button class="btn small" :disabled="i === members.length - 1" @click="move(i, 1)">下移</button>
                                        <button class="btn small danger" @click="removeMember(i)">移除</button>
                                    </div>
                                </td>
                            </tr>
                        </tbody>
                    </table>
                </div>
                <div class="field-row" style="margin-top:12px">
                    <div class="field" style="margin-bottom:0">
                        <select v-model="addSelect" @change="addMember(addSelect); addSelect = ''">
                            <option value="">＋ 添加候选节点…</option>
                            <option v-for="n in available" :key="n.id" :value="n.id">
                                {{ n.name }}（{{ form.record_type === 'AAAA' ? (n.ipv6 || '无 IPv6') : (n.ipv4 || '无 IPv4') }}）
                            </option>
                        </select>
                    </div>
                </div>
            </fieldset>

            <fieldset>
                <legend>切换策略</legend>
                <div class="field">
                    <label>模式</label>
                    <select v-model="form.strategy">
                        <option value="failover">自动故障切换（按优先级选主）</option>
                        <option value="manual">仅手动切换</option>
                    </select>
                </div>
                <template v-if="form.strategy === 'failover'">
                    <p class="field-hint" style="margin-bottom:8px">什么情况下自动切走当前节点：</p>
                    <label class="checkbox-label" style="margin-bottom:6px">
                        <input type="checkbox" v-model="form.switch_on_exceed"> 流量耗尽时
                    </label>
                    <label class="checkbox-label" style="margin-bottom:6px">
                        <input type="checkbox" v-model="form.switch_on_offline"> 节点离线时
                    </label>
                    <label class="checkbox-label">
                        <input type="checkbox" v-model="form.switch_on_warn">
                        流量达到预警线时就提前切走（给自己留余量）
                    </label>
                </template>
                <label class="checkbox-label" style="margin-top:10px">
                    <input type="checkbox" v-model="form.enabled"> 启用这条记录
                </label>
            </fieldset>

            <div class="modal-actions">
                <button class="btn" @click="$emit('close')">取消</button>
                <button class="btn primary" :disabled="saving || !form.zone" @click="save">
                    <span v-if="saving" class="spinner"></span>保存
                </button>
            </div>
        </Modal>
    `,
};

export const DNSView = {
    components: { ProviderEditor, RecordEditor, Modal },
    setup() {
        const providers = ref([]);
        const records = ref([]);
        const nodes = ref([]);
        const loading = ref(true);
        const editProvider = ref(null);
        const editRecord = ref(null);
        const plan = ref(null);
        const switching = ref(null);
        const busy = ref(0);

        async function load() {
            loading.value = true;
            try {
                const [p, r, n] = await Promise.all([
                    api("/api/dns/providers"),
                    api("/api/dns/records"),
                    api("/api/nodes"),
                ]);
                providers.value = p || [];
                records.value = r || [];
                nodes.value = n || [];
            } catch (e) {
                toast(e.message, "error");
            } finally {
                loading.value = false;
            }
        }

        onMounted(load);

        function nodeName(id) {
            if (!id) return "—";
            return nodes.value.find((n) => n.id === id)?.name || `#${id}`;
        }

        async function removeProvider(p) {
            if (!window.confirm(`删除服务商「${p.name}」会连带删除它下面的所有域名记录配置。继续？`)) return;
            try {
                await api(`/api/dns/providers/${p.id}`, { method: "DELETE" });
                toast("已删除", "success");
                await load();
            } catch (e) {
                toast(e.message, "error");
            }
        }

        async function removeRecord(r) {
            if (!window.confirm(`删除记录「${r.name}」的切换配置吗？\n注意：只删面板里的配置，不会动服务商上已有的解析记录。`)) return;
            try {
                await api(`/api/dns/records/${r.id}`, { method: "DELETE" });
                toast("已删除", "success");
                await load();
            } catch (e) {
                toast(e.message, "error");
            }
        }

        async function showPlan() {
            try {
                plan.value = await api("/api/dns/plan");
            } catch (e) {
                toast(e.message, "error");
            }
        }

        async function sync(r) {
            busy.value = r.id;
            try {
                const res = await api(`/api/dns/records/${r.id}/sync`, { method: "POST", body: {} });
                if (res.found) {
                    toast(`服务商上当前解析为 ${res.value}` +
                        (res.matched_node_name ? `（节点「${res.matched_node_name}」）` : "（不属于任何已知节点）"),
                        "success", 7000);
                } else {
                    toast("服务商上还没有这条记录，第一次切换时会自动创建", "info", 7000);
                }
                await load();
            } catch (e) {
                toast(e.message, "error");
            } finally {
                busy.value = 0;
            }
        }

        async function doSwitch(recordId, nodeId) {
            switching.value = null;
            busy.value = recordId;
            try {
                const d = await api(`/api/dns/records/${recordId}/switch`, {
                    method: "POST", body: { node_id: Number(nodeId) },
                });
                toast(d.change ? `已切换到 ${d.target_value}` : "解析已经是这个值，无需改动", "success");
                await load();
            } catch (e) {
                toast(e.message, "error", 9000);
            } finally {
                busy.value = 0;
            }
        }

        return {
            providers, records, nodes, loading, editProvider, editRecord, plan,
            switching, busy, load, nodeName, removeProvider, removeRecord,
            showPlan, sync, doSwitch, fmtTime,
        };
    },
    template: `
        <div>
            <div class="page-head">
                <div>
                    <h1>域名切换</h1>
                    <p>主力机器流量跑满或者掉线时，自动把域名解析切到备用机器上</p>
                </div>
                <div class="btn-row">
                    <button class="btn" @click="showPlan">切换演练</button>
                    <button class="btn" @click="load">刷新</button>
                </div>
            </div>

            <div class="card">
                <div class="card-title">
                    <span>DNS 服务商</span>
                    <button class="btn small primary" @click="editProvider = { provider: null }">添加</button>
                </div>
                <div v-if="!providers.length" class="empty">
                    先添加一个 DNS 服务商，面板需要用它的 API 来改解析记录。
                </div>
                <div v-else class="table-scroll">
                    <table>
                        <thead><tr><th>名称</th><th>类型</th><th>凭据</th><th>操作</th></tr></thead>
                        <tbody>
                            <tr v-for="p in providers" :key="p.id">
                                <td>{{ p.name }}</td>
                                <td>{{ p.type_label }}</td>
                                <td>
                                    <span class="badge" :class="p.has_credentials ? 'good' : 'critical'">
                                        <span class="dot"></span>{{ p.has_credentials ? '已配置' : '缺失' }}
                                    </span>
                                </td>
                                <td>
                                    <div class="btn-row">
                                        <button class="btn small" @click="editProvider = { provider: p }">编辑</button>
                                        <button class="btn small danger" @click="removeProvider(p)">删除</button>
                                    </div>
                                </td>
                            </tr>
                        </tbody>
                    </table>
                </div>
            </div>

            <div class="card">
                <div class="card-title">
                    <span>域名记录</span>
                    <button class="btn small primary" :disabled="!providers.length"
                            @click="editRecord = { record: null }">添加</button>
                </div>
                <div v-if="!records.length" class="empty">
                    还没有配置域名记录。添加一条，把候选节点按优先级排好，<br>
                    主节点流量跑满或掉线时面板就会自动切到下一个。
                </div>
                <div v-else class="table-scroll">
                    <table>
                        <thead>
                            <tr>
                                <th>记录</th><th>当前解析</th><th>候选节点</th>
                                <th>触发条件</th><th>最近切换</th><th>操作</th>
                            </tr>
                        </thead>
                        <tbody>
                            <tr v-for="r in records" :key="r.id">
                                <td>
                                    <div>{{ r.name }}</div>
                                    <div class="node-meta">
                                        {{ r.record_type }} · TTL {{ r.ttl }}
                                        <template v-if="r.proxied"> · 已开代理</template>
                                        <template v-if="!r.enabled"> · 已停用</template>
                                    </div>
                                </td>
                                <td>
                                    <div class="mono">{{ r.current_value || '未知' }}</div>
                                    <div class="node-meta">{{ nodeName(r.current_node_id) }}</div>
                                </td>
                                <td>
                                    <div v-for="(m, i) in r.members" :key="m.node_id" class="node-meta">
                                        {{ i + 1 }}. {{ m.node_name || nodeName(m.node_id) }}
                                    </div>
                                    <span v-if="!r.members || !r.members.length" class="muted">未配置</span>
                                </td>
                                <td>
                                    <div v-if="r.strategy === 'manual'" class="muted">仅手动</div>
                                    <template v-else>
                                        <div v-if="r.switch_on_exceed" class="node-meta">流量耗尽</div>
                                        <div v-if="r.switch_on_offline" class="node-meta">节点离线</div>
                                        <div v-if="r.switch_on_warn" class="node-meta">达到预警线</div>
                                    </template>
                                </td>
                                <td class="tabular muted">
                                    {{ r.last_switch_at ? fmtTime(r.last_switch_at, true) : '—' }}
                                    <div v-if="r.last_error" class="node-meta" style="color:var(--critical)">
                                        上次失败：{{ r.last_error }}
                                    </div>
                                </td>
                                <td>
                                    <div class="btn-row">
                                        <button class="btn small" :disabled="busy === r.id"
                                                @click="switching = { record: r }">切换</button>
                                        <button class="btn small" :disabled="busy === r.id" @click="sync(r)">同步</button>
                                        <button class="btn small" @click="editRecord = { record: r }">编辑</button>
                                        <button class="btn small danger" @click="removeRecord(r)">删除</button>
                                    </div>
                                </td>
                            </tr>
                        </tbody>
                    </table>
                </div>
            </div>

            <ProviderEditor v-if="editProvider" :provider="editProvider.provider"
                            @close="editProvider = null" @saved="editProvider = null; load()" />

            <RecordEditor v-if="editRecord" :record="editRecord.record"
                          :providers="providers" :nodes="nodes"
                          @close="editRecord = null" @saved="editRecord = null; load()" />

            <Modal v-if="switching" :title="'手动切换 ' + switching.record.name" @close="switching = null">
                <p class="field-hint" style="margin-bottom:12px">
                    手动切换会忽略健康检查和冷却期，立即把解析改到指定节点。
                </p>
                <div class="table-scroll">
                    <table>
                        <thead><tr><th>节点</th><th>解析值</th><th></th></tr></thead>
                        <tbody>
                            <tr v-for="m in switching.record.members" :key="m.node_id">
                                <td>{{ m.node_name || nodeName(m.node_id) }}</td>
                                <td class="mono">
                                    {{ (nodes.find(n => n.id === m.node_id) || {})[switching.record.record_type === 'AAAA' ? 'ipv6' : 'ipv4'] || '—' }}
                                </td>
                                <td>
                                    <button class="btn small primary"
                                            @click="doSwitch(switching.record.id, m.node_id)">切到这台</button>
                                </td>
                            </tr>
                        </tbody>
                    </table>
                </div>
                <div class="modal-actions">
                    <button class="btn" @click="switching = null">取消</button>
                </div>
            </Modal>

            <Modal v-if="plan" title="切换演练（只计算，不改动解析）" wide @close="plan = null">
                <p class="field-hint" style="margin-bottom:12px">
                    这是面板此刻的选主结论。第一次配置时用它确认逻辑符合预期，再交给自动切换。
                </p>
                <div v-for="p in plan" :key="p.record_id" class="card" style="margin-bottom:12px">
                    <div class="card-title">
                        <span>{{ p.record_name }}</span>
                        <span class="badge" :class="p.change ? 'warning' : 'muted'">
                            <span class="dot"></span>{{ p.change ? '会切换' : '不变' }}
                        </span>
                    </div>
                    <p class="field-hint" style="margin-top:-6px">
                        当前 <span class="mono">{{ p.current_value || '未知' }}</span>
                        <template v-if="p.target_value">
                            → 目标 <span class="mono">{{ p.target_value }}</span>（{{ p.target_node_name }}）
                        </template>
                        <template v-if="p.skipped"> · {{ p.skipped }}</template>
                    </p>
                    <table>
                        <thead><tr><th>优先级</th><th>候选节点</th><th>可用</th><th>说明</th></tr></thead>
                        <tbody>
                            <tr v-for="c in p.candidates" :key="c.node_id">
                                <td class="tabular">{{ c.priority }}</td>
                                <td>{{ c.node_name }}</td>
                                <td>
                                    <span class="badge" :class="c.eligible ? 'good' : 'critical'">
                                        <span class="dot"></span>{{ c.eligible ? '可用' : '不可用' }}
                                    </span>
                                </td>
                                <td class="muted">{{ c.reason || '—' }}</td>
                            </tr>
                        </tbody>
                    </table>
                </div>
                <div v-if="!plan.length" class="empty">还没有配置任何域名记录</div>
                <div class="modal-actions">
                    <button class="btn" @click="plan = null">关闭</button>
                </div>
            </Modal>
        </div>
    `,
};
