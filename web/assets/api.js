// 与面板后端通信的薄封装 + 一些公共的格式化函数。

import { reactive } from "vue";

const TOKEN_KEY = "vpspanel_token";

export const session = reactive({
    token: localStorage.getItem(TOKEN_KEY) || "",
    username: "",
});

export function setToken(token, username) {
    session.token = token || "";
    session.username = username || "";
    if (token) {
        localStorage.setItem(TOKEN_KEY, token);
    } else {
        localStorage.removeItem(TOKEN_KEY);
    }
}

// toasts 是全局的提示队列，任何地方都能往里塞消息。
export const toasts = reactive([]);

let toastSeq = 0;

export function toast(message, kind = "info", ttl = 4200) {
    const id = ++toastSeq;
    toasts.push({ id, message, kind });
    setTimeout(() => {
        const i = toasts.findIndex((t) => t.id === id);
        if (i >= 0) toasts.splice(i, 1);
    }, ttl);
}

export class ApiError extends Error {
    constructor(message, status) {
        super(message);
        this.status = status;
    }
}

export async function api(path, options = {}) {
    const headers = { ...(options.headers || {}) };
    if (session.token) headers["Authorization"] = "Bearer " + session.token;
    if (options.body !== undefined) headers["Content-Type"] = "application/json";

    let resp;
    try {
        resp = await fetch(path, {
            ...options,
            headers,
            body: options.body === undefined ? undefined : JSON.stringify(options.body),
        });
    } catch (e) {
        throw new ApiError("无法连接面板服务：" + e.message, 0);
    }

    if (resp.status === 401) {
        // 登录态失效：清掉 token，路由会自动回到登录页
        setToken("");
        throw new ApiError("登录已过期，请重新登录", 401);
    }

    const text = await resp.text();
    let data = null;
    if (text) {
        try {
            data = JSON.parse(text);
        } catch {
            data = { error: text };
        }
    }

    if (!resp.ok) {
        throw new ApiError((data && data.error) || `请求失败（HTTP ${resp.status}）`, resp.status);
    }
    return data;
}

// --- 格式化 ---

const UNITS = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];

// fmtBytes 用 1024 进制，和 VPS 商家标流量的习惯一致。
export function fmtBytes(bytes, digits = 2) {
    const n = Number(bytes) || 0;
    if (n <= 0) return "0 B";
    let v = n;
    let i = 0;
    while (v >= 1024 && i < UNITS.length - 1) {
        v /= 1024;
        i++;
    }
    return `${v.toFixed(i === 0 ? 0 : digits)} ${UNITS[i]}`;
}

export function fmtRate(bytesPerSec) {
    return fmtBytes(bytesPerSec, 1) + "/s";
}

export function fmtPercent(v, digits = 1) {
    return (Number(v) || 0).toFixed(digits) + "%";
}

// isZeroTime 判断是不是 Go 的零值时间（后端把未设置的时间序列化成 0001 年）。
export function isZeroTime(s) {
    return !s || s.startsWith("0001-01-01");
}

export function fmtTime(s, withSeconds = false) {
    if (isZeroTime(s)) return "—";
    const d = new Date(s);
    if (isNaN(d)) return "—";
    const opts = {
        year: "numeric", month: "2-digit", day: "2-digit",
        hour: "2-digit", minute: "2-digit",
        hour12: false,
    };
    if (withSeconds) opts.second = "2-digit";
    return d.toLocaleString("zh-CN", opts);
}

export function fmtDate(s) {
    if (isZeroTime(s)) return "—";
    const d = new Date(s);
    if (isNaN(d)) return "—";
    return d.toLocaleDateString("zh-CN", { month: "2-digit", day: "2-digit" });
}

// fmtAgo 把时间转成"多久以前"，比绝对时间更适合看心跳。
export function fmtAgo(s) {
    if (isZeroTime(s)) return "从未";
    const d = new Date(s);
    if (isNaN(d)) return "从未";
    const sec = Math.floor((Date.now() - d.getTime()) / 1000);
    if (sec < 0) return "刚刚";
    if (sec < 60) return `${sec} 秒前`;
    if (sec < 3600) return `${Math.floor(sec / 60)} 分钟前`;
    if (sec < 86400) return `${Math.floor(sec / 3600)} 小时前`;
    return `${Math.floor(sec / 86400)} 天前`;
}

export function fmtUptime(sec) {
    const n = Number(sec) || 0;
    if (n <= 0) return "—";
    const d = Math.floor(n / 86400);
    const h = Math.floor((n % 86400) / 3600);
    const m = Math.floor((n % 3600) / 60);
    if (d > 0) return `${d} 天 ${h} 小时`;
    if (h > 0) return `${h} 小时 ${m} 分`;
    return `${m} 分钟`;
}

// parseBytes 把 "500GB" / "1.5 TiB" / "1024" 这类输入解析成字节数。
// 返回 null 表示解析不了。
export function parseBytes(input) {
    if (input === null || input === undefined) return null;
    const s = String(input).trim();
    if (s === "") return 0;

    const m = s.match(/^([\d.]+)\s*([a-zA-Z]*)$/);
    if (!m) return null;
    const num = parseFloat(m[1]);
    if (isNaN(num)) return null;

    const unit = m[2].toUpperCase().replace("I", "");
    const mult = { "": 1, B: 1, K: 1024, KB: 1024, M: 1024 ** 2, MB: 1024 ** 2,
        G: 1024 ** 3, GB: 1024 ** 3, T: 1024 ** 4, TB: 1024 ** 4,
        P: 1024 ** 5, PB: 1024 ** 5 }[unit];
    if (mult === undefined) return null;
    return Math.round(num * mult);
}

// --- 常量表（和后端的枚举保持一致）---

export const BILLING_MODES = [
    { value: "sum", label: "双向计费（出 + 入）", hint: "最常见。上传和下载加起来算" },
    { value: "max", label: "单向计费（出/入取大）", hint: "只按进出中较大的那个方向计费，另一方向免费" },
    { value: "out", label: "仅出站", hint: "只统计服务器发出的流量" },
    { value: "in", label: "仅入站", hint: "只统计服务器接收的流量" },
];

export const EXCEED_ACTIONS = [
    { value: "none", label: "不处理", hint: "只记录，什么都不做" },
    { value: "dns_only", label: "仅切换解析 + 告警", hint: "把域名切到备用节点，机器保持运行" },
    { value: "shutdown_agent", label: "探针本地关机", hint: "由探针执行关机。探针进程挂了就失效" },
    { value: "shutdown_ssh", label: "SSH 远程关机", hint: "面板直连 SSH 关机。探针死了也能生效，最可靠" },
    { value: "command", label: "执行自定义命令", hint: "如 systemctl stop xray，停服务但保留 SSH" },
];

export const FORWARD_PROTOCOLS = [
    { value: "tcp", label: "TCP", hint: "最常见。HTTP、SSH、大部分代理协议都是 TCP" },
    { value: "udp", label: "UDP", hint: "DNS、QUIC、部分游戏和代理协议" },
    { value: "tcp+udp", label: "TCP + UDP", hint: "同一个端口两种协议都转" },
];

export const FORWARD_MODES = [
    { value: "kernel", label: "内核态（零拷贝）", hint: "nftables DNAT，开销最低，TCP/UDP 都支持。默认用它" },
    { value: "userspace", label: "用户态（分段 TCP）", hint: "每跳单独建连接，避免多跳串联时 TCP 拥塞叠加，还能省掉一次握手往返。只支持 TCP" },
];

export const PROVIDER_TYPES = [
    { value: "cloudflare", label: "Cloudflare" },
    { value: "dnspod", label: "腾讯云 DNSPod" },
    { value: "alidns", label: "阿里云 DNS" },
];

// --- 阿里云 CDT ---

export const CDT_SITES = [
    { value: "auto", label: "自动识别（推荐）" },
    { value: "international", label: "国际站（USD）" },
    { value: "china", label: "中国站（CNY）" },
];

export const CDT_SHUTDOWN_MODES = [
    { value: "StopCharging", label: "节省模式（停机不计费）",
      hint: "按量付费的 VPC 实例停机后不再收实例费，这是熔断想要的效果。代价是公网 IP 可能会变" },
    { value: "KeepCharging", label: "普通停机（继续计费）",
      hint: "保留资源和公网 IP，但实例费照收。只在需要固定 IP 时选它" },
];

// 阿里云常用地域。CDT 流量是账号级的，这个只决定去哪个地域拉 ECS 实例列表。
export const CDT_REGIONS = [
    { value: "cn-hongkong", label: "中国香港（cn-hongkong）" },
    { value: "ap-southeast-1", label: "新加坡（ap-southeast-1）" },
    { value: "ap-southeast-5", label: "印尼雅加达（ap-southeast-5）" },
    { value: "ap-northeast-1", label: "日本东京（ap-northeast-1）" },
    { value: "ap-northeast-2", label: "韩国首尔（ap-northeast-2）" },
    { value: "us-west-1", label: "美国硅谷（us-west-1）" },
    { value: "us-east-1", label: "美国弗吉尼亚（us-east-1）" },
    { value: "eu-central-1", label: "德国法兰克福（eu-central-1）" },
    { value: "eu-west-1", label: "英国伦敦（eu-west-1）" },
    { value: "me-east-1", label: "阿联酋迪拜（me-east-1）" },
    { value: "cn-hangzhou", label: "华东1 杭州（cn-hangzhou）" },
    { value: "cn-shanghai", label: "华东2 上海（cn-shanghai）" },
    { value: "cn-beijing", label: "华北2 北京（cn-beijing）" },
    { value: "cn-zhangjiakou", label: "华北3 张家口（cn-zhangjiakou）" },
    { value: "cn-shenzhen", label: "华南1 深圳（cn-shenzhen）" },
    { value: "cn-guangzhou", label: "华南3 广州（cn-guangzhou）" },
    { value: "cn-chengdu", label: "西南1 成都（cn-chengdu）" },
];

export const COMMON_TIMEZONES = [
    "Asia/Shanghai", "Asia/Hong_Kong", "Asia/Tokyo", "Asia/Singapore",
    "Asia/Seoul", "Asia/Kolkata", "UTC", "Europe/London", "Europe/Berlin",
    "Europe/Moscow", "America/New_York", "America/Chicago", "America/Los_Angeles",
    "America/Sao_Paulo", "Australia/Sydney",
];

// statusMeta 把节点状态映射成徽标样式和中文。
// 颜色之外一定配文字，不靠颜色单独表意。
export function statusMeta(status) {
    switch (status) {
        case "online": return { cls: "good", text: "在线" };
        case "offline": return { cls: "critical", text: "离线" };
        case "exceeded": return { cls: "serious", text: "流量超额" };
        case "stopped": return { cls: "muted", text: "已关停" };
        // unknown 表示探针从未上报过。说"未知"会让人以为是面板出问题了，
        // 说"待接入"才对得上真实情况：节点建好了，探针还没装上或还没连上。
        default: return { cls: "muted", text: "待接入" };
    }
}

export function levelMeta(level) {
    switch (level) {
        case "error": return { cls: "critical", text: "错误" };
        case "warn": return { cls: "warning", text: "警告" };
        default: return { cls: "muted", text: "信息" };
    }
}
