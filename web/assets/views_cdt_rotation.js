import { computed, onMounted, onUnmounted, reactive, ref } from "vue";
import { api, fmtTime, toast } from "./api.js";

export const CDTRotationPanel = {
    props: { accounts: Array },
    emits: ["updated"],
    setup(props, { emit }) {
        const config = ref({ enabled: false, timezone: "Asia/Shanghai", dns_record_id: 0, slots: [] });
        const state = ref({ pending_stops: {} });
        const records = ref([]);
        const editing = ref(false);
        const saving = ref(false);
        const err = ref("");
        const form = reactive({ enabled: true, timezone: "Asia/Shanghai", dns_record_id: 0, slots: [] });
        let timer;
        let disposed = false;
        const availableRecords = computed(() => records.value.filter(r => r.enabled && r.strategy === "cdt_rotation" && r.record_type === "A"));
        const recordName = computed(() => records.value.find(r => r.id === config.value.dns_record_id)?.name || "未选择域名");
        function instancesFor(id) {
            return (props.accounts?.find(a => a.id === Number(id))?.instances || []).filter(i => i.guarded);
        }
        function instanceName(id) {
            for (const a of props.accounts || []) {
                const i = (a.instances || []).find(i => i.id === Number(id));
                if (i) return `${a.name} · ${i.instance_name || i.instance_id}`;
            }
            return `实例 #${id}`;
        }
        async function load() {
            try {
                const res = await api("/api/cdt/rotation");
                config.value = res.config;
                state.value = res.state;
                if (!editing.value && !saving.value) err.value = "";
                emit("updated", res.config);
            } catch (e) { err.value = e.message; }
        }
        async function edit() {
            err.value = "";
            try { records.value = (await api("/api/dns/records")) || []; }
            catch (e) { err.value = e.message; return; }
            Object.assign(form, JSON.parse(JSON.stringify(config.value)));
            if (!form.slots.length) form.enabled = true;
            editing.value = true;
        }
        function accountChanged(slot) {
            slot.instance_id = instancesFor(slot.account_id)[0]?.id || 0;
        }
        function add() {
            const a = (props.accounts || []).find(a => a.enabled && !form.slots.some(s => s.account_id === a.id));
            form.slots.push({ account_id: a?.id || 0, instance_id: instancesFor(a?.id)[0]?.id || 0, start: "00:00", end: "00:00" });
        }
        function distribute() {
            const accounts = (props.accounts || []).filter(a => a.enabled && instancesFor(a.id).length);
            if (!accounts.length) { err.value = "请先添加账号，并将参与换班的实例设为受守护"; return; }
            const clock = m => `${String(Math.floor(m / 60)).padStart(2, "0")}:${String(m % 60).padStart(2, "0")}`;
            form.slots = accounts.map((a, i) => ({
                account_id: a.id, instance_id: instancesFor(a.id)[0].id,
                start: clock(Math.floor(1440 * i / accounts.length)),
                end: clock(Math.floor(1440 * (i + 1) / accounts.length) % 1440),
            }));
            err.value = "";
        }
        async function save(body = form) {
            saving.value = true; err.value = "";
            try {
                const res = await api("/api/cdt/rotation", { method: "PUT", body: JSON.parse(JSON.stringify(body)) });
                config.value = res.config; state.value = res.state;
                emit("updated", res.config);
                editing.value = false;
                toast(res.config.enabled ? "换班计划已启用" : "换班计划已停用", "success");
            } catch (e) { err.value = e.message; }
            finally { saving.value = false; }
        }
        function disable() { return save({ ...config.value, enabled: false }); }
        onMounted(async () => {
            await load();
            try { records.value = (await api("/api/dns/records")) || []; } catch (_) { /* edit retries and displays errors */ }
            if (!disposed) timer = setInterval(load, 15000);
        });
        onUnmounted(() => { disposed = true; clearInterval(timer); });
        return { config, state, editing, saving, err, form, availableRecords, recordName,
            instancesFor, instanceName, edit, add, distribute, save, disable, load, accountChanged, fmtTime };
    },
    template: `
      <div class="card" style="margin-bottom:18px">
        <div class="page-head" style="margin-bottom:12px">
          <div><div class="card-title">CDT 账号换班 <span class="badge" :class="config.enabled ? 'good' : 'muted'">{{ config.enabled ? '已启用' : '未启用' }}</span></div>
            <p class="field-hint">按窗口切换 DDNS，旧实例保留 30 分钟后节省关机。流量耗尽立即关机，次月恢复参与换班。</p>
          </div>
          <div class="btn-row" v-if="!editing">
            <button class="btn" @click="load">刷新状态</button>
            <button class="btn primary" @click="edit">配置窗口</button>
            <button v-if="config.enabled" class="btn" :disabled="saving" @click="disable">停用计划</button>
          </div>
        </div>
        <div v-if="err" class="notice error">{{ err }}</div>
        <template v-if="!editing && config.slots.length">
          <p>{{ recordName }} · {{ config.timezone }}</p>
          <div v-for="slot in config.slots" :key="slot.account_id" class="node-meta">{{ slot.start }}–{{ slot.end }}：{{ instanceName(slot.instance_id) }}</div>
          <p v-if="state.current_instance_id">当前解析：{{ instanceName(state.current_instance_id) }} → <code>{{ state.current_ip }}</code>（{{ fmtTime(state.last_switch_at) }}）</p>
          <p v-else-if="config.enabled" class="field-hint">等待核实额度、实例运行状态及 DDNS。</p>
          <div v-for="(deadline, id) in state.pending_stops" :key="id" class="notice">{{ instanceName(id) }}：计划在 {{ fmtTime(deadline) }} 节省关机</div>
          <div v-if="state.last_error" class="notice error">{{ state.last_error }}</div>
        </template>
        <template v-if="editing">
          <div class="field"><label class="checkbox-label"><input type="checkbox" v-model="form.enabled">启用换班计划（保存后开始调度）</label></div>
          <div class="field"><label>DDNS 域名</label><select v-model.number="form.dns_record_id">
            <option :value="0">请选择域名</option>
            <option v-for="r in availableRecords" :key="r.id" :value="r.id">{{ r.name }}</option>
          </select><p class="field-hint">先在 <a href="#/dns">域名管理</a> 添加 A 记录，切换策略选择「CDT 账号换班」，TTL 不超过 1800 秒。</p></div>
          <div class="field"><label>窗口时区</label><input v-model="form.timezone" placeholder="Asia/Shanghai"></div>
          <div class="btn-row" style="margin-bottom:12px"><button class="btn" @click="add">添加账号窗口</button><button class="btn" @click="distribute">按账号均分全天</button></div>
          <div v-for="(slot, index) in form.slots" :key="index" class="card" style="margin-bottom:12px">
            <div class="field"><label>账号</label><select v-model.number="slot.account_id" @change="accountChanged(slot)">
              <option :value="0">请选择账号</option><option v-for="a in accounts" :key="a.id" :value="a.id" :disabled="!a.enabled">{{ a.name }}</option>
            </select></div>
            <div class="field"><label>受守护实例</label><select v-model.number="slot.instance_id">
              <option :value="0">请选择实例（需要先勾选受守护）</option>
              <option v-for="i in instancesFor(slot.account_id)" :key="i.id" :value="i.id">{{ i.instance_name || i.instance_id }} · {{ i.public_ip || '尚无公网 IP' }}</option>
            </select></div>
            <div class="btn-row"><label>开始 <input type="time" v-model="slot.start" step="60"></label><label>结束 <input type="time" v-model="slot.end" step="60"></label><button class="btn danger" @click="form.slots.splice(index, 1)">移除</button></div>
          </div>
          <p class="field-hint">窗口需覆盖全天且互不重叠，午夜填 00:00。例如 A 00:00–08:00、B 08:00–16:00、C 16:00–00:00。单账号开始与结束相同表示全天。</p>
          <p class="field-hint">额度按账号已有熔断线判断。当班账号耗尽时，按后续窗口顺序选择有额度的账号。DDNS 失败会保留旧实例；超限停机不等待 30 分钟。所有账号耗尽时保持停机，月初核实额度后按当前窗口恢复。</p>
          <p class="field-hint">启用期间，参与账号的独立定时开关机、保活和手动开关机由此计划接管。修改计划会重新计算等待时间；停用计划会取消待执行的换班停机。</p>
          <div class="btn-row"><button class="btn primary" :disabled="saving" @click="save()">{{ saving ? '保存中…' : '保存计划' }}</button><button class="btn" :disabled="saving" @click="editing = false">取消</button></div>
        </template>
      </div>
    `,
};
