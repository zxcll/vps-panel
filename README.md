# VPS 流量面板

监控多台 VPS 的存活与流量，流量跑满自动关机，节点掉线或流量耗尽时自动把域名解析切到备用节点。

面板和探针都是单个静态二进制，探针丢到任意 Linux 机器上就能跑，不依赖任何运行时。

```
┌──────────────┐   WebSocket 上报计数器 / 下发指令   ┌─────────────┐
│              │◄──────────────────────────────────►│  节点探针    │
│  面板         │                                    │  vps-agent   │
│  vps-panel   │   SSH 远程关机（探针死了也能用）      │             │
│              │───────────────────────────────────►│  你的 VPS    │
│  SQLite 账本  │                                    └─────────────┘
│  + 内嵌前端   │   改解析记录
│              │───────────────► Cloudflare / DNSPod / 阿里云 DNS
└──────────────┘
```

## 它解决什么问题

手上有几台 VPS，每台有月流量配额。跑超了轻则扣费，重则被商家停机。
不同商家的计费口径还不一样：有的按进出流量相加，有的只按进出中较大的那个方向算（所谓"单向流量"）。
账单日也各不相同，有的是 1 号，有的是 8 号，还得按机房所在时区算。

同时，主节点一旦跑满或者掉线，指向它的域名就得赶紧切到备用机器上。

这个面板把这几件事做成自动的。

## 核心设计：流量账本存在面板端

**节点机器重启后 `/proc/net/dev` 的计数器会归零。** 如果让节点自己记账，重启就丢数据。

所以这里的分工是：

- **探针只上报网卡累计计数器的原始值**，不做任何记账、不判断配额、不做决策
- **面板负责做差值累加**，靠 `boot_id`（`/proc/sys/kernel/random/boot_id`，每次开机必变）识别机器重启

机器重启时 `boot_id` 变了，面板知道计数器已归零，于是把当前值整个计入本周期。
最坏情况只丢失"最后一次上报到断电"之间的流量（默认上报间隔 30 秒）；
而且探针收到 `SIGTERM` 时会补报一次，正常重启连这一点都不丢。

这条保证有专门的自动化测试守着，见 [验证](#验证)。

## 功能

**流量统计**
- 四种计费口径：双向相加 / 单向取大 / 仅出站 / 仅入站
- 计费倍率，用来校准网卡统计和商家账单之间那几个百分点的差异
- 每个节点独立设清零日（1–31 号）和时区，31 号遇小月自动落到月末
- 小时级流量曲线 + 历史账单周期归档

**流量耗尽后的处置**（每个节点单独配，默认什么都不做）
- SSH 远程关机 —— 面板直连节点 SSH，探针进程死了也照样能关
- 探针本地关机
- 执行自定义命令，比如 `systemctl stop xray`，停服务但保留 SSH
- 只切解析 + 告警，不动机器

**域名自动切换**
- 支持 Cloudflare、腾讯云 DNSPod、阿里云 DNS
- 按优先级选主，触发条件可分别开关：流量耗尽 / 节点离线 / 到达预警线
- 防误切：探针失联后再做一次 TCP 拨测确认，连续失败 N 次才判定下线，两次切换之间有冷却期
- 「切换演练」按钮：只计算不改动，告诉你此刻会切到哪台、每个候选为什么被淘汰

**其他**
- Telegram / Webhook 通知
- 事件日志：上下线、预警、耗尽、关机动作、域名切换、周期清零
- SSH 凭据与 DNS API 凭据用 AES-256-GCM 加密存储
- SSH 主机密钥 TOFU 校验，机器被换掉会拒绝连接并告警

## 安装

### 面板端

```bash
curl -fsSL https://raw.githubusercontent.com/zxcll/vps-panel/main/install.sh -o install.sh
sudo bash install.sh panel install
```

首次启动会打印一次随机管理员密码，记得保存。没看到的话：

```bash
journalctl -u vps-panel | grep -A6 初始化
```

不带参数运行 `sudo bash install.sh` 会进入交互菜单，安装、启动、停止、重启、查看日志、改配置、卸载都在里面。

**国内机器连不上 GitHub**，加个加速前缀：

```bash
GITHUB_PROXY=https://ghfast.top/ sudo bash install.sh panel install
```

### 节点端

在面板里新建节点，页面会给出一条带密钥的安装命令，复制到那台 VPS 上执行就行。

也可以手动装：

```bash
sudo bash install.sh agent install --server wss://panel.example.com --secret 节点密钥
```

### Docker

```bash
cp config.example.yaml config.yaml   # 按需修改
docker compose up -d
```

### 从源码编译

只需要 Go 1.26，**不需要 Node** —— 前端是浏览器直接能跑的 ES 模块，`go:embed` 打进二进制。

```bash
make build     # 产出 bin/panel 和四个架构的探针
make test      # 单元测试
make e2e       # 端到端验收
make run       # 本地起面板调试
```

## 部署建议

面板保存着各节点的 SSH 凭据，是高价值目标。**不要直接暴露在公网。**

推荐只监听 `127.0.0.1`，前面用 Nginx 反代 + HTTPS：

```nginx
server {
    listen 443 ssl http2;
    server_name panel.example.com;

    ssl_certificate     /etc/letsencrypt/live/panel.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/panel.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;

        # 探针走 WebSocket 长连接，这两行必须有
        proxy_set_header Upgrade    $http_upgrade;
        proxy_set_header Connection "upgrade";

        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # 长连接不能被反代掐断
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }
}
```

反代后面记得开 `trust_proxy_headers: true`（或环境变量 `PANEL_TRUST_PROXY=true`），
否则日志里记的是反代的 IP。

**要备份的东西**：数据目录下的 `panel.db`（流量账本、节点配置）和 `master.key`（凭据解密密钥）。
密钥丢了，所有 SSH 密码和 DNS 凭据都解不开，只能重填。

## 配置说明

启动期配置见 [`config.example.yaml`](config.example.yaml)。
运行期参数（离线判定阈值、失败次数、切换冷却、通知渠道、数据保留天数）在面板的「设置」页面里改，不用重启。

几个值得说明的：

| 参数 | 默认 | 说明 |
|---|---|---|
| 离线判定阈值 | 90 秒 | 心跳超过这个时长没来就认为探针失联。探针默认 30 秒报一次，留三倍余量 |
| 连续失败次数 | 3 | 达到这个次数才真判定下线，避免一次网络抖动就把域名切走 |
| TCP 拨测 | 开启 | 探针失联后再拨一次端口确认。探针进程崩了但机器还在跑业务时不该切 |
| 切换冷却 | 300 秒 | 两次自动切换的最小间隔，防止状态在临界点抖动导致解析来回改。手动切换不受限 |
| 计费倍率 | 1.0 | 网卡层统计和商家计量口径通常差几个百分点，用它校准 |

配额建议设成商家标称值的 95% 左右，留点余量。

## 验证

```bash
make test      # 单元测试
make e2e       # 端到端：起真实面板 + 探针，19 项断言
```

端到端脚本覆盖的场景：

1. 探针上报 → 面板账本累加
2. **机器重启（boot_id 变化）→ 账本继续累加而不是归零**
3. 面板进程重启 → 账本不丢
4. 重复上报同一份报文 → 不重复计账
5. 流量到达预警线 → 产生预警事件
6. 流量耗尽 → 状态转超额、写入事件、不误触发关机
7. 探针后续心跳不会把超额状态刷回在线
8. 手动清零 → 用量归零、上周期归档、超额限制解除
9. 单向计费口径确实按较大方向算

DNS 三家服务商的签名和请求构造有对应的单元测试（用 `httptest` mock），
选主逻辑（优先级、超额淘汰、冷却期、全员不健康时保持现状）也有独立测试。

首次配置真实域名时，先用面板上的「切换演练」确认选主结论符合预期，再交给自动切换。

## 项目结构

```
cmd/panel、cmd/agent        两个入口
internal/
  ingest/                   计数器 → 增量 → 账本（重启安全的核心）
  quota/                    计费口径与配额判定
  billing/                  账单周期边界（重置日 + 时区）
  failover/                 健康判定、选主、DNS 切换
  dns/                      Cloudflare / DNSPod / 阿里云，手写签名不引 SDK
  action/                   SSH 远程关机、探针指令下发
  engine/                   后台调度：周期滚动、配额判定、动作编排
  server/                   HTTP 接口、WebSocket 探针接入、内嵌前端
  agent/                    探针：采集、上报、执行指令
web/                        前端源码（Vue 3 ESM，无构建步骤）
```

## 已知限制

- **流量口径与商家不完全一致**。`/proc/net/dev` 统计的是网卡层字节，商家可能按 IP 层算，含不含协议开销也各家不同，通常差几个百分点。用计费倍率校准。
- **面板只能关机，不能开机**。没有带外管理权限，被关掉的机器要到服务商后台手动开。新周期开始时面板会解除超额限制，但机器还得你自己开。
- **容器里跑探针**时 `/proc/sys/kernel/random/boot_id` 是宿主机的，多个容器会撞成同一个值。用 `VPS_AGENT_BOOT_ID` 给每个容器指定不同的值。
- **单管理员账号**，没有多用户和角色权限。

## 许可

MIT
