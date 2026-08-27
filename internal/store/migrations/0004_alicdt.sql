-- 阿里云 CDT 管理。
--
-- 这一套表和节点账本（node_counters / node_usage / traffic_hourly）**完全隔离**，
-- 一个字节都不互通。理由和转发账本那条铁律是同一个：
-- 阿里云 CDT 的计量口径（账号级、只算出方向、按业务地域分池）和探针的网卡口径
-- （单机、进出各记一遍）根本不是一回事，混在一起只会把两边都算错。
--
-- 迁移全部写成幂等的（IF NOT EXISTS），满足本项目「每次启动全量重放」的约定。

-- ---------------------------------------------------------------------------
-- 账号
--
-- 一个账号 = 一组 AccessKey + 一个地域。CDT 额度本身是账号级的，
-- 地域只影响 ECS 实例列表要去哪个域名拉。
CREATE TABLE IF NOT EXISTS cdt_accounts (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    name                  TEXT    NOT NULL,
    access_key_id         TEXT    NOT NULL DEFAULT '',
    -- AccessKeySecret 用 AES-256-GCM 加密后存，和 SSH 凭据、DNS 凭据同一套密钥。
    cred_enc              BLOB,
    region_id             TEXT    NOT NULL DEFAULT 'cn-hongkong',
    -- china | international。只影响账单接口的域名和记账货币。
    site_type             TEXT    NOT NULL DEFAULT 'international',

    -- 两个免费额度池，单位 GB。阿里云的规则就是分开算的：
    -- 中国内地 20GB/月、非中国内地 200GB/月，不是一个 220GB 的总额度。
    quota_mainland_gb     REAL    NOT NULL DEFAULT 20,
    quota_overseas_gb     REAL    NOT NULL DEFAULT 200,
    -- 用到额度的百分之多少就熔断停机。
    threshold_percent     REAL    NOT NULL DEFAULT 95,
    -- 待还金额超过这个数也熔断。0 = 不启用这条。
    outstanding_threshold REAL    NOT NULL DEFAULT 0,
    -- StopCharging（节省模式，停机不计费）| KeepCharging
    shutdown_mode         TEXT    NOT NULL DEFAULT 'StopCharging',

    -- 抢占式实例被回收后自动拉起。
    keep_alive            INTEGER NOT NULL DEFAULT 0,
    -- 定时开关机，"HH:MM" 格式，空串表示不启用。
    auto_start_time       TEXT    NOT NULL DEFAULT '',
    auto_stop_time        TEXT    NOT NULL DEFAULT '',
    schedule_tz           TEXT    NOT NULL DEFAULT 'Asia/Shanghai',

    -- tripped_at 非 0 表示这个账号已经被面板熔断停机了。
    -- 它的作用和 nodes.exceed_handled_at 一样：停机是不可逆动作，
    -- 先落标记再执行，宁可漏执行也不能重复执行。
    tripped_at            INTEGER NOT NULL DEFAULT 0,
    tripped_reason        TEXT    NOT NULL DEFAULT '',
    -- 记下是哪个账期熔断的，账期一翻页就自动解除。
    tripped_cycle         TEXT    NOT NULL DEFAULT '',
    -- 抢占式实例售罄只在第一次告警，避免保活每轮都刷一条。
    nostock_notified      INTEGER NOT NULL DEFAULT 0,

    -- 多久去阿里云查一次（秒）。流量、账单、实例状态、抢占式保活都按它走。
    -- 老库里没有这一列，由 store.addMissingColumns 补上 —— SQLite 的
    -- ADD COLUMN 没有 IF NOT EXISTS，写进迁移文件会让第二次启动直接失败。
    sync_interval_sec     INTEGER NOT NULL DEFAULT 300,

    last_sync_at          INTEGER NOT NULL DEFAULT 0,
    last_error            TEXT    NOT NULL DEFAULT '',
    enabled               INTEGER NOT NULL DEFAULT 1,
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL
);

-- ---------------------------------------------------------------------------
-- 实例快照
--
-- guarded 标记这台实例受不受熔断/保活/定时管辖。参考项目是每个账号只能绑
-- 一台实例，这里改成打标记，一个账号可以守好几台。
CREATE TABLE IF NOT EXISTS cdt_instances (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id     INTEGER NOT NULL REFERENCES cdt_accounts(id) ON DELETE CASCADE,
    instance_id    TEXT    NOT NULL,
    instance_name  TEXT    NOT NULL DEFAULT '',
    region_id      TEXT    NOT NULL DEFAULT '',
    zone_id        TEXT    NOT NULL DEFAULT '',
    status         TEXT    NOT NULL DEFAULT 'Unknown',
    public_ip      TEXT    NOT NULL DEFAULT '',
    instance_type  TEXT    NOT NULL DEFAULT '',
    bandwidth_mbps INTEGER NOT NULL DEFAULT 0,
    is_spot        INTEGER NOT NULL DEFAULT 0,
    guarded        INTEGER NOT NULL DEFAULT 0,
    -- 这台实例是不是面板主动停的（定时关机/熔断/手动）。
    -- 保活靠它区分「被阿里云回收了」和「我们自己停的」，否则两个功能会打架。
    planned_stop   INTEGER NOT NULL DEFAULT 0,
    last_synced    INTEGER NOT NULL DEFAULT 0,
    updated_at     INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_cdt_instances_uniq
    ON cdt_instances(account_id, instance_id);
CREATE INDEX IF NOT EXISTS idx_cdt_instances_acct ON cdt_instances(account_id);

-- ---------------------------------------------------------------------------
-- 流量快照
--
-- 存的是**快照**而不是增量：ListCdtInternetTraffic 返回的本来就是
-- 「本自然月至今的累计值」，账号级、由阿里云自己算。面板这边不需要做差值
-- 累加，也就不存在探针那套 boot_id / epoch 的重启安全问题。
--
-- 按 (账号, 账期, 业务地域) 存，好处是账期翻页时旧数据自然留着，
-- 能直接看上个月用了多少。
CREATE TABLE IF NOT EXISTS cdt_traffic (
    account_id         INTEGER NOT NULL REFERENCES cdt_accounts(id) ON DELETE CASCADE,
    cycle              TEXT    NOT NULL,
    business_region_id TEXT    NOT NULL,
    traffic_type       TEXT    NOT NULL DEFAULT '',
    traffic_bytes      INTEGER NOT NULL DEFAULT 0,
    updated_at         INTEGER NOT NULL,
    PRIMARY KEY (account_id, cycle, business_region_id)
);

-- ---------------------------------------------------------------------------
-- 余额与账单快照
CREATE TABLE IF NOT EXISTS cdt_bill (
    account_id       INTEGER NOT NULL REFERENCES cdt_accounts(id) ON DELETE CASCADE,
    cycle            TEXT    NOT NULL,
    available_amount REAL    NOT NULL DEFAULT 0,
    outstanding      REAL    NOT NULL DEFAULT 0,
    currency         TEXT    NOT NULL DEFAULT '',
    symbol           TEXT    NOT NULL DEFAULT '',
    updated_at       INTEGER NOT NULL,
    PRIMARY KEY (account_id, cycle)
);
