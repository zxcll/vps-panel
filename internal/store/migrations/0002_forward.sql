-- 端口转发。
--
-- 迁移是每次进程启动全量重放的（没有版本表），所以这里只允许出现幂等语句：
-- CREATE TABLE / INDEX IF NOT EXISTS。特别注意 SQLite 的
-- ALTER TABLE ... ADD COLUMN 不幂等，第二次启动就会以 duplicate column name
-- 让面板起不来 —— 所以本文件只新增表，一列都不往老表上加。

-- 转发相关的节点级配置。和 nodes 表分开，除了上面说的迁移限制，
-- 也因为「中转入口地址」和「探测地址」未必是同一个（比如探测走内网、中转走公网）。
CREATE TABLE IF NOT EXISTS forward_nodes (
    node_id       INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    -- 其他节点访问本节点用的地址。留空则回落 nodes.ipv4，再回落 nodes.ssh_host。
    relay_host    TEXT    NOT NULL DEFAULT '',
    relay_host_v6 TEXT    NOT NULL DEFAULT '',
    -- 中间跳自动分配监听端口的范围。首跳端口由用户自己指定，不从这里取。
    port_start    INTEGER NOT NULL DEFAULT 20000,
    port_end      INTEGER NOT NULL DEFAULT 29999,
    enabled       INTEGER NOT NULL DEFAULT 1,
    updated_at    INTEGER NOT NULL DEFAULT 0
);

-- 一条逻辑转发规则：从入口一路转到最终目标。
-- 中间经过哪些机器由 forward_hops 描述。
CREATE TABLE IF NOT EXISTS forward_rules (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL,
    proto      TEXT    NOT NULL DEFAULT 'tcp',   -- tcp|udp|tcp+udp
    dest_host  TEXT    NOT NULL DEFAULT '',      -- 最终目标，可以是 IP 也可以是域名
    dest_port  INTEGER NOT NULL,
    enabled    INTEGER NOT NULL DEFAULT 1,
    remark     TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- 规则在某台机器上的一跳。position 从 0 开始，0 就是入口。
--
-- proto 是从 forward_rules 冗余下来的，只为了能建下面那条唯一索引：
-- tcp/8443 和 udp/8443 是两个不同的坑位，不带 proto 的唯一约束会误伤。
-- 跳总是随规则整组重写，所以这份冗余不会漂移。
CREATE TABLE IF NOT EXISTS forward_hops (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    rule_id        INTEGER NOT NULL REFERENCES forward_rules(id) ON DELETE CASCADE,
    position       INTEGER NOT NULL,
    node_id        INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    listen_port    INTEGER NOT NULL,
    proto          TEXT    NOT NULL DEFAULT 'tcp',
    mode           TEXT    NOT NULL DEFAULT 'kernel',  -- kernel|userspace
    bandwidth_mbps INTEGER NOT NULL DEFAULT 0          -- 0 = 不限速
);
-- 这里**故意不建** (rule_id, position) 的唯一索引 —— 它在 0003 里被
-- (rule_id, position, node_id) 取代了（入口允许多台机器共用 position 0）。
--
-- 别把它加回来。迁移是每次启动全量重放的：0002 建、0003 删，第一次升级时
-- 因为索引本来就在，IF NOT EXISTS 是空操作，看着一切正常；等用户建了多入口
-- 规则、面板再重启一次，0002 这次会真的去建索引，然后被已有数据打回来，
-- **面板直接起不来**。这个坑踩过一次了。
--
-- 结论：在"全量重放"这个模型下，迁移文件描述的是**当前想要的 schema**，
-- 后面的迁移放宽了约束，前面的迁移就必须跟着改，不能只靠新文件去 DROP。
CREATE UNIQUE INDEX IF NOT EXISTS idx_forward_hops_port ON forward_hops(node_id, listen_port, proto);
CREATE INDEX IF NOT EXISTS        idx_forward_hops_node ON forward_hops(node_id);

-- ---------------------------------------------------------------------------
-- 转发流量账本
--
-- 这三张表和节点账本（node_counters / node_usage / traffic_hourly）是同构的，
-- 但**完全独立、不参与配额判定**。
--
-- 原因：节点配额只认网卡累计计数器，而中转流量在网卡上本来就进出各走一遍，
-- 已经计进去了。把这里的数字再加进节点账本就是重复计费，会误触发超额关机。
-- 这里的数据只回答「哪条规则用了多少」，用于分账展示。
-- 换算成节点计费口径的公式见 internal/quota.ForwardShare。
-- ---------------------------------------------------------------------------

-- 探针上报的每跳累计计数快照。epoch 变化即代表探针侧计数器已归零，
-- 作用等同于 node_counters 里的 boot_id。
CREATE TABLE IF NOT EXISTS forward_counters (
    hop_id     INTEGER PRIMARY KEY REFERENCES forward_hops(id) ON DELETE CASCADE,
    epoch      TEXT    NOT NULL DEFAULT '',
    last_up    INTEGER NOT NULL DEFAULT 0,
    last_down  INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL DEFAULT 0
);

-- 当前账单周期内每跳的累计用量。周期边界跟随该跳所在节点的账单周期。
CREATE TABLE IF NOT EXISTS forward_usage (
    hop_id      INTEGER PRIMARY KEY REFERENCES forward_hops(id) ON DELETE CASCADE,
    cycle_start INTEGER NOT NULL DEFAULT 0,
    up_bytes    INTEGER NOT NULL DEFAULT 0,
    down_bytes  INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL DEFAULT 0
);

-- 按小时聚合的每跳增量，用于画转发流量曲线。
CREATE TABLE IF NOT EXISTS forward_hourly (
    hop_id     INTEGER NOT NULL REFERENCES forward_hops(id) ON DELETE CASCADE,
    hour_ts    INTEGER NOT NULL,
    up_delta   INTEGER NOT NULL DEFAULT 0,
    down_delta INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (hop_id, hour_ts)
);
CREATE INDEX IF NOT EXISTS idx_forward_hourly_ts ON forward_hourly(hour_ts);
