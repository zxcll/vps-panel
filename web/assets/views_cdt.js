// 阿里云 CDT 管理：账号凭据、免费额度用量、ECS 实例的守护与开关机。
//
// 这一栏和面板的节点账本是**完全分开**的两套东西。阿里云 CDT 按账号统计、
// 只算出方向、还分中国内地/非中国内地两个池；探针那套是按单机网卡算的。
// 两者口径不同，页面上也就不放在一起比。

import { computed, onMounted, reactive, ref } from "vue";
import {
    api, CDT_REGIONS, CDT_SHUTDOWN_MODES, CDT_SITES,
    COMMON_TIMEZONES, fmtBytes, fmtTime, toast,
} from "./api.js";
import { Modal } from "./components.js";

const AccountEditor = {
    components: { Modal },
    props: { account: Object },
    emits: ["close", "saved"],
    setup(props, { emit }) {
        const isEdit = computed(() => !!props.account);
        const a = props.account;

        const form = reactive({
            name: a?.name || "",
            access_key_id: a?.access_key_id || "",
            access_key_secret: "",
            region_id: a?.region_id || "cn-hongkong",
            site_type: a?.site_type || "international",
            // 0 交给后端回落成阿里云的官方额度（20 / 200），
            // 不在前端写死，免得规则变了两边对不上。
            quota_mainland_gb: a?.quota_mainland_gb ?? 20,
            quota_overseas_gb: a?.quota_overseas_gb ?? 200,
            threshold_percent: a?.threshold_percent ?? 95,
            outstanding_threshold: a?.outstanding_threshold ?? 0,
            shutdown_mode: a?.shutdown_mode || "StopCharging",
            sync_interval_sec: a?.sync_interval_sec || 300,
            keep_alive: a?.keep_alive ?? false,
            auto_start_time: a?.auto_start_time || "",
            auto_stop_time: a?.auto_stop_time || "",
            schedule_tz: a?.schedule_tz || "Asia/Shanghai",
            enabled: a?.enabled ?? true,
        });

        const saving = ref(false);
        const err = ref("");

        const canSave = computed(() =>
            form.name.trim() && form.access_key_id.trim() && form.region_id &&
            (isEdit.value || form.access_key_secret.trim()));

        async function save() {
            err.value = "";
            saving.value = true;
            const body = {
                ...form,
                quota_mainland_gb: Number(form.quota_mainland_gb) || 0,
                quota_overseas_gb: Number(form.quota_overseas_gb) || 0,
                threshold_percent: Number(form.threshold_percent) || 0,
                outstanding_threshold: Number(form.outstanding_threshold) || 0,
                sync_interval_sec: Number(form.sync_interval_sec) || 0,
            };
            try {
                if (isEdit.value) {
                    await api(`/api/cdt/accounts/${a.id}`, { method: "PUT", body });
                } else {
                    await api("/api/cdt/accounts", { method: "POST", body });
                }
                toast("已保存", "success");
                emit("saved");
            } catch (e) {
                err.value = e.message;
            } finally {
                saving.value = false;
            }
        }

        return {
            form, saving, err, save, isEdit, canSave,
            CDT_REGIONS, CDT_SITES, CDT_SHUTDOWN_MODES, COMMON_TIMEZONES,
        };
    },
    template: `
        <Modal :title="isEdit ? '编辑阿里云账号' : '添加阿里云账号'" wide @close="$emit('close')">
            <div v-if="err" class="notice error">{{ err }}</div>

            <div class="field-row">
                <div class="field">
                    <label>账号名称 *</label>
                    <input type="text" v-model="form.name" placeholder="如：香港抢占式">
                </div>
                <div class="field">
                    <label>站点 *</label>
                    <select v-model="form.site_type">
                        <option v-for="s in CDT_SITES" :key="s.value" :value="s.value">{{ s.label }}</option>
                    </select>
                    <div class="field-hint">只影响账单接口的域名和记账货币</div>
                </div>
            </div>

            <div class="field">
                <label>AccessKeyId *</label>
                <input type="text" v-model="form.access_key_id" class="mono">
            </div>
            <div class="field">
                <label>AccessKeySecret {{ isEdit ? '' : '*' }}</label>
                <input type="password" v-model="form.access_key_secret"
                       :placeholder="isEdit ? '留空表示不修改已保存的凭据' : ''">
                <div class="field-hint">
                    AES-256-GCM 加密后落库，从不回显。保存前面板会先拿它调一次接口验证可用性。<br>
                    RAM 用户需要的权限：<b>CDT</b>（读流量）、<b>ECS</b>（读实例；用到开关机才需要写权限）、
                    <b>BSS</b>（读余额账单，可选）。<b>只做监控的话建议只给只读策略。</b>
                </div>
            </div>

            <div class="field">
                <label>ECS 地域 *</label>
                <select v-model="form.region_id">
                    <option v-for="r in CDT_REGIONS" :key="r.value" :value="r.value">{{ r.label }}</option>
                </select>
                <div class="field-hint">
                    实例在哪个地域就填哪个。CDT 流量本身是账号级的，不受这个影响；
                    它只决定去哪个地域拉实例列表。
                </div>
            </div>

            <h3 style="margin:18px 0 6px">免费额度与熔断</h3>
            <div class="field-row">
                <div class="field">
                    <label>中国内地额度（GB/月）</label>
                    <input type="number" min="0" step="1" v-model="form.quota_mainland_gb">
                </div>
                <div class="field">
                    <label>非中国内地额度（GB/月）</label>
                    <input type="number" min="0" step="1" v-model="form.quota_overseas_gb">
                </div>
            </div>
            <div class="field-hint" style="margin-top:-6px">
                阿里云的 CDT 免费额度是<b>分两个池</b>算的，不是一个总额度：中国内地 20GB、
                非中国内地 200GB，每自然月 1 日 0 点（北京时间）刷新，不结转。
                <b>只统计出方向</b>，入方向不计费也不消耗额度。
                香港、澳门、台湾虽然地域 ID 是 cn- 开头，计费上算<b>非中国内地</b>。
                填 0 表示用上面这套官方默认值。
            </div>

            <div class="field-row" style="margin-top:12px">
                <div class="field">
                    <label>流量熔断线（%）</label>
                    <input type="number" min="0" step="1" v-model="form.threshold_percent">
                    <div class="field-hint">
                        任意一个池用到这个比例就停机。两个池<b>分别判</b>，不是加起来判。
                    </div>
                </div>
                <div class="field">
                    <label>待还金额熔断线</label>
                    <input type="number" min="0" step="0.01" v-model="form.outstanding_threshold">
                    <div class="field-hint">待还超过这个数也停机。填 0 表示不启用这一条</div>
                </div>
            </div>

            <div class="field">
                <label>停机方式</label>
                <select v-model="form.shutdown_mode">
                    <option v-for="m in CDT_SHUTDOWN_MODES" :key="m.value" :value="m.value">{{ m.label }}</option>
                </select>
                <div class="field-hint">
                    {{ (CDT_SHUTDOWN_MODES.find(m => m.value === form.shutdown_mode) || {}).hint }}
                </div>
            </div>

            <div class="field">
                <label>同步间隔（秒）</label>
                <input type="number" min="10" max="86400" step="10" v-model="form.sync_interval_sec">
                <div class="field-hint">
                    多久去阿里云查一次。<b>CDT 流量、余额账单、实例状态、抢占式实例保活</b>都按它走，
                    默认 300 秒。<br>
                    设短一点主要是让<b>保活更快</b>——实例被回收后最多隔这么久就会被拉起来。
                    流量数字不会因此更实时：阿里云的 CDT 统计本身就有小时级延迟，
                    查得再勤也是同一个值。<br>
                    每轮要打三四次阿里云接口，别设得太小，免得撞上人家的调用频率限制。<br>
                    <b>定时开关机不受影响</b>，它固定一分钟看一次——那是按钟点触发的，
                    间隔拉长会直接错过时间点。
                </div>
            </div>

            <h3 style="margin:18px 0 6px">保活与定时</h3>
            <label class="checkbox-label">
                <input type="checkbox" v-model="form.keep_alive">
                抢占式实例被回收后自动拉起
            </label>
            <div class="field-hint" style="margin-bottom:12px">
                每分钟查一次受守护的抢占式实例，发现停了就重新启动。
                可用区售罄（NoStock）时只在第一次告警，之后静默重试，恢复了再报一次。
                账号处于熔断状态时保活不生效——那时候机器是面板故意停的。
            </div>

            <div class="field-row">
                <div class="field">
                    <label>定时开机</label>
                    <input type="text" v-model="form.auto_start_time" placeholder="如 08:00，留空不启用">
                </div>
                <div class="field">
                    <label>定时关机</label>
                    <input type="text" v-model="form.auto_stop_time" placeholder="如 23:30，留空不启用">
                </div>
                <div class="field">
                    <label>定时用的时区</label>
                    <select v-model="form.schedule_tz">
                        <option v-for="tz in COMMON_TIMEZONES" :key="tz" :value="tz">{{ tz }}</option>
                    </select>
                </div>
            </div>
            <div class="field-hint">
                只对<b>受守护</b>的实例生效。判定的是「此刻的 HH:MM 是否等于设定值」，
                所以面板停机超过一分钟会错过那个点，不会补执行——
                补一个半小时前的关机指令比错过它更让人意外。
            </div>

            <label class="checkbox-label" style="margin-top:14px">
                <input type="checkbox" v-model="form.enabled">
                启用这个账号（关掉后面板不再同步，也不会对它执行任何动作）
            </label>

            <div class="modal-actions">
                <button class="btn" @click="$emit('close')">取消</button>
                <button class="btn primary" :disabled="saving || !canSave" @click="save">
                    <span v-if="saving" class="spinner"></span>保存
                </button>
            </div>
        </Modal>
    `,
};

export const CDTView = {
    components: { AccountEditor, Modal },
    setup() {
        const accounts = ref([]);
        const loading = ref(true);
        const editAccount = ref(null);
        const showEditor = ref(false);
        const busy = ref(0);
        const instBusy = ref(0);

        async function load() {
            loading.value = true;
            try {
                accounts.value = (await api("/api/cdt/accounts")) || [];
            } catch (e) {
                toast(e.message, "error");
            } finally {
                loading.value = false;
            }
        }

        onMounted(load);

        function openNew() {
            editAccount.value = null;
            showEditor.value = true;
        }

        function openEdit(a) {
            editAccount.value = a;
            showEditor.value = true;
        }

        async function onSaved() {
            showEditor.value = false;
            editAccount.value = null;
            await load();
        }

        async function remove(a) {
            if (!window.confirm(
                `删除账号「${a.name}」会连带删掉它的实例、流量和账单记录。\n` +
                `只删面板里的数据，不会动阿里云上的任何东西。继续？`)) return;
            try {
                await api(`/api/cdt/accounts/${a.id}`, { method: "DELETE" });
                toast("已删除", "success");
                await load();
            } catch (e) {
                toast(e.message, "error");
            }
        }

        async function syncNow(a) {
            busy.value = a.id;
            try {
                await api(`/api/cdt/accounts/${a.id}/sync`, { method: "POST", body: {} });
                toast("已同步", "success");
                await load();
            } catch (e) {
                toast(e.message, "error", 9000);
            } finally {
                busy.value = 0;
            }
        }

        async function test(a) {
            busy.value = a.id;
            try {
                const res = await api(`/api/cdt/accounts/${a.id}/test`, { method: "POST", body: {} });
                toast(res.message, "success", 7000);
            } catch (e) {
                toast(e.message, "error", 12000);
            } finally {
                busy.value = 0;
            }
        }

        async function toggleGuard(inst) {
            instBusy.value = inst.id;
            try {
                await api(`/api/cdt/instances/${inst.id}/guard`, {
                    method: "POST", body: { guarded: !inst.guarded },
                });
                await load();
            } catch (e) {
                toast(e.message, "error");
            } finally {
                instBusy.value = 0;
            }
        }

        async function power(inst, start) {
            const what = start ? "启动" : "停止";
            if (!start && !window.confirm(
                `确定要停止实例「${inst.instance_name || inst.instance_id}」吗？\n` +
                `这会真的关掉阿里云上的这台机器。`)) return;

            instBusy.value = inst.id;
            try {
                const res = await api(`/api/cdt/instances/${inst.id}/${start ? "start" : "stop"}`,
                    { method: "POST", body: {} });
                toast(res.message || `已下发${what}指令`, "success");
                await load();
            } catch (e) {
                toast(e.message, "error", 12000);
            } finally {
                instBusy.value = 0;
            }
        }

        function statusMeta(status) {
            switch (status) {
                case "Running": return { cls: "good", text: "运行中" };
                case "Stopped": return { cls: "critical", text: "已停止" };
                case "Starting": return { cls: "warning", text: "启动中" };
                case "Stopping": return { cls: "warning", text: "停止中" };
                default: return { cls: "muted", text: status || "未知" };
            }
        }

        function meterClass(u) {
            if (u.percent >= 100) return "critical";
            if (u.percent >= 80) return "warning";
            return "";
        }

        return {
            accounts, loading, editAccount, showEditor, busy, instBusy,
            load, openNew, openEdit, onSaved, remove, syncNow, test,
            toggleGuard, power, statusMeta, meterClass, fmtBytes, fmtTime,
        };
    },
    template: `
        <div>
            <div class="page-head">
                <div>
                    <h1>阿里云 CDT</h1>
                    <p>CDT 免费额度监控、超额自动停机、抢占式实例保活与定时开关机</p>
                </div>
                <div class="btn-row">
                    <button class="btn primary" @click="openNew">添加账号</button>
                    <button class="btn" @click="load">刷新</button>
                </div>
            </div>

            <div v-if="loading" class="empty"><span class="spinner"></span>加载中</div>

            <div v-else-if="!accounts.length" class="empty">
                还没有添加阿里云账号。<br>
                添加后面板会定期拉取 CDT 流量、ECS 实例和账单，
                并按你设的阈值自动停机、给抢占式实例保活。
            </div>

            <div v-for="a in accounts" :key="a.id" class="card" style="margin-bottom:18px">
                <div class="page-head" style="margin-bottom:12px">
                    <div>
                        <div class="card-title">
                            {{ a.name }}
                            <span class="badge muted">{{ a.site_label }}</span>
                            <span class="badge muted">{{ a.region_id }}</span>
                            <span v-if="!a.enabled" class="badge muted"><span class="dot"></span>已停用</span>
                            <span v-if="a.tripped_reason" class="badge critical">
                                <span class="dot"></span>已熔断
                            </span>
                        </div>
                        <div class="node-meta">
                            账期 {{ a.cycle }} ·
                            上次同步 {{ fmtTime(a.last_sync_at, true) }}
                            （每 {{ a.sync_interval_sec || 300 }} 秒一次） ·
                            实例 {{ a.instances.length }} 台（受守护 {{ a.guarded_count }}）
                            <template v-if="a.keep_alive"> · 保活已开</template>
                            <template v-if="a.auto_start_time || a.auto_stop_time">
                                · 定时 {{ a.auto_start_time || '—' }} 开 / {{ a.auto_stop_time || '—' }} 关
                                （{{ a.schedule_tz }}）
                            </template>
                        </div>
                    </div>
                    <div class="btn-row">
                        <button class="btn small" :disabled="busy === a.id" @click="test(a)">
                            <span v-if="busy === a.id" class="spinner"></span>测试凭据
                        </button>
                        <button class="btn small" :disabled="busy === a.id" @click="syncNow(a)">立即同步</button>
                        <button class="btn small" @click="openEdit(a)">编辑</button>
                        <button class="btn small danger" @click="remove(a)">删除</button>
                    </div>
                </div>

                <div v-if="a.last_error" class="notice error" style="margin-bottom:12px">
                    上次同步失败：{{ a.last_error }}
                </div>
                <div v-if="a.tripped_reason" class="notice error" style="margin-bottom:12px">
                    <b>已熔断停机</b>（账期 {{ a.tripped_cycle }}）：{{ a.tripped_reason }}<br>
                    新账期开始时会自动解除并把受守护的实例拉起来；也可以手动启动实例来立即解除。
                </div>
                <div v-if="a.nostock_notified" class="notice error" style="margin-bottom:12px">
                    抢占式实例所在可用区售罄，保活正在持续重试。
                </div>

                <!-- 两个额度池分开显示。这是这一栏最重要的一件事：
                     加起来看的话，中国内地那 20GB 跑满了也看不出来。 -->
                <div class="field-row" style="gap:20px">
                    <div v-for="u in a.usage.buckets" :key="u.bucket" style="flex:1">
                        <div class="meter">
                            <div class="meter-head">
                                <span>{{ u.label }}（出方向）</span>
                                <span class="tabular">
                                    {{ fmtBytes(u.used_bytes) }} / {{ fmtBytes(u.quota_bytes) }}
                                    （{{ u.percent.toFixed(1) }}%）
                                </span>
                            </div>
                            <div class="meter-track">
                                <div class="meter-fill" :class="meterClass(u)"
                                     :style="{ width: Math.min(100, u.percent) + '%' }"></div>
                            </div>
                        </div>
                    </div>
                </div>

                <div v-if="a.bill" class="node-meta" style="margin-top:10px">
                    余额 {{ a.bill.symbol }}{{ a.bill.available_amount.toFixed(2) }} ·
                    本账期待还 {{ a.bill.symbol }}{{ a.bill.outstanding.toFixed(2) }}
                    <template v-if="a.outstanding_threshold > 0">
                        （待还熔断线 {{ a.bill.symbol }}{{ a.outstanding_threshold.toFixed(2) }}）
                    </template>
                </div>

                <details v-if="a.regions.length" style="margin-top:10px">
                    <summary class="node-meta" style="cursor:pointer">
                        逐地域明细（{{ a.regions.length }} 个地域）
                    </summary>
                    <div class="table-scroll" style="margin-top:8px">
                        <table>
                            <thead>
                                <tr>
                                    <th>业务地域</th>
                                    <th style="width:140px">归入额度池</th>
                                    <th style="width:110px">线路</th>
                                    <th style="width:130px">出方向流量</th>
                                </tr>
                            </thead>
                            <tbody>
                                <tr v-for="r in a.regions" :key="r.business_region_id">
                                    <td class="mono">{{ r.business_region_id }}</td>
                                    <td>{{ r.bucket_label }}</td>
                                    <td>{{ r.traffic_type || '—' }}</td>
                                    <td class="tabular">{{ fmtBytes(r.traffic_bytes) }}</td>
                                </tr>
                            </tbody>
                        </table>
                    </div>
                </details>

                <div class="table-scroll" style="margin-top:14px">
                    <table>
                        <thead>
                            <tr>
                                <th>实例</th>
                                <th style="width:110px">状态</th>
                                <th style="width:140px">公网 IP</th>
                                <th style="width:130px">规格</th>
                                <th style="width:100px">受守护</th>
                                <th style="width:200px">操作</th>
                            </tr>
                        </thead>
                        <tbody>
                            <tr v-if="!a.instances.length">
                                <td colspan="6" class="muted">
                                    这个地域下没有实例。地域填错了，或者凭据没有 ECS 读权限。
                                </td>
                            </tr>
                            <tr v-for="inst in a.instances" :key="inst.id">
                                <td>
                                    {{ inst.instance_name || '（未命名）' }}
                                    <span v-if="inst.is_spot" class="badge warning">抢占式</span>
                                    <div class="node-meta mono">{{ inst.instance_id }}</div>
                                </td>
                                <td>
                                    <span class="badge" :class="statusMeta(inst.status).cls">
                                        <span class="dot"></span>{{ statusMeta(inst.status).text }}
                                    </span>
                                </td>
                                <td class="mono">{{ inst.public_ip || '—' }}</td>
                                <td>
                                    {{ inst.instance_type || '—' }}
                                    <div v-if="inst.bandwidth_mbps" class="node-meta">
                                        {{ inst.bandwidth_mbps }} Mbps
                                    </div>
                                </td>
                                <td>
                                    <label class="checkbox-label">
                                        <input type="checkbox" :checked="inst.guarded"
                                               :disabled="instBusy === inst.id"
                                               @change="toggleGuard(inst)">
                                    </label>
                                </td>
                                <td>
                                    <div class="btn-row">
                                        <button class="btn small" :disabled="instBusy === inst.id"
                                                @click="power(inst, true)">启动</button>
                                        <button class="btn small danger" :disabled="instBusy === inst.id"
                                                @click="power(inst, false)">停止</button>
                                    </div>
                                </td>
                            </tr>
                        </tbody>
                    </table>
                </div>
                <div class="field-hint" style="margin-top:8px">
                    只有勾了<b>受守护</b>的实例才会被熔断停机、保活拉起和定时开关机管到。
                    没勾的实例面板只看不动。
                </div>
            </div>

            <AccountEditor v-if="showEditor" :account="editAccount"
                           @close="showEditor = false" @saved="onSaved" />
        </div>
    `,
};
