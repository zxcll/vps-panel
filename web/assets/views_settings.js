// 设置页与事件日志页。

import { onMounted, reactive, ref } from "vue";
import { api, fmtTime, levelMeta, setToken, toast } from "./api.js";

export const SettingsView = {
    setup() {
        const cfg = reactive({});
        const loading = ref(true);
        const saving = ref(false);
        const tokenTouched = ref(false);

        const pw = reactive({ old_password: "", new_password: "", confirm: "", new_username: "" });
        const pwSaving = ref(false);

        async function load() {
            loading.value = true;
            try {
                Object.assign(cfg, await api("/api/settings"));
            } catch (e) {
                toast(e.message, "error");
            } finally {
                loading.value = false;
            }
        }

        onMounted(load);

        async function save() {
            saving.value = true;
            try {
                const body = {
                    offline_after_sec: Number(cfg.offline_after_sec),
                    fail_threshold: Number(cfg.fail_threshold),
                    switch_cooldown_sec: Number(cfg.switch_cooldown_sec),
                    tcp_probe_enabled: !!cfg.tcp_probe_enabled,
                    tcp_probe_timeout_ms: Number(cfg.tcp_probe_timeout_ms),
                    hourly_retain_days: Number(cfg.hourly_retain_days),
                    notify_telegram_token: cfg.notify_telegram_token || "",
                    notify_telegram_chat_id: cfg.notify_telegram_chat_id || "",
                    notify_webhook_url: cfg.notify_webhook_url || "",
                    telegram_token_changed: tokenTouched.value,
                };
                Object.assign(cfg, await api("/api/settings", { method: "PUT", body }));
                tokenTouched.value = false;
                toast("设置已保存", "success");
            } catch (e) {
                toast(e.message, "error", 8000);
            } finally {
                saving.value = false;
            }
        }

        async function changePassword() {
            if (pw.new_password !== pw.confirm) {
                toast("两次输入的新密码不一致", "error");
                return;
            }
            pwSaving.value = true;
            try {
                const res = await api("/api/auth/password", {
                    method: "POST",
                    body: {
                        old_password: pw.old_password,
                        new_password: pw.new_password,
                        new_username: pw.new_username,
                    },
                });
                toast(res.message, "success", 6000);
                setToken("");
            } catch (e) {
                toast(e.message, "error");
            } finally {
                pwSaving.value = false;
            }
        }

        return { cfg, loading, saving, save, pw, pwSaving, changePassword, tokenTouched };
    },
    template: `
        <div>
            <div class="page-head">
                <div>
                    <h1>设置</h1>
                    <p>健康判定、切换防抖、通知渠道和登录凭据</p>
                </div>
            </div>

            <div v-if="loading" class="empty"><span class="spinner"></span> 加载中…</div>

            <template v-else>
                <div class="card">
                    <div class="card-title">健康判定</div>
                    <div class="field-row">
                        <div class="field">
                            <label>离线判定阈值（秒）</label>
                            <input type="number" min="30" max="3600" v-model="cfg.offline_after_sec">
                            <div class="field-hint">
                                心跳超过这个时长没来就认为探针失联。探针默认 30 秒上报一次，
                                这里建议至少给 90 秒的余量
                            </div>
                        </div>
                        <div class="field">
                            <label>连续失败几次才判定离线</label>
                            <input type="number" min="1" max="20" v-model="cfg.fail_threshold">
                            <div class="field-hint">避免一次网络抖动就把域名切走</div>
                        </div>
                    </div>
                    <div class="field-row">
                        <div class="field">
                            <label>拨测超时（毫秒）</label>
                            <input type="number" min="500" max="60000" v-model="cfg.tcp_probe_timeout_ms">
                        </div>
                        <div class="field" style="display:flex;align-items:flex-end">
                            <label class="checkbox-label" style="margin-bottom:9px">
                                <input type="checkbox" v-model="cfg.tcp_probe_enabled">
                                探针失联后再做一次 TCP 拨测确认
                            </label>
                        </div>
                    </div>
                    <div class="field-hint">
                        拨测的意义：探针进程崩了但机器还在正常跑业务时，
                        端口拨得通就不该把域名切走。关掉这个开关会让误切概率明显上升。
                    </div>
                </div>

                <div class="card">
                    <div class="card-title">切换防抖与数据保留</div>
                    <div class="field-row">
                        <div class="field">
                            <label>切换冷却时间（秒）</label>
                            <input type="number" min="0" max="86400" v-model="cfg.switch_cooldown_sec">
                            <div class="field-hint">
                                两次自动切换之间的最小间隔，防止节点状态在临界点抖动时域名被来回改。
                                手动切换不受此限制
                            </div>
                        </div>
                        <div class="field">
                            <label>流量曲线保留天数</label>
                            <input type="number" min="7" max="3650" v-model="cfg.hourly_retain_days">
                            <div class="field-hint">超过天数的小时级数据会被清理，历史周期归档不受影响</div>
                        </div>
                    </div>
                </div>

                <div class="card">
                    <div class="card-title">通知</div>
                    <div class="field-row">
                        <div class="field">
                            <label>Telegram Bot Token</label>
                            <input type="text" v-model="cfg.notify_telegram_token" @input="tokenTouched = true">
                            <div class="field-hint">找 @BotFather 创建机器人拿到。显示的是打码值，改了才会覆盖</div>
                        </div>
                        <div class="field">
                            <label>Telegram Chat ID</label>
                            <input type="text" v-model="cfg.notify_telegram_chat_id">
                            <div class="field-hint">给机器人发条消息，再找 @userinfobot 拿自己的 ID</div>
                        </div>
                    </div>
                    <div class="field">
                        <label>Webhook 地址</label>
                        <input type="text" v-model="cfg.notify_webhook_url" placeholder="https://…">
                        <div class="field-hint">
                            面板会往这个地址 POST 一个 JSON，字段为 level / title / body / node_name / time
                        </div>
                    </div>
                    <div class="field-hint">
                        会推送的事件：节点离线与恢复、流量预警、流量耗尽、超额动作执行结果、域名切换、周期清零。
                    </div>
                </div>

                <div class="btn-row" style="margin-bottom:20px">
                    <button class="btn primary" :disabled="saving" @click="save">
                        <span v-if="saving" class="spinner"></span>保存设置
                    </button>
                </div>

                <div class="card">
                    <div class="card-title">修改登录凭据</div>
                    <div class="field-row">
                        <div class="field">
                            <label>当前密码</label>
                            <input type="password" v-model="pw.old_password">
                        </div>
                        <div class="field">
                            <label>新用户名（可选）</label>
                            <input type="text" v-model="pw.new_username" placeholder="留空则不改">
                        </div>
                    </div>
                    <div class="field-row">
                        <div class="field">
                            <label>新密码</label>
                            <input type="password" v-model="pw.new_password">
                            <div class="field-hint">至少 8 位</div>
                        </div>
                        <div class="field">
                            <label>确认新密码</label>
                            <input type="password" v-model="pw.confirm">
                        </div>
                    </div>
                    <button class="btn" :disabled="pwSaving || !pw.old_password || !pw.new_password"
                            @click="changePassword">
                        <span v-if="pwSaving" class="spinner"></span>修改并重新登录
                    </button>
                    <div class="field-hint" style="margin-top:10px">
                        面板存有各节点的 SSH 凭据，属于高价值目标。
                        建议只绑定 127.0.0.1，前面挂 Nginx 反代并启用 HTTPS，不要直接暴露在公网。
                    </div>
                </div>
            </template>
        </div>
    `,
};

export const EventsView = {
    setup() {
        const events = ref([]);
        const loading = ref(true);
        const level = ref("");

        async function load() {
            loading.value = true;
            try {
                const q = level.value ? `?level=${level.value}&limit=400` : "?limit=400";
                events.value = (await api("/api/events" + q)) || [];
            } catch (e) {
                toast(e.message, "error");
            } finally {
                loading.value = false;
            }
        }

        onMounted(load);

        function setLevel(l) {
            level.value = l;
            load();
        }

        return { events, loading, level, load, setLevel, fmtTime, levelMeta };
    },
    template: `
        <div>
            <div class="page-head">
                <div>
                    <h1>事件日志</h1>
                    <p>上下线、流量预警、关机动作、域名切换、周期清零都会记在这里</p>
                </div>
                <div class="btn-row">
                    <div class="seg">
                        <button v-for="l in [['','全部'],['info','信息'],['warn','警告'],['error','错误']]"
                                :key="l[0]" :class="{ active: level === l[0] }" @click="setLevel(l[0])">
                            {{ l[1] }}
                        </button>
                    </div>
                    <button class="btn" @click="load">刷新</button>
                </div>
            </div>

            <div class="card">
                <div v-if="loading" class="empty"><span class="spinner"></span> 加载中…</div>
                <div v-else-if="!events.length" class="empty">暂无事件</div>
                <div v-else class="table-scroll">
                    <table>
                        <thead><tr><th style="width:160px">时间</th><th style="width:80px">级别</th><th style="width:120px">类型</th><th>内容</th></tr></thead>
                        <tbody>
                            <tr v-for="e in events" :key="e.id">
                                <td class="tabular muted">{{ fmtTime(e.created_at, true) }}</td>
                                <td>
                                    <span class="badge" :class="levelMeta(e.level).cls">
                                        <span class="dot"></span>{{ levelMeta(e.level).text }}
                                    </span>
                                </td>
                                <td class="muted">{{ e.type }}</td>
                                <td style="white-space:pre-wrap">{{ e.message }}</td>
                            </tr>
                        </tbody>
                    </table>
                </div>
            </div>
        </div>
    `,
};
