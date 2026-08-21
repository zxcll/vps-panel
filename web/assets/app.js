// 应用入口：登录门禁 + 基于 hash 的极简路由。

import { computed, createApp, onMounted, onBeforeUnmount, ref } from "vue";
import { api, session, setToken, toast, toasts } from "./api.js";
import { Toasts } from "./components.js";
import { NodeDetailView, OverviewView } from "./views_overview.js";
import { NodesView } from "./views_nodes.js";
import { DNSView } from "./views_dns.js";
import { EventsView, SettingsView } from "./views_settings.js";

const LoginView = {
    setup() {
        const username = ref("admin");
        const password = ref("");
        const busy = ref(false);
        const err = ref("");

        async function submit() {
            err.value = "";
            busy.value = true;
            try {
                const res = await api("/api/auth/login", {
                    method: "POST",
                    body: { username: username.value, password: password.value },
                });
                setToken(res.token, res.username);
            } catch (e) {
                err.value = e.message;
            } finally {
                busy.value = false;
            }
        }

        return { username, password, busy, err, submit };
    },
    template: `
        <div class="login-page">
            <form class="login-box" @submit.prevent="submit">
                <h1>VPS 流量面板</h1>
                <p>首次使用请查看面板启动日志里打印的初始密码</p>

                <div v-if="err" class="notice error">{{ err }}</div>

                <div class="field">
                    <label>用户名</label>
                    <input type="text" v-model="username" autocomplete="username">
                </div>
                <div class="field">
                    <label>密码</label>
                    <input type="password" v-model="password" autocomplete="current-password" autofocus>
                </div>
                <button class="btn primary" type="submit" :disabled="busy || !password" style="width:100%">
                    <span v-if="busy" class="spinner"></span>{{ busy ? '登录中' : '登录' }}
                </button>
            </form>
        </div>
    `,
};

const NAV = [
    { key: "overview", label: "总览", icon: "▤" },
    { key: "nodes", label: "节点管理", icon: "▦" },
    { key: "dns", label: "域名切换", icon: "⇄" },
    { key: "events", label: "事件日志", icon: "☰" },
    { key: "settings", label: "设置", icon: "⚙" },
];

const App = {
    components: {
        LoginView, OverviewView, NodesView, NodeDetailView,
        DNSView, EventsView, SettingsView, Toasts,
    },
    setup() {
        const route = ref(parseHash());
        const theme = ref(localStorage.getItem("vpspanel_theme") || "dark");

        function parseHash() {
            const h = (location.hash || "#/overview").slice(2);
            const [page, id] = h.split("/");
            return { page: page || "overview", id: id ? Number(id) : 0 };
        }

        function onHashChange() {
            route.value = parseHash();
        }

        onMounted(() => {
            window.addEventListener("hashchange", onHashChange);
            applyTheme();
            if (session.token) {
                // 校验一下 token 还有效，顺便拿用户名
                api("/api/me")
                    .then((me) => (session.username = me.username))
                    .catch(() => {});
            }
        });
        onBeforeUnmount(() => window.removeEventListener("hashchange", onHashChange));

        function go(page, id) {
            location.hash = id ? `#/${page}/${id}` : `#/${page}`;
        }

        function applyTheme() {
            document.documentElement.setAttribute("data-theme", theme.value);
            localStorage.setItem("vpspanel_theme", theme.value);
        }

        function toggleTheme() {
            theme.value = theme.value === "dark" ? "light" : "dark";
            applyTheme();
        }

        function logout() {
            setToken("");
            toast("已退出登录", "info");
        }

        // 节点详情页要通过 key 强制重建，否则从一个节点切到另一个时定时器不会重置
        const detailKey = computed(() => `node-${route.value.id}`);

        return { route, session, NAV, go, logout, theme, toggleTheme, toasts, detailKey };
    },
    template: `
        <template v-if="!session.token">
            <LoginView />
        </template>

        <div v-else class="app">
            <nav class="sidebar">
                <div class="brand">
                    VPS 流量面板
                    <small>{{ session.username || 'admin' }}</small>
                </div>
                <button v-for="n in NAV" :key="n.key" class="nav-item"
                        :class="{ active: route.page === n.key || (n.key === 'nodes' && route.page === 'node') }"
                        @click="go(n.key)">
                    <span aria-hidden="true">{{ n.icon }}</span>{{ n.label }}
                </button>
                <div class="nav-spacer"></div>
                <button class="nav-item" @click="toggleTheme">
                    <span aria-hidden="true">{{ theme === 'dark' ? '☀' : '☾' }}</span>
                    {{ theme === 'dark' ? '浅色模式' : '深色模式' }}
                </button>
                <button class="nav-item" @click="logout">
                    <span aria-hidden="true">→</span>退出登录
                </button>
            </nav>

            <main class="main">
                <OverviewView v-if="route.page === 'overview'" @open-node="go('node', $event)" />
                <NodesView v-else-if="route.page === 'nodes'" @open-node="go('node', $event)" />
                <NodeDetailView v-else-if="route.page === 'node'" :key="detailKey" :node-id="route.id"
                                @back="go('overview')" @edit="go('nodes')" />
                <DNSView v-else-if="route.page === 'dns'" />
                <EventsView v-else-if="route.page === 'events'" />
                <SettingsView v-else-if="route.page === 'settings'" />
                <div v-else class="empty">页面不存在</div>
            </main>
        </div>

        <Toasts :items="toasts" />
    `,
};

createApp(App).mount("#app");
