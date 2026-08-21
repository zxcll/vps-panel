// 公共 UI 组件：图表、配额条、统计磁贴、弹窗、提示。
// 图表是手写 SVG，不引图表库——只有两个系列、两种图形，自己画反而更可控。

import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { fmtBytes, fmtPercent, fmtTime, statusMeta } from "./api.js";

// --- 状态徽标 ---

export const StatusBadge = {
    props: { status: String },
    setup(props) {
        const meta = computed(() => statusMeta(props.status));
        return { meta };
    },
    template: `
        <span class="badge" :class="meta.cls">
            <span class="dot"></span>{{ meta.text }}
        </span>
    `,
};

// --- 统计磁贴 ---

export const StatTile = {
    // compact 用于值本身就很长的场合（比如「9.93 GiB / 4.97 GiB」），
    // 大字号会折行，读起来反而更差
    props: { label: String, value: [String, Number], sub: String, compact: Boolean },
    template: `
        <div class="stat">
            <div class="stat-label">{{ label }}</div>
            <div class="stat-value" :class="{ compact }">{{ value }}</div>
            <div v-if="sub" class="stat-sub">{{ sub }}</div>
        </div>
    `,
};

// --- 配额进度条 ---
//
// 进度条的颜色随用量变化，但百分比数字始终显示在旁边——
// 色觉障碍的用户不该只能靠颜色判断"快满了"。

export const QuotaMeter = {
    props: {
        used: { type: Number, default: 0 },
        quota: { type: Number, default: 0 },
        warnPercent: { type: Number, default: 90 },
        label: { type: String, default: "本周期已用" },
    },
    setup(props) {
        const unlimited = computed(() => !props.quota || props.quota <= 0);
        const percent = computed(() => {
            if (unlimited.value) return 0;
            return Math.min(100, (props.used / props.quota) * 100);
        });
        const rawPercent = computed(() => {
            if (unlimited.value) return 0;
            return (props.used / props.quota) * 100;
        });
        const cls = computed(() => {
            if (unlimited.value) return "";
            if (rawPercent.value >= 100) return "critical";
            if (rawPercent.value >= props.warnPercent) return "warning";
            return "";
        });
        return { unlimited, percent, rawPercent, cls, fmtBytes, fmtPercent };
    },
    template: `
        <div class="meter">
            <div class="meter-head">
                <span>{{ label }}</span>
                <span v-if="unlimited">{{ fmtBytes(used) }} · 不限量</span>
                <span v-else>{{ fmtBytes(used) }} / {{ fmtBytes(quota) }}（{{ fmtPercent(rawPercent) }}）</span>
            </div>
            <div class="meter-track">
                <div class="meter-fill" :class="cls"
                     :style="{ width: (unlimited ? 0 : percent) + '%' }"></div>
            </div>
        </div>
    `,
};

// --- 流量图表 ---
//
// 用并排柱状而不是折线/面积，有两个理由：
//   1. 数据本身是"每个时间桶用了多少"，是离散的量，不是连续信号。
//      柱子从基线起算，长度直接就是量，读起来没有歧义
//   2. 只有一个桶有数据时（刚装上探针的头一个小时），
//      折线退化成一个看不见的点，柱状则照常显示
//
// 两个系列并排而不是堆叠：计费口径可能是"出入取大"，
// 那种口径下堆起来的总高度是个没有意义的数字。

const SERIES = [
    { key: "rx_delta", name: "入站", cssVar: "--series-rx" },
    { key: "tx_delta", name: "出站", cssVar: "--series-tx" },
];

// 同一个时间桶里两根柱子之间留出的表面缝隙，让相邻色块不会粘成一片
const BAR_GAP = 2;

// barPath 画一个底部贴基线、顶部圆角的柱子。
// 柱子很矮时圆角自动收窄，免得变成一颗药丸。
function barPath(x, y, w, h, r) {
    if (h <= 0.5) return "";
    const rad = Math.min(r, w / 2, h);
    const bottom = y + h;
    return `M${x},${bottom}L${x},${y + rad}Q${x},${y} ${x + rad},${y}` +
        `L${x + w - rad},${y}Q${x + w},${y} ${x + w},${y + rad}L${x + w},${bottom}Z`;
}

export const TrafficChart = {
    props: {
        points: { type: Array, default: () => [] },
        bucket: { type: String, default: "hour" },
        height: { type: Number, default: 240 },
    },
    setup(props) {
        const wrap = ref(null);
        const width = ref(760);
        const hover = ref(-1);
        const mouse = ref({ x: 0, y: 0 });
        const showTable = ref(false);

        let ro = null;
        onMounted(() => {
            if (!wrap.value) return;
            // 用 ResizeObserver 而不是 viewBox 缩放：
            // 缩放会把线宽和字号一起拉伸，视觉上不干净
            ro = new ResizeObserver((entries) => {
                for (const e of entries) {
                    const w = Math.floor(e.contentRect.width);
                    if (w > 0) width.value = w;
                }
            });
            ro.observe(wrap.value);
        });
        onBeforeUnmount(() => ro && ro.disconnect());

        const pad = { top: 10, right: 10, bottom: 26, left: 62 };

        const inner = computed(() => ({
            w: Math.max(60, width.value - pad.left - pad.right),
            h: Math.max(60, props.height - pad.top - pad.bottom),
        }));

        const maxVal = computed(() => {
            let m = 0;
            for (const p of props.points) {
                m = Math.max(m, p.rx_delta || 0, p.tx_delta || 0);
            }
            return m;
        });

        // niceMax 把纵轴上界抬到一个整齐的刻度上，读数更舒服
        const niceMax = computed(() => {
            const m = maxVal.value;
            if (m <= 0) return 1024;
            const exp = Math.floor(Math.log2(m) / 10);
            const unit = Math.pow(1024, Math.max(0, exp));
            const scaled = m / unit;
            const steps = [1, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10, 12, 16, 20, 32, 64, 128, 256, 512, 1024];
            const pick = steps.find((s) => s >= scaled) || scaled;
            return pick * unit;
        });

        // 每个时间桶占一段水平区间，两根柱子并排放在里面
        const geom = computed(() => {
            const n = Math.max(1, props.points.length);
            const slot = inner.value.w / n;
            // 桶之间留白，柱子之间也留 2px 的表面缝隙
            const usable = Math.max(2, slot * 0.72);
            const barW = Math.max(1, Math.min(16, (usable - BAR_GAP) / 2));
            return { n, slot, barW };
        });

        // slotCenter 返回第 i 个桶的中心横坐标
        const slotCenter = (i) => pad.left + geom.value.slot * (i + 0.5);
        const yAt = (v) => pad.top + inner.value.h - (v / niceMax.value) * inner.value.h;

        const bars = computed(() => {
            const { barW } = geom.value;
            const baseline = pad.top + inner.value.h;
            const out = [];

            props.points.forEach((p, i) => {
                const center = slotCenter(i);
                SERIES.forEach((s, si) => {
                    const v = p[s.key] || 0;
                    const y = yAt(v);
                    // 左边一根，右边一根，中间隔 BAR_GAP
                    const x = center - barW - BAR_GAP / 2 + si * (barW + BAR_GAP);
                    out.push({
                        key: `${i}-${s.key}`,
                        cssVar: s.cssVar,
                        d: barPath(x, y, barW, baseline - y, 4),
                    });
                });
            });
            return out;
        });

        // 横向网格线：4 档，含 0 和上界
        const yTicks = computed(() => {
            const out = [];
            for (let i = 0; i <= 4; i++) {
                const v = (niceMax.value / 4) * i;
                out.push({ v, y: yAt(v), label: fmtBytes(v, v >= 1024 ? 1 : 0) });
            }
            return out;
        });

        // 横轴标签只标 6 个左右，标满会糊成一片
        const xTicks = computed(() => {
            const n = props.points.length;
            if (!n) return [];
            const want = Math.min(6, n);
            const step = Math.max(1, Math.ceil(n / want));
            const out = [];
            for (let i = 0; i < n; i += step) {
                out.push({ i, x: slotCenter(i), label: labelFor(props.points[i].hour_ts) });
            }
            return out;
        });

        function labelFor(ts) {
            const d = new Date(ts);
            if (isNaN(d)) return "";
            if (props.bucket === "day") {
                return d.toLocaleDateString("zh-CN", { month: "numeric", day: "numeric" });
            }
            return d.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit", hour12: false });
        }

        function onMove(e) {
            const rect = e.currentTarget.getBoundingClientRect();
            const x = e.clientX - rect.left;
            const n = props.points.length;
            if (!n) return;

            const idx = Math.floor((x - pad.left) / geom.value.slot);
            hover.value = Math.max(0, Math.min(n - 1, idx));
            mouse.value = { x, y: e.clientY - rect.top };
        }

        function onLeave() {
            hover.value = -1;
        }

        const hoverPoint = computed(() =>
            hover.value >= 0 ? props.points[hover.value] : null
        );

        // 提示框贴着鼠标，靠右时翻到左边，免得被容器裁掉
        const tooltipStyle = computed(() => {
            const flip = mouse.value.x > width.value - 170;
            return {
                left: (flip ? mouse.value.x - 160 : mouse.value.x + 14) + "px",
                top: Math.max(4, mouse.value.y - 40) + "px",
            };
        });

        const hasData = computed(() => props.points.length > 0 && maxVal.value > 0);

        return {
            wrap, width, pad, inner, bars, geom, yTicks, xTicks, hover, hoverPoint,
            tooltipStyle, onMove, onLeave, slotCenter, yAt, hasData, SERIES,
            showTable, fmtBytes, fmtTime, labelFor,
        };
    },
    template: `
        <div>
            <div class="card-title" style="margin-bottom:10px">
                <div class="legend">
                    <span v-for="s in SERIES" :key="s.key" class="legend-item">
                        <span class="legend-swatch" :style="{ background: 'var(' + s.cssVar + ')' }"></span>
                        {{ s.name }}
                    </span>
                </div>
                <button class="btn small" @click="showTable = !showTable">
                    {{ showTable ? '看图表' : '看数据表' }}
                </button>
            </div>

            <div v-if="!showTable" ref="wrap" class="chart-wrap">
                <div v-if="!hasData" class="chart-empty">这段时间还没有流量数据</div>

                <svg v-else :height="height" :width="width"
                     @mousemove="onMove" @mouseleave="onLeave"
                     role="img" aria-label="各时间段的入站与出站流量柱状图">

                    <!-- 网格线：细、淡，不抢数据的视觉重量 -->
                    <g>
                        <line v-for="t in yTicks" :key="'g'+t.v"
                              :x1="pad.left" :x2="pad.left + inner.w"
                              :y1="t.y" :y2="t.y"
                              stroke="var(--grid)" stroke-width="1" />
                        <text v-for="t in yTicks" :key="'l'+t.v"
                              :x="pad.left - 8" :y="t.y + 4"
                              text-anchor="end" font-size="11"
                              fill="var(--text-muted)"
                              style="font-variant-numeric:tabular-nums">{{ t.label }}</text>
                    </g>

                    <!-- 悬停高亮带垫在柱子下面，不遮挡数据 -->
                    <rect v-if="hover >= 0"
                          :x="slotCenter(hover) - geom.slot / 2" :y="pad.top"
                          :width="geom.slot" :height="inner.h"
                          fill="var(--text-muted)" opacity="0.08" />

                    <!-- 两个系列并排，各自从基线起算，不堆叠 -->
                    <g>
                        <path v-for="b in bars" :key="b.key" :d="b.d"
                              :fill="'var(' + b.cssVar + ')'" />
                    </g>

                    <!-- 基线 -->
                    <line :x1="pad.left" :x2="pad.left + inner.w"
                          :y1="pad.top + inner.h" :y2="pad.top + inner.h"
                          stroke="var(--axis)" stroke-width="1" />

                    <text v-for="t in xTicks" :key="'x'+t.i"
                          :x="t.x" :y="pad.top + inner.h + 17"
                          text-anchor="middle" font-size="11"
                          fill="var(--text-muted)">{{ t.label }}</text>

                </svg>

                <div v-if="hover >= 0 && hoverPoint" class="chart-tooltip" :style="tooltipStyle">
                    <div class="tt-time">{{ fmtTime(hoverPoint.hour_ts) }}</div>
                    <div v-for="s in SERIES" :key="'t'+s.key" class="tt-row">
                        <span>
                            <span class="legend-swatch"
                                  :style="{ background: 'var(' + s.cssVar + ')', display:'inline-block', marginRight:'5px' }"></span>{{ s.name }}
                        </span>
                        <span>{{ fmtBytes(hoverPoint[s.key] || 0) }}</span>
                    </div>
                </div>
            </div>

            <!-- 数据表视图：给读屏和需要精确数字的场景 -->
            <div v-else class="table-scroll" style="max-height:340px;overflow-y:auto">
                <table>
                    <thead>
                        <tr><th>时间</th><th class="num">入站</th><th class="num">出站</th></tr>
                    </thead>
                    <tbody>
                        <tr v-for="p in points.slice().reverse()" :key="p.hour_ts">
                            <td>{{ fmtTime(p.hour_ts) }}</td>
                            <td class="num">{{ fmtBytes(p.rx_delta) }}</td>
                            <td class="num">{{ fmtBytes(p.tx_delta) }}</td>
                        </tr>
                        <tr v-if="!points.length"><td colspan="3" class="muted">暂无数据</td></tr>
                    </tbody>
                </table>
            </div>
        </div>
    `,
};

// --- 弹窗 ---

export const Modal = {
    props: { title: String, wide: Boolean },
    emits: ["close"],
    setup(_, { emit }) {
        function onKey(e) {
            if (e.key === "Escape") emit("close");
        }
        onMounted(() => window.addEventListener("keydown", onKey));
        onBeforeUnmount(() => window.removeEventListener("keydown", onKey));
        return {};
    },
    template: `
        <div class="modal-mask" @click.self="$emit('close')">
            <div class="modal" :style="wide ? 'max-width:840px' : ''">
                <h2>{{ title }}</h2>
                <slot></slot>
            </div>
        </div>
    `,
};

// --- 提示条 ---

export const Toasts = {
    props: { items: Array },
    template: `
        <div class="toast-wrap">
            <div v-for="t in items" :key="t.id" class="toast" :class="t.kind">{{ t.message }}</div>
        </div>
    `,
};

// --- 确认对话框 ---

export const ConfirmDialog = {
    props: { title: String, message: String, confirmText: { type: String, default: "确认" }, danger: Boolean },
    emits: ["confirm", "cancel"],
    components: { Modal },
    template: `
        <Modal :title="title" @close="$emit('cancel')">
            <p style="white-space:pre-wrap;line-height:1.6">{{ message }}</p>
            <div class="modal-actions">
                <button class="btn" @click="$emit('cancel')">取消</button>
                <button class="btn" :class="danger ? 'danger' : 'primary'" @click="$emit('confirm')">
                    {{ confirmText }}
                </button>
            </div>
        </Modal>
    `,
};
