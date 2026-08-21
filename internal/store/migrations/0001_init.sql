-- 所有时间列统一存 Unix 秒（INTEGER, UTC），避免时区/字符串格式歧义。
-- 时区只在两个地方出现：节点的 reset_tz（决定账单周期边界）和前端展示。

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS nodes (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    name                  TEXT    NOT NULL,
    remark                TEXT    NOT NULL DEFAULT '',
    secret                TEXT    NOT NULL UNIQUE,      -- 探针鉴权 token
    ipv4                  TEXT    NOT NULL DEFAULT '',
    ipv6                  TEXT    NOT NULL DEFAULT '',

    -- 流量配额
    quota_bytes           INTEGER NOT NULL DEFAULT 0,   -- 0 = 不限量
    billing_mode          TEXT    NOT NULL DEFAULT 'sum', -- sum|max|out|in
    traffic_ratio         REAL    NOT NULL DEFAULT 1.0,   -- 计费倍率，用于校准商家口径
    warn_percent          INTEGER NOT NULL DEFAULT 90,

    -- 账单周期
    reset_day             INTEGER NOT NULL DEFAULT 1,   -- 1-31，遇小月自动落到月末
    reset_tz              TEXT    NOT NULL DEFAULT 'Asia/Shanghai',
    cycle_start           INTEGER NOT NULL DEFAULT 0,
    cycle_end             INTEGER NOT NULL DEFAULT 0,

    -- 超额动作
    action_on_exceed      TEXT    NOT NULL DEFAULT 'none', -- none|shutdown_agent|shutdown_ssh|command|dns_only
    exceed_command        TEXT    NOT NULL DEFAULT '',
    auto_recover_on_reset INTEGER NOT NULL DEFAULT 1,

    -- SSH（用于探针已死时仍能远程关机）
    ssh_host              TEXT    NOT NULL DEFAULT '',
    ssh_port              INTEGER NOT NULL DEFAULT 22,
    ssh_user              TEXT    NOT NULL DEFAULT 'root',
    ssh_auth              TEXT    NOT NULL DEFAULT 'password', -- password|key
    ssh_secret_enc        BLOB,                                -- 密码或私钥 PEM，AES-GCM
    ssh_key_pass_enc      BLOB,                                -- 私钥 passphrase
    ssh_host_key          TEXT    NOT NULL DEFAULT '',         -- TOFU 记录的主机公钥
    ssh_use_sudo          INTEGER NOT NULL DEFAULT 0,

    -- 健康探测
    probe_port            INTEGER NOT NULL DEFAULT 0,   -- 0 表示复用 ssh_port

    -- 状态
    status                TEXT    NOT NULL DEFAULT 'unknown', -- unknown|online|offline|exceeded|stopped
    last_seen             INTEGER,
    enabled               INTEGER NOT NULL DEFAULT 1,
    exceed_handled_at     INTEGER,                      -- 超额动作去重，防重复关机
    warn_handled_at       INTEGER,                      -- 预警去重
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL
);

-- 探针上报的网卡累计计数器快照。boot_id 变化即代表机器重启过。
CREATE TABLE IF NOT EXISTS node_counters (
    node_id    INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    boot_id    TEXT    NOT NULL DEFAULT '',
    iface      TEXT    NOT NULL DEFAULT '',
    last_rx    INTEGER NOT NULL DEFAULT 0,
    last_tx    INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL DEFAULT 0
);

-- 当前账单周期的累计用量。这是"账本"，只由面板写。
CREATE TABLE IF NOT EXISTS node_usage (
    node_id     INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    cycle_start INTEGER NOT NULL DEFAULT 0,
    rx_bytes    INTEGER NOT NULL DEFAULT 0,
    tx_bytes    INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL DEFAULT 0
);

-- 按小时聚合的增量，用于画曲线和跨周期回溯。
CREATE TABLE IF NOT EXISTS traffic_hourly (
    node_id  INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    hour_ts  INTEGER NOT NULL,           -- 该小时的起始 Unix 秒
    rx_delta INTEGER NOT NULL DEFAULT 0,
    tx_delta INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (node_id, hour_ts)
);
CREATE INDEX IF NOT EXISTS idx_traffic_hourly_ts ON traffic_hourly(hour_ts);

-- 历史账单周期归档。
CREATE TABLE IF NOT EXISTS cycle_history (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id      INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    cycle_start  INTEGER NOT NULL,
    cycle_end    INTEGER NOT NULL,
    rx_bytes     INTEGER NOT NULL,
    tx_bytes     INTEGER NOT NULL,
    billed_bytes INTEGER NOT NULL,
    quota_bytes  INTEGER NOT NULL,
    billing_mode TEXT    NOT NULL DEFAULT 'sum',
    archived_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cycle_history_node ON cycle_history(node_id, cycle_start DESC);

CREATE TABLE IF NOT EXISTS dns_providers (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL,
    type       TEXT NOT NULL,           -- cloudflare|dnspod|alidns
    cred_enc   BLOB,                    -- JSON 凭据，AES-GCM
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS dns_records (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_id       INTEGER NOT NULL REFERENCES dns_providers(id) ON DELETE CASCADE,
    zone              TEXT    NOT NULL,              -- 主域名，如 example.com
    name              TEXT    NOT NULL,              -- 完整记录名，如 us.example.com
    record_type       TEXT    NOT NULL DEFAULT 'A',  -- A|AAAA
    ttl               INTEGER NOT NULL DEFAULT 60,
    proxied           INTEGER NOT NULL DEFAULT 0,    -- 仅 Cloudflare 有意义
    strategy          TEXT    NOT NULL DEFAULT 'failover', -- failover|manual
    switch_on_exceed  INTEGER NOT NULL DEFAULT 1,
    switch_on_offline INTEGER NOT NULL DEFAULT 1,
    switch_on_warn    INTEGER NOT NULL DEFAULT 0,
    current_node_id   INTEGER REFERENCES nodes(id) ON DELETE SET NULL,
    current_value     TEXT    NOT NULL DEFAULT '',
    last_switch_at    INTEGER,
    last_error        TEXT    NOT NULL DEFAULT '',
    enabled           INTEGER NOT NULL DEFAULT 1,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS dns_record_members (
    record_id INTEGER NOT NULL REFERENCES dns_records(id) ON DELETE CASCADE,
    node_id   INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    priority  INTEGER NOT NULL DEFAULT 100,
    PRIMARY KEY (record_id, node_id)
);

CREATE TABLE IF NOT EXISTS events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id    INTEGER REFERENCES nodes(id) ON DELETE SET NULL,
    type       TEXT NOT NULL,
    level      TEXT NOT NULL DEFAULT 'info',  -- info|warn|error
    message    TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_created ON events(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_node ON events(node_id, created_at DESC);
