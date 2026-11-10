# OpenDiscord 部署手册（单机 embedded）

> 适用：单机 2C2G 起步的 embedded 部署（bridge/user/data/admin 同进程）。
> 权威来源：安全基线见 [security-spec.md](security-spec.md) §6；数据与 API 设计见
> [admin-data-design.md](admin-data-design.md)。本文只写「怎么装、怎么改、怎么救」。
>
> 约定：`$` = 普通用户，`#` = root；`/opt/opendiscord/current` 永远指向当前 release 目录。
> 假设发行包（`.tar.gz` + `SHA256SUMS`）已上传到 `/root` 并解压。

---

## 0. 五分钟快速路径

```bash
# 0) 校验并解包（在存放 tar.gz 与 SHA256SUMS 的目录执行）
cd /root
sha256sum -c SHA256SUMS && tar -xzf opendiscord-<version>-linux-amd64.tar.gz
cd opendiscord-<version>

# 1) 装系统（建用户/目录/密钥/unit，幂等）
sudo ./deploy/install.sh

# 2) 改三个值：域名、冷库 DSN；监听地址保持 127.0.0.1
sudoedit /etc/opendiscord/opendiscord.toml

# 3) 起服务（骨架阶段先按附录 A.1 把 Type=notify 改为 Type=exec）
sudo systemctl enable --now opendiscord
systemctl is-active opendiscord && ss -lntp | grep -E ':(8080|7780|9090)'

# 4) 边缘：Caddy 自动 HTTPS
sudo cp /etc/opendiscord/Caddyfile.reference /etc/caddy/Caddyfile
sudoedit /etc/caddy/Caddyfile          # OPENDISCORD_DOMAIN / remote_ip 白名单
sudo caddy validate --config /etc/caddy/Caddyfile && sudo systemctl restart caddy

# 5) 首登：先在本机/跳板完成首个账号注册（自动成为管理员）→ /admin/ 开 TOTP
curl -sI https://chat.example.com/ | head -1        # 200
curl -s  https://chat.example.com/admin/ | grep -o '<title>[^<]*'
```

---

## 1. 系统要求

| 项 | 最低 | 说明 |
|---|---|---|
| 操作系统 | Linux + systemd 249+ | Debian 12 / Ubuntu 22.04 / Rocky 9 验证；x86_64 或 aarch64 |
| CPU / 内存 | 2 vCPU / 2 GB | `GOMEMLIMIT=1280MiB` 已写入 `/etc/opendiscord/env` |
| 磁盘 | 40 GB SSD | 媒体目录建议独立 LUKS 卷；备份目录与数据盘分开 |
| 冷库 | PostgreSQL 14+（外部） | `sslmode=require` 为底线；本机不装 PG 也能先跑（归档会失败告警） |
| 运行时依赖 | `bash` `coreutils` `openssl` `sqlite3` `curl` `tar` | `install.sh` 需要 `useradd/groupadd/getent/install`；`backup.sh` 需要 `sqlite3` |
| 可选依赖 | `age`（备份加密）、`rsync`（异地备份）、`jq`（日志过滤） | `age` 优先于 `openssl` |
| 边缘 | Caddy 2.8+（stock 即可） | 限流插件可选，见 §5.6 |
| 网络 | 域名 A/AAAA 已解析；公网放行 22/80/443 | 80 仅用于 ACME HTTP-01 与跳转 |

**端口与监听面**（全部只监听回环，公网唯一入口是 Caddy）：

| 地址 | 用途 | 谁能访问 |
|---|---|---|
| `127.0.0.1:8080` | 用户层 HTTP + WebSocket + 内嵌 SPA（`listen`） | Caddy |
| `127.0.0.1:7780` | 管理 API（`admin_listen`） | Caddy + IP 白名单 |
| `127.0.0.1:9090` | Prometheus 指标（`metrics_listen`，当前版本未监听，见附录 A.2） | 本机/内网 |
| `22 / 80 / 443` | SSH / ACME / HTTPS | 公网（22 建议源 IP 白名单） |

---

## 2. 安装（install.sh）

### 2.1 取包与校验

```bash
cd /root
sha256sum -c SHA256SUMS
tar -xzf opendiscord-<version>-linux-amd64.tar.gz
cd opendiscord-<version>
ls -l dist/chatd deploy/install.sh docs/deploy.md
sha256sum dist/chatd          # 与发布页 SHA256SUMS 对照
```

包内布局（`make release-all` 与 `release.sh` 产物一致）：

```
opendiscord-<version>/
├── dist/chatd                 # 单二进制（内含 client + admin 前端）
├── deploy/{install.sh,release.sh,backup.sh,opendiscord.toml,policy.json,Caddyfile,
│           systemd/,journald/,keygen/,logrotate.conf}
├── docs/deploy.md
├── README.md
└── LICENSE
```

> 前端已 `go:embed` 进 `chatd`，发布包**不再**带 `web/` 目录，目标机无需 Node。
> `install.sh` 兼容旧布局：若存在 `apps/{client,admin}/dist` 会额外复制到
> `/opt/opendiscord/releases/bootstrap/web/`，只作静态回退，不参与内嵌。

### 2.2 执行

```bash
sudo ./deploy/install.sh                 # embedded 单进程（1.4 起 embedded-only）
sudo ./deploy/install.sh --no-start      # 只落盘，不 enable/start（打镜像）
sudo ./deploy/install.sh --skip-keys     # 密钥由 KMS 下发时
sudo ./deploy/install.sh --prefix /opt/opendiscord --etc /etc/opendiscord --state /var/lib/opendiscord
```

| 参数 | 默认 | 作用 |
|---|---|---|
| `--no-start` | off | 只 `daemon-reload`，不 `enable --now` |
| `--skip-keys` | off | 跳过密钥生成（要求已由外部下发） |
| `--prefix` | `/opt/opendiscord` | 二进制 release 根 |
| `--etc` | `/etc/opendiscord` | 配置根 |
| `--state` | `/var/lib/opendiscord` | 数据根 |

退出码：`0` 成功 / `64` 用法错误 / `77` 非 root / `78` 缺少依赖（如 openssl 与 go 都没有）。

### 2.3 每一步做什么、产物在哪

| # | 动作 | 产物 | 现场校验 |
|---|---|---|---|
| 1 | 建组 `opendiscord` 与系统用户（`opendiscord`，shell=nologin，home=`/var/lib/opendiscord`；embedded-only） | `/etc/group`、`/etc/passwd` | `getent passwd opendiscord` |
| 2 | 建目录并按 §6 矩阵 chmod/chown | 见 §6 表 | `stat -c '%a %U:%G %n' /etc/opendiscord /var/lib/opendiscord` |
| 3 | 写环境变量文件（存在则不动） | `/etc/opendiscord/env`（0600 root:opendiscord） | `ls -l /etc/opendiscord/env` |
| 4 | 生成密钥（存在则跳过，绝不覆盖） | `keys/{jwt_sign_key.pem,admin_jwt_key.pem,ipc_key,hmac_key,pii_key,phone_lookup_key,download_key,upload_secret,audit_chain_key,backup.key}`；`/etc/opendiscord/backup.key -> keys/backup.key` | `ls -l /etc/opendiscord/keys`；`openssl pkey -in .../jwt_sign_key.pem -pubout \| openssl dgst -sha256` |
| 5 | 安装配置（存在则保留；`Caddyfile.reference` 每次覆盖） | `opendiscord.toml` 0640、`policy.json` 0640、`Caddyfile.reference` 0640；`config/chatd.toml -> ../opendiscord.toml` | `ls -l /etc/opendiscord` |
| 6 | 安装二进制（前端已内嵌；若包内有 `apps/*/dist` 则额外复制到 `current/web` 作静态回退） | `/opt/opendiscord/releases/bootstrap/bin/chatd`；`current -> releases/bootstrap`；`bin -> current/bin`；`web -> current/web` | `ls -l /opt/opendiscord/current/bin/chatd` |
| 7 | 安装 unit 与 journald 配额 | `/etc/systemd/system/opendiscord.service`；`/etc/systemd/journald.conf.d/opendiscord.conf`（`SystemMaxUse=200M`） | `systemctl cat opendiscord`；`journalctl --disk-usage` |
| 8 | `daemon-reload` + `enable --now`（`--no-start` 跳过） | 运行中的 unit | `systemctl is-active opendiscord` |
| 9 | 红线自检：`find /etc/opendiscord /var/lib/opendiscord -perm /o+rwx` 必须为空，否则 **exit 1** | — | 手工重跑同一条命令 |
| 10 | 打印下一步（改 toml / policy / Caddy / 首登） | stdout | — |

**幂等语义**：重复执行只校正目录权限，不覆盖已有配置、环境文件与密钥；升级配置需人工 merge
（`install_cfg` 会打印「已存在，保持不变」）。所以**升级前先备份 `/etc/opendiscord`**（§8.1）。

> 第 9 步失败时服务可能已经启动（第 8 步在前）。修完权限重跑脚本即可，不必先停服。
> `install.sh` **不安装** `adminctl` / `opendiscordctl`，CLI 安装见 §9.1。

---

## 3. 配置项

### 3.1 `/etc/opendiscord/opendiscord.toml`（0640 root:opendiscord）

改完 **必须 restart**（TOML 不热载）：`sudo systemctl restart opendiscord`。

| 字段 | 默认值 | 含义与约束 |
|---|---|---|
| `mode` | `"embedded"` | embedded 单进程。**1.4 起 embedded-only**：split 多进程未实现（lts-1.4-plan §5 #4），勿改 |
| `listen` | `"127.0.0.1:8080"` | 用户层 HTTP/WS 监听。**必须是回环**；公网只走 Caddy。也用它托管内嵌 SPA |
| `admin_listen` | `"127.0.0.1:7780"` | 管理 API 监听（Caddy 仅对白名单 IP 反代 `/api/admin/*`） |
| `metrics_listen` | `"127.0.0.1:9090"` | Prometheus `/metrics` 监听（契约字段，见下方「消费进度」） |
| `public_origin` | `"https://chat.example.com"` | 对外站点，用于绝对 URL、CORS、Cookie 域；**不带尾斜杠**，必须与实际域名一致（否则 WS 握手 401） |
| `hot_db` | `"/var/lib/opendiscord/chatd.db"` | 热库 SQLite（WAL）。文件 0600，目录 0700 |
| `admin_db` | `"/var/lib/opendiscord/admin.db"` | 管理库（管理员/会话），独立文件做最小权限隔离 |
| `media_dir` | `"/var/lib/opendiscord/media"` | 媒体根（LUKS 卷挂载点），0750 |
| `jwt_key_path` | `"/etc/opendiscord/keys/jwt_sign_key.pem"` | 用户 token 签名私钥（Ed25519 PKCS#8，0640 root:opendiscord） |
| `pii_hmac_key_path` | `"/etc/opendiscord/keys/phone_lookup_key"` | 手机号/邮箱 HMAC 索引密钥（32B hex，0600） |
| `cold_dsn` | `postgres://opendiscord@db.internal:5432/opendiscord?sslmode=require` | 冷库 DSN；`sslmode=require` 为底线，内网也建议 `verify-full` |
| `policy_path` | `"/etc/opendiscord/policy.json"` | 策略唯一真源（保留期/cron/限流）；SIGHUP 热载 |
| `log_level` | `"info"` | `debug`/`info`/`warn`/`error`；日志只写 stdout → journald |
| `shutdown_timeout_sec` | `30` | SIGTERM 优雅退出预算（flusher 最后组提交 + 归档断点） |
| `watchdog_interval_sec` | `7` | 进程心跳间隔，取 systemd `WatchdogSec=15` 的一半（契约字段） |
| `max_devices_per_user` | `3` | 单用户并发设备上限（用户级限流聚合，§1.2.1；契约字段） |
| `max_upload_bytes` | `52428800` | 单文件上传上限（50 MB，D-3/D-4；契约字段） |
| `daily_upload_bytes_per_user` | `2147483648` | 单用户日上传上限（2 GB，D-4 红线；契约字段） |
| `[feature_flags].video_enabled` | `false` | 视频消息开关；运行期可后台热改（kind 911 下发） |
| `[feature_flags].e2ee` | `false` | 端到端加密开关 |
| `[feature_flags].registration` | `true` | 注册开关；被刷时后台关（§6.4.3 一键禁注册） |
| `[feature_flags].readonly` | `false` | 全站只读（收信保留，MSG_SEND 回 READONLY） |
| `[feature_flags].call_enabled` | `false` | 通话开关 |
| `[socket].bus/user/data/admin` | 注释占位 | 前瞻字段；split 未实现，当前无效 |

**不要改**：监听地址改成 `0.0.0.0`（绕过 Caddy 等于裸奔）、`public_origin` 用 http、
把 `hot_db` 放到 NFS（SQLite WAL 不支持网络文件系统）。

> **热库 PRAGMA 与持久性（perf-spec 模块 3）**：热库连接强制
> `journal_mode=WAL`、`synchronous=NORMAL`、`busy_timeout=5000`、
> `foreign_keys=ON`、`cache_size=-8000`（8MB/连接）、`journal_size_limit=64MB`、
> `wal_autocheckpoint=2000`（约 8MB WAL 触发被动检查点，减少写路径停顿）。
> **`synchronous=NORMAL`（而非 `OFF`）的持久性权衡**：WAL 模式下进程崩溃不丢已提交
> 事务；但**操作系统崩溃 / 掉电会丢失最近一次 checkpoint 之后、尚未 fsync 的事务**。
> 这是吞吐与持久性的显式取舍，**默认值即如此，且不提供改成 `OFF` 的开关**
> （`OFF` 会在掉电时丢失任意多条已 ACK 消息，不适合作为默认）。对消息不可丢有强
> 需求的部署，应通过外部机制（写前日志、异地冗余）而非 `synchronous=OFF` 补偿。

> **消费进度**：当前 `chatd` 已读取 `mode/listen/admin_listen/public_origin/hot_db/admin_db/
> cold_dsn/policy_path/jwt_key_path/pii_hmac_key_path/media_dir/log_level/shutdown_timeout_sec`
> 与 `[feature_flags]`；`metrics_listen`、`watchdog_interval_sec`、`max_devices_per_user`、
> `max_upload_bytes`、`daily_upload_bytes_per_user` 是契约字段，本版本尚未消费（未知键被
> 安全忽略，不会导致启动失败）。改这些字段前先确认二进制版本。

### 3.2 `/etc/opendiscord/policy.json`（0640 root:opendiscord）

策略唯一真源；改完 `sudo systemctl reload opendiscord`（SIGHUP）热生效。非法策略会被拒绝并继续用旧值。

| 键 | 默认 | 含义 |
|---|---|---|
| `hot_retention_days` | `90` | 热库保留天数；超期由归档作业搬去冷库 |
| `archive.cron` | `"0 4 * * *"` | 归档调度（每日 04:00） |
| `archive.batch_size` | `1000` | 单批搬运行数 |
| `archive.batch_pause_ms` | `100` | 批间停顿，避免打满 I/O |
| `archive.active_exempt` | `true` | 活跃会话（近 7 天有消息）不归档 |
| `backup.cron` | `"30 5 * * *"` | 备份调度（每日 05:30；调度器见 §8.2） |
| `backup.dir` | `"/var/lib/opendiscord/backup"` | 本地备份目录（0700） |
| `backup.keep_days` | `14` | 本地备份保留天数 |
| `trim[]` | `sys_rate_limit` 7 天、`identity_login_history` 180 天 | 只允许白名单表；其他值加载即拒绝（防动态 SQL） |
| `rate_limit.msg_per_user_per_min` | `60` | 用户级消息速率（跨设备聚合） |
| `rate_limit.login_per_ip_per_min` | `10` | 单 IP 登录尝试速率 |
| `write_path.batch_size` | `1000` | 写路径组提交批大小（一个事务的消息条数）。调大可提吞吐；上限 `8000`。批越大，单次提交的最坏 ACK 延迟越高 |
| `write_path.flush_interval_ms` | `50` | 组提交兜底间隔（不足一批时最多等多久提交）。调小延迟更低但提交更频繁。范围 `5`–`2000` |

> **`write_path` 调参建议（perf-spec 模块 3，写路径优化）**：默认
> `batch_size=1000`、`flush_interval_ms=50` 与 v1.1.1 行为一致（可安全回退）。
> 压测建议从 `batch_size=2000`、`flush_interval_ms=50` 起步（预期 ~1.5x），
> 只在 P95 仍达标时继续加。`write_path` 只影响吞吐/延迟，不改变
> all-or-nothing、去重（`ErrDuplicate`）与崩溃持久性语义；SIGHUP 热载
> 对**下一批**生效（在途批次按旧参数提交）。

### 3.3 `/etc/opendiscord/env`（0600 root:opendiscord）

只放运行时参数，不放密钥（密钥走 `keys/`）：

| 变量 | 默认 | 说明 |
|---|---|---|
| `TZ` | `UTC` | 固定 UTC，避免 cron/归档时间漂移 |
| `GOMEMLIMIT` | `1280MiB` | Go 软内存上限（2C2G 留 700MB 给系统/页缓存） |
| `OPENDISCORD_CONFIG` | `/etc/opendiscord/opendiscord.toml` | 配置路径（`chatd --config` 的默认值来源） |
| `OPENDISCORD_LAYER` | `embedded` | 进程角色标识（embedded-only） |

改完 `systemctl restart opendiscord`（EnvironmentFile 只在启动时读取）。

---

## 4. 首次启动与管理员引导

### 4.1 启动前检查（4 条，30 秒）

```bash
# 1) 配置键齐全、引号成对（TOML 不支持中文标点）
grep -nE '^(mode|listen|admin_listen|public_origin|hot_db|cold_dsn|policy_path|jwt_key_path)' /etc/opendiscord/opendiscord.toml

# 2) 密钥齐全且权限对（私钥 0640，对称密钥 0600）
ls -l /etc/opendiscord/keys/

# 3) unit 语法与安全评分
systemd-analyze verify /etc/systemd/system/opendiscord.service
systemd-analyze security --offline=true /etc/systemd/system/opendiscord.service | tail -3

# 4) 端口没被占用
ss -lntp | grep -E ':(8080|7780|9090)' || echo "ports free"
```

### 4.2 启动

```bash
sudo systemctl enable --now opendiscord
systemctl is-active opendiscord                 # 期望 active
journalctl -u opendiscord -n 50 --no-pager      # 期望 msg="chatd ready"

# 本机验证（绕过 Caddy，确认上游活着）
curl -sI http://127.0.0.1:8080/ | head -3             # 200 + text/html
curl -s  http://127.0.0.1:8080/admin/ | grep -o '<title>[^<]*'
curl -s  http://127.0.0.1:8080/healthz               # 期望 ok/200
```

> 骨架阶段 `Type=notify` 未实现 `sd_notify`，会 `start operation timed out`。
> 先按 **附录 A.1** 临时改 `Type=exec`（上线前恢复并实现 notify + watchdog）。

### 4.3 先堵抢注，再开放公网

**首个注册账号自动成为管理员**（见 [admin-guide.md](admin-guide.md)）。因此开放公网前必须
先完成首登，否则任何人都可能抢到管理员：

```bash
# 方式 A（推荐）：只在本机操作
ssh -L 8080:127.0.0.1:8080 ops@chat.example.com
# 浏览器打开 http://127.0.0.1:8080 完成注册

# 方式 B：防火墙临时只放行运维出口 IP
sudo nft add rule inet filter input tcp dport {80,443} ip saddr != <你的IP> drop
```

完成 §4.4 后再放开公网。

### 4.4 管理员引导（逐屏操作）

| 步 | 操作 | 期望结果 |
|---|---|---|
| 1 | 打开 `https://chat.example.com/`，用手机号/邮箱注册第一个账号 | 账号创建成功，角色自动为管理员 |
| 2 | 打开 `https://chat.example.com/admin/`，用同一账号登录 | 进入管理后台（登录需要密码 + TOTP；首次会要求绑定） |
| 3 | 绑定 TOTP：扫描二维码，输入 6 位验证码 | 绑定成功；**立即抄下 8 个一次性恢复码**（服务端只存哈希，丢了只能重置） |
| 4 | 核对身份与权限（`adminctl whoami` 返回同一管理员） | 角色/RBAC 正确 |
| 5 | 打开「审计日志」→「校验哈希链」 | 返回 ok；记下当前最大 id 作为基线 |
| 6 | 打开「用户」页确认列表可读（空列表也算通过） | `/api/admin/v1/*` 白名单生效 |

TOTP 参数（§3.3 红线）：**SHA-1 / 6 位 / 30s 周期，时间窗 ±1**；连续 10 次失败锁 30 分钟；
管理会话 access TTL 5 分钟、idle 15 分钟强制重认证。服务器时间必须同步（§5.6），否则全员 TOTP 失败。

### 4.5 上线验收（5 条，全过才算部署完成）

```bash
# 1) 站点 200 且证书有效
curl -sSI https://chat.example.com/ | grep -E 'HTTP/|strict-transport-security'
# 2) 管理后台 200（白名单内）
curl -sS https://chat.example.com/admin/ | grep -q 'OpenDiscord 管理后台' && echo admin-ok
# 3) 指标只对本机开放：公网应为 404
curl -s -o /dev/null -w '%{http_code}\n' https://chat.example.com/metrics   # 期望 404
# 4) 管理 API 对非白名单 404（从白名单外的机器执行）
curl -s -o /dev/null -w '%{http_code}\n' https://chat.example.com/api/admin/v1/whoami  # 期望 404
# 5) 审计链基线
adminctl --server http://127.0.0.1:7780 audit --since-id 0 --limit 1 --output json
```

---

## 5. Caddy 与 TLS

### 5.1 安装 Caddy 2.8+

```bash
# Debian/Ubuntu（官方源）
sudo apt install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
  | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
  | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo apt update && sudo apt install -y caddy
caddy version        # 期望 v2.8.x 或更高
```

### 5.2 部署 Caddyfile 与环境变量

Caddyfile 用 `{$VAR:default}` 读环境变量，变量放在 systemd drop-in（不要写死在 unit 主文件）：

```bash
sudo mkdir -p /etc/systemd/system/caddy.service.d
sudo tee /etc/systemd/system/caddy.service.d/opendiscord.conf >/dev/null <<'EOF'
[Service]
Environment=OPENDISCORD_DOMAIN=chat.example.com
Environment=OPENDISCORD_ACME_EMAIL=ops@example.com
Environment=OPENDISCORD_WS_PORT=8080
Environment=OPENDISCORD_METRICS_PORT=9090
EOF
sudo systemctl daemon-reload

sudo cp /etc/opendiscord/Caddyfile.reference /etc/caddy/Caddyfile
sudo caddy fmt --overwrite /etc/caddy/Caddyfile
sudo caddy validate --config /etc/caddy/Caddyfile     # 必须 "Valid configuration"
```

> 本 Caddyfile 的 global 块是 `admin off`（关闭本地 admin API），因此**不能**用
> `caddy reload` / `systemctl reload caddy` 热加载；改完配置用 `systemctl restart caddy`
> （中断 < 1s，可接受）。若要热加载，删掉 `admin off` 并确保 2019 端口只监听回环。

### 5.3 路由与保护

| 路径 | 上游 | 保护 |
|---|---|---|
| `/ws` | `127.0.0.1:{$OPENDISCORD_WS_PORT:8081}` | WS upgrade；`flush_interval -1`；建议 10 次/分钟/IP（E-3）+ 失败熔断（E-4） |
| `/api/admin/*` | `127.0.0.1:7780` | `remote_ip` 白名单；**非白名单返回 404**（不暴露管理端存在） |
| `/metrics` | `127.0.0.1:{$OPENDISCORD_METRICS_PORT:9090}` | 仅 127/8、10/8、192.168/16；公网 404 |
| `/api/*` | `127.0.0.1:8080` | 请求体 1 MB（E-7） |
| `/api/v1/media/presign` | `127.0.0.1:8080` | 请求体 64 KB（E-7） |
| 其余（`/`、`/admin/`、静态资源） | `127.0.0.1:8080` | 二进制内嵌 SPA；index 不缓存、hash 资源 immutable |

管理端 IP 白名单在 Caddyfile 里改这一行（换成你的运维出口 IP，可多个）：

```caddyfile
@notAllowed not remote_ip 127.0.0.1/32 10.0.0.0/8 192.168.0.0/16 203.0.113.10/32
```

改完 `sudo caddy validate ... && sudo systemctl restart caddy`，再从白名单外验证返回 404。

### 5.4 TLS / ACME

- **HTTP-01**（默认）：Caddy 自动申请 Let's Encrypt 证书，要求 80 端口公网可达且域名解析正确。
  证书落在 `/var/lib/caddy/.local/share/caddy/certificates/`，到期前 30 天自动续期。
- **DNS-01**（80 被封/内网）：需要带 DNS provider 插件的 Caddy
  （`xcaddy build --with github.com/caddy-dns/<provider>`），在 global 块加 `acme_dns <provider> <token>`；
  stock Caddy 不支持。
- 自签（仅测试）：`tls internal`，浏览器需手动信任根证书；**生产禁用**。
- 检查：

```bash
curl -vI https://chat.example.com 2>&1 | grep -E 'subject:|issuer:|expire date|SSL certificate verify'
echo | openssl s_client -connect chat.example.com:443 -servername chat.example.com 2>/dev/null \
  | openssl x509 -noout -dates -issuer
```

- HSTS 已带 `includeSubDomains; preload`：**只有当所有子域都永久 HTTPS 时**才保留 preload，
  否则删掉 `preload` 与 `includeSubDomains`，避免把自己锁死。

### 5.5 防火墙（nftables 最小集）

```bash
sudo tee /etc/nftables.conf >/dev/null <<'EOF'
#!/usr/sbin/nft -f
flush ruleset
table inet filter {
  chain input {
    type filter hook input priority 0; policy drop;
    ct state established,related accept
    iif lo accept
    ip protocol icmp accept
    tcp dport 22 ip saddr { 203.0.113.0/24 } accept   # SSH 只放行运维网段
    tcp dport { 80, 443 } accept
    counter drop
  }
  chain forward { type filter hook forward priority 0; policy drop; }
  chain output  { type filter hook output  priority 0; policy accept; }
}
EOF
sudo nft -f /etc/nftables.conf && sudo systemctl enable --now nftables
```

出站建议白名单（443、DNS、NTP、短信/邮件供应商 API），至少保证 443 与 DNS 可用，
否则 ACME、冷库、OTP 通道都会失败。

### 5.6 限流与时间同步

- Caddy 核心无 `rate_limit`。三选一：编译 `caddy-ratelimit` 插件（Caddyfile 内已留注释位）、
  前置 nftables `ct count` 做 IP 级、云 WAF。E-3/E-4 是**红线**，必须落地其一。
- 时间同步是 TOTP/JWT 红线（§6.3）：`sudo timedatectl set-ntp true && timedatectl status`
  或装 chrony；偏差 > 30s 会导致全部 TOTP 校验失败。

### 5.7 Caddy 自检

```bash
sudo systemctl status caddy --no-pager | head -5
sudo journalctl -u caddy -n 50 --no-pager | grep -iE 'error|certificate|obtain'
ss -lntp | grep -E ':(80|443)\b'
```

---

## 6. 目录与权限矩阵

embedded 形态（**1.4 起 embedded-only**，split 多进程未实现）。**这张表就是 security-spec §6.2 红线的落地值**，
任何 `other` 位带 `rwx` 都视为未通过上线评审。

| 路径 | owner:group | mode | 内容 / 说明 |
|---|---|---|---|
| `/opt/opendiscord` | root:root | 0755 | 发布根；运行用户不可写 |
| `/opt/opendiscord/releases/<ver>` | root:root | 0755 | 每个版本一个目录 |
| `/opt/opendiscord/releases/<ver>/bin/*` | root:root | 0755 | `chatd` 等二进制 |
| `/opt/opendiscord/current` | root:root | symlink | → `releases/<ver>`（原子的切换点） |
| `/opt/opendiscord/bin` | root:root | symlink | → `current/bin` |
| `/opt/opendiscord/web` | root:root | symlink | → `current/web`（旧布局静态回退，可空） |
| `/etc/opendiscord` | root:opendiscord | 0750 | 配置根 |
| `/etc/opendiscord/opendiscord.toml` | root:opendiscord | 0640 | 运行配置 |
| `/etc/opendiscord/policy.json` | root:opendiscord | 0640 | 策略真源 |
| `/etc/opendiscord/Caddyfile.reference` | root:opendiscord | 0640 | 边缘配置样例（每次安装覆盖） |
| `/etc/opendiscord/env` | root:opendiscord | 0600 | 环境变量 |
| `/etc/opendiscord/keys/` | root:opendiscord | 0750 | 密钥目录 |
| `/etc/opendiscord/keys/*.pem` | root:opendiscord | 0640 | Ed25519 私钥（JWT 签名） |
| `/etc/opendiscord/keys/{ipc,hmac,pii,phone_lookup,download,upload_secret,audit_chain}_key` | root:opendiscord | 0600 | 对称密钥 |
| `/etc/opendiscord/keys/backup.key` | root:opendiscord | 0600 | 备份加密口令（`/etc/opendiscord/backup.key` 为符号链接） |
| `/var/lib/opendiscord` | opendiscord:opendiscord | 0700 | 运行数据根（embedded） |
| `/var/lib/opendiscord/chatd.db` | opendiscord:opendiscord | 0600 | 热库（含 `-wal`/`-shm`） |
| `/var/lib/opendiscord/admin.db` | opendiscord:opendiscord | 0600 | 管理库 |
| `/var/lib/opendiscord/media/` | opendiscord:opendiscord | 0750 | 媒体对象（建议 LUKS 卷） |
| `/var/lib/opendiscord/backup/` | opendiscord:opendiscord | 0700 | 本地加密备份 |
| `/run/opendiscord/` | opendiscord:opendiscord | 0750 | UDS 目录（systemd `RuntimeDirectory`） |
| `/etc/systemd/system/opendiscord*.service` | root:root | 0644 | unit |
| `/etc/systemd/journald.conf.d/opendiscord.conf` | root:root | 0644 | journald 配额 |

### 6.1 权限自检（贴进巡检脚本）

```bash
# 1) 红线：other 位不得有 rwx（期望无输出）
sudo find /etc/opendiscord /var/lib/opendiscord -perm /o+rwx -print

# 2) 关键路径逐项核对
stat -c '%a %U:%G %n' /etc/opendiscord /etc/opendiscord/keys /etc/opendiscord/env \
  /var/lib/opendiscord /var/lib/opendiscord/media /var/lib/opendiscord/backup

# 3) 逐级解析（确认父目录没有意外放行）
namei -l /etc/opendiscord/keys/jwt_sign_key.pem

# 4) 二进制不可被运行用户改写
sudo -u opendiscord test -w /opt/opendiscord/current/bin/chatd && echo "FAIL: writable" || echo ok
```

---

## 7. 升级与回滚（release.sh）

### 7.1 发布机前置

| 依赖 | 用途 |
|---|---|
| `ssh` / `scp` | **必须免密（BatchMode）**；`~/.ssh/config` 配好目标机 |
| `tar` / `sha256sum` | 打包与校验 |
| `node` + `npm` | 构建 client/admin（`--skip-build` 可跳过，复用 `apps/*/dist`） |
| `go` | 构建 `chatd`（`--skip-build` 时复用 `dist/`） |
| 目标机 `curl` / `systemctl` | 健康检查与重启 |

```bash
ssh -o BatchMode=yes root@chat.example.com true   # 先验证免密
```

### 7.2 命令

```bash
# 全量：构建 → 打包 → 上传 → 切换 → 健康检查 → 失败自动回滚
./deploy/release.sh --target root@chat.example.com

# 指定版本 / 复用 CI 已构建的 dist
./deploy/release.sh --target root@chat.example.com --version v1.4.2 --skip-build

# 其它参数
--service opendiscord.service     # systemd 单元名
--health-url http://127.0.0.1:8080/healthz
--remote-root /opt/opendiscord    # 远端根
--ssh-port 2222
```

版本号默认 `git describe --tags --always --dirty`，并做白名单清洗
（`tr -c 'A-Za-z0-9._-' '-'`），会进入目录名与回滚命令，不要用带空格/斜杠的字符串。

### 7.3 release.sh 实际行为（8 步）

| # | 动作 | 失败时的表现 |
|---|---|---|
| 1 | `npm ci && npm run build`（client、admin 各自目录） | 立即退出，不动远端 |
| 2 | `go build -trimpath -ldflags "-s -w"` 逐个子命令（只有存在的 `server/cmd/*` 才编） | 同上；`dist/chatd` 缺失直接 die |
| 3 | 打包 `dist/opendiscord-<ver>-linux-<arch>.tar.gz`：`bin/` + `web/{client,admin}` + `deploy/`，并生成 `dist/SHA256SUMS` | 同上 |
| 4 | `scp` 到 `$REMOTE_ROOT/incoming/` | SSH 失败即退出 |
| 5 | 远端 `sha256sum -c SHA256SUMS` → 解包到 `releases/<ver>` → `chown -R root:root` | 校验失败即退出，旧版本不受影响 |
| 6 | 原子切换 `current`（`ln -sfn` 到 `.new` + `mv -Tf`），同步 `bin`/`web` 链接，`systemctl restart` | 启动失败进入回滚 |
| 7 | 健康检查：最多 20 次 × 1s 请求 `--health-url`；若返回 404 则退化为 `systemctl is-active`（兼容旧构建） | 失败 → `current` 指回 `last` → 重启 → 再检查 |
| 8 | 记录 `last`，清理 `incoming` 与多余 release（只留 `current`、`last`、`bootstrap`） | 清理失败不影响服务 |

回滚语义：第 7 步失败时把 `current` 指回上一版本并重启；**回滚也失败**则退出非 0 并保留现场
（`journalctl -u opendiscord -n 100`）。首次发布没有 `last` 时无法自动回滚，必须人工介入。

### 7.4 产物与校验

```bash
ls -l dist/opendiscord-*-linux-*.tar.gz dist/SHA256SUMS
sha256sum -c dist/SHA256SUMS          # 在 dist/ 目录内执行
tar -tzf dist/opendiscord-<ver>-linux-amd64.tar.gz | head -20
```

远端布局（`--remote-root /opt/opendiscord`）：

```
/opt/opendiscord/
├── releases/v1.4.2/{bin,web,deploy}
├── releases/v1.4.1/            # last
├── current -> releases/v1.4.2
├── last    -> releases/v1.4.1
├── bin     -> current/bin
└── web     -> current/web
```

### 7.5 手工回滚

```bash
# 1) 看当前与上一版本
readlink -f /opt/opendiscord/current /opt/opendiscord/last

# 2) 切回上一版本（原子）
sudo ln -sfn "$(readlink -f /opt/opendiscord/last)" /opt/opendiscord/current.new
sudo mv -Tf /opt/opendiscord/current.new /opt/opendiscord/current
sudo ln -sfn current/bin /opt/opendiscord/bin
sudo ln -sfn current/web /opt/opendiscord/web

# 3) 重启并验证
sudo systemctl restart opendiscord
systemctl is-active opendiscord
journalctl -u opendiscord -n 100 --no-pager
```

**数据库迁移只向前**：回滚二进制前先确认该版本引入的迁移是否向后兼容；破坏性迁移必须
先做备份（§8）并在 staging 演练。不确定时不要回滚，改用前向修复（forward fix）。

### 7.6 `make release-all` 与 `release.sh` 的分工

| | `make release-all` | `deploy/release.sh` |
|---|---|---|
| 产物 | `dist/opendiscord-<ver>-linux-<arch>.tar.gz` + `dist/SHA256SUMS` | 同左，但直接上传并切换 |
| 前端 | `make web`（npm ci + build + 复制到 `server/web/{client,admin}`） | 自己 `npm ci + build`，再把 `apps/*/dist` 打进 `web/` |
| 适用 | CI 出包、离线交付 | 运维一条命令发布到远端 |
| 前置 | 本机 Node + Go | 本机 Node + Go + 免密 SSH |

---

### 7.7 升级到 v1.4：`opendiscordctl upgrade`（一次性维护自动化）

1.4 把历史上需要手工做的两项一次性维护合并成一条命令：

| # | 步骤 | 背景 |
|---|---|---|
| 1 | 旧 `thumb_data` 内联缩略图迁移到 `media_dir/thumbs/` 文件（即原 `migrate thumbs`） | v1.0 的缩略图按 base64 存在热库，每行 ~10.7KB，百万级图片可占 >10GB |
| 2 | 热库 `auto_vacuum=0 → INCREMENTAL` 转换（`PRAGMA auto_vacuum=INCREMENTAL; VACUUM;`） | SQLite 默认 `auto_vacuum=0` 只把删除的页标记为空闲，文件不缩小；不转换则 2C2G 上磁盘只涨不落 |

**安全约定：默认 dry-run**——不带 `--run` 时命令只读热库、输出结构化计划（含
`--json` 信封），不写盘、不改库、不加写锁；带 `--run` 才执行。auto_vacuum 的
`VACUUM` 会重写整个库文件，因此 `--run` 前服务必须已停止（`systemctl is-active`
检测到运行中会拒绝并提示，exit 77；仅离线维护窗口可用 `--force` 跳过检查）。

推荐流程：

```bash
# 1. 先备份（§8）
sudo deploy/backup.sh

# 2. 停服（VACUUM 期间不能有连接）
sudo systemctl stop opendiscord

# 3. 预览计划（dry-run，只读）
opendiscordctl upgrade --hot-db /var/lib/opendiscord/data/hot.db
opendiscordctl upgrade --json --hot-db /var/lib/opendiscord/data/hot.db   # 结构化输出

# 4. 执行（thumbs 迁移 + auto_vacuum 转换，幂等，可重跑）
opendiscordctl upgrade --run --hot-db /var/lib/opendiscord/data/hot.db

# 5. 校验并启动：auto_vacuum 必须为 2（INCREMENTAL）
opendiscordctl doctor --hot-db /var/lib/opendiscord/data/hot.db   # 应全 OK
sudo systemctl start opendiscord
```

计划输出示例（dry-run）：

```
upgrade: dry-run（未改库；加 --run 执行）
  hot_db      : /var/lib/opendiscord/data/hot.db (auto_vacuum=0)
  [!] thumbs       1234 行旧 thumb_data 待迁移到 /var/lib/opendiscord/media
  [!] auto_vacuum  auto_vacuum=0 → 需转换为 INCREMENTAL（PRAGMA auto_vacuum=INCREMENTAL; VACUUM;）
图例: ! 待执行  ✓ 已执行  - 无需  ✗ 失败
```

已转换的库重跑为 no-op（两个步骤均报「无需」）。等价的手工路径（不使用 CLI 时）：
`VACUUM INTO` 备份 → `sqlite3 "$HOT" "PRAGMA auto_vacuum=INCREMENTAL; VACUUM;"` →
`PRAGMA auto_vacuum;` 返回 2。

**升级检查清单（→ v1.4）**

| 步骤 | 命令 / 动作 | 失败会怎样 |
|---|---|---|
| 1 | 备份热库与配置（`deploy/backup.sh`） | 无备份可回滚 |
| 2 | `opendiscordctl upgrade`（dry-run 预览 → `--run` 执行；含 auto_vacuum 转换与 thumbs 迁移，一次性） | 磁盘只涨不落 / 热库虚胖 |
| 3 | 确认 `rate_limit_enabled` 未显式为 false | 生产环境会**拒绝启动**（这是有意的护栏） |
| 4 | 确认 `public_origin` 已设为 https 域名 | 同源校验与安全 Cookie 走宽松分支 |
| 5 | 若用短信/邮件登录：配置供应商（未配则仅 dev 模式可用） | 生产注册登录不可用 |
| 6 | 若要开视频/语音通话：`feature_flags.call_enabled=true` + 配 TURN | 通话按钮不出现/建连失败 |
| 7 | 替换二进制 → `systemctl restart opendiscord` | — |
| 8 | `opendiscordctl doctor` + `opendiscordctl status` | 逐项排查 |

## 8. 备份与恢复（backup.sh）

### 8.1 先搞清楚 backup.sh 备份了什么

`deploy/backup.sh` **只备份热库 `chatd.db`**（外加 manifest 与 sha256）。
以下内容**不在**备份范围内，必须单独保护：

| 对象 | 为什么重要 | 单独备份方式 |
|---|---|---|
| `/var/lib/opendiscord/admin.db` | 管理员账号、TOTP secret、会话 | 停服或 `sqlite3 .backup` 后加密（见 §8.5） |
| `/etc/opendiscord/keys/` | `audit_chain_key` 丢失 = 审计链永久无法验证；`pii_key` 丢失 = 手机号不可解 | 离线加密保管，**不要**放进 `/var/lib/opendiscord/backup` 自动清理目录 |
| 媒体文件 `media/` | 图片/语音/文件本体 | LUKS 卷快照或 `restic`/`borg` 增量 |
| 冷库 PostgreSQL | 90 天前的全量历史 | PostgreSQL 自身 PITR / `pg_dump` |
| `/etc/opendiscord/*.toml/json` | 配置与策略 | 随发布包或配置管理（Git）留存 |

> 只恢复 `chatd.db` 而丢掉 `admin.db`/`keys`，等于「有消息但没人能登录、审计链无法验证」。
> 生产环境必须把上表纳入同一份恢复演练。

### 8.2 调度（policy.json 只描述策略，不自带调度器）

`policy.json` 的 `backup.cron` 是**声明**，由外部调度器执行。推荐 systemd timer（可查、幂等）：

```bash
sudo install -m 0755 deploy/backup.sh /usr/local/sbin/opendiscord-backup

sudo tee /etc/systemd/system/opendiscord-backup.service >/dev/null <<'EOF'
[Unit]
Description=OpenDiscord hot DB backup (sqlite3 .backup + encrypt)
After=opendiscord.service
[Service]
Type=oneshot
User=root
ExecStart=/usr/local/sbin/opendiscord-backup
Nice=10
IOSchedulingClass=best-effort
IOSchedulingPriority=7
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/opendiscord/backup
NoNewPrivileges=true
EOF

sudo tee /etc/systemd/system/opendiscord-backup.timer >/dev/null <<'EOF'
[Unit]
Description=Daily OpenDiscord backup at 05:30
[Timer]
OnCalendar=*-*-* 05:30:00
Persistent=true
RandomizedDelaySec=300
[Install]
WantedBy=timers.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now opendiscord-backup.timer
systemctl list-timers opendiscord-backup.timer
```

`OnCalendar` 用**系统时区**；建议 `sudo timedatectl set-timezone UTC` 与 `policy.json` 的
`"30 5 * * *"` 对齐。备份以 root 运行：`backup.key` 是 0600 root:opendiscord，普通用户读不到。

### 8.3 手动备份与产物

```bash
# 预检：空间、密钥、加密工具、异地目标（不写任何文件）
sudo /usr/local/sbin/opendiscord-backup --dry-run

# 立即备份
sudo /usr/local/sbin/opendiscord-backup
```

产物（`$BACKUP_DIR` 默认 `/var/lib/opendiscord/backup`，umask 077）：

```
opendiscord-20261029T053000Z.db.enc        # openssl 加密；用 age 时为 .db.age
opendiscord-20261029T053000Z.db.enc.sha256 # 密文校验和
opendiscord-20261029T053000Z.manifest.json # created_at / 源库大小 / 明文 sha256 / 保留天数 / host
```

流程：`sqlite3 .backup`（在线一致快照，不锁写）→ `PRAGMA integrity_check` 必须 ok →
立即删除明文（有 `shred` 则覆写）→ 加密 → 原子 rename 落盘 → 异地 → 按 `keep_days` 清理。
任一步失败退出非 0，timer 记录 failed。

加密选择：`OPENDISCORD_AGE_RECIPIENT`（或 `backup.key` 内容以 `age1` 开头）且装了 `age` → age；
否则 `openssl enc -aes-256-cbc -pbkdf2 -iter 200000 -salt -pass file:/etc/opendiscord/backup.key`。

### 8.4 异地副本

```bash
# rsync（写进 systemd unit 的 Environment= 或 root 的 crontab）
OPENDISCORD_BACKUP_RSYNC=backup@10.0.0.9:/srv/opendiscord-backup

# 对象存储（aws 或 rclone 二选一）
OPENDISCORD_BACKUP_S3=s3://my-bucket/opendiscord
```

未配置异地目标时脚本只告警（不会失败），但**本机磁盘故障会同时丢失数据与备份**——
生产环境必须配置其一，并定期验证能拉回。

### 8.5 恢复演练（每季度一次，先在 staging 做）

```bash
# 0) 选定一份备份（含 .sha256 与 manifest）
B=/var/lib/opendiscord/backup
LATEST=$(ls -1t "$B"/opendiscord-*.db.enc "$B"/opendiscord-*.db.age 2>/dev/null | head -1)
echo "$LATEST"

# 1) 先验密文完整性（在备份目录内执行，文件名要对得上）
( cd "$B" && sha256sum -c "$(basename "$LATEST").sha256" )

# 2) 解密到临时目录（0600）
#    age 身份私钥由运维离线保管（install.sh 不生成）：
#      age-keygen -o /root/.age/opendiscord.key && chmod 600 /root/.age/opendiscord.key
#    公钥（age1...）写进 OPENDISCORD_AGE_RECIPIENT 后再备份。
sudo install -d -m 0700 /var/tmp/oc-restore
case "$LATEST" in
  *.age)
    sudo age -d -i /root/.age/opendiscord.key -o /var/tmp/oc-restore/chatd.db "$LATEST" ;;
  *.enc)
    sudo openssl enc -d -aes-256-cbc -pbkdf2 -iter 200000 \
      -in "$LATEST" -out /var/tmp/oc-restore/chatd.db -pass file:/etc/opendiscord/backup.key ;;
esac

# 3) 恢复前校验（不通过就停止，不要覆盖生产库）
sudo sqlite3 /var/tmp/oc-restore/chatd.db "PRAGMA integrity_check;"   # 必须 ok
sudo sqlite3 /var/tmp/oc-restore/chatd.db "SELECT count(*) FROM sqlite_master;"  # 表数量合理

# 4) 停服，保留现场（不要直接覆盖唯一副本）
sudo systemctl stop opendiscord
sudo mv /var/lib/opendiscord/chatd.db "/var/lib/opendiscord/chatd.db.bak.$(date -u +%Y%m%dT%H%M%SZ)"
sudo rm -f /var/lib/opendiscord/chatd.db-wal /var/lib/opendiscord/chatd.db-shm

# 5) 落盘（owner/mode 必须与矩阵一致）
sudo install -m 0600 -o opendiscord -g opendiscord /var/tmp/oc-restore/chatd.db /var/lib/opendiscord/chatd.db

# 6) 起服
sudo systemctl start opendiscord
systemctl is-active opendiscord
journalctl -u opendiscord -n 50 --no-pager

# 7) 业务验证（四件事）
sudo sqlite3 /var/lib/opendiscord/chatd.db "PRAGMA quick_check;"          # ok
curl -sI https://chat.example.com/ | head -1                            # 200
adminctl --server http://127.0.0.1:7780 audit --since-id 0 --limit 5 --output json
# 管理后台随机抽 3 个会话，确认最近消息时间点 = 备份时刻

# 8) 清理明文
sudo shred -u /var/tmp/oc-restore/chatd.db 2>/dev/null || sudo rm -f /var/tmp/oc-restore/chatd.db
```

**恢复后必须知道的两件事**：

1. 热库回到备份时刻，**冷库不回滚**。归档游标可能落后于冷库已有数据，先跑
   `opendiscordctl archive --dry-run` / `adminctl archive status` 核对 cutoff，再决定是否重跑归档。
2. 备份时刻之后写入的审计事件会缺失，哈希链会出现断点。这是数据丢失的**预期结果**，
   不是"被篡改"；按 §10.4 记录事件，并在审计报告里说明恢复点（附 manifest 的 `sha256_plain`）。

指标：RPO = 备份间隔（默认 24h，可加密到每小时）；RTO ≈ 10 分钟（2 GB 以内热库）。

---

## 9. 日常运维命令

### 9.1 opendiscordctl（本机运维，直连 systemd/信号/热库）

`opendiscordctl` 不说话给网络端口：它读 systemd 状态、给进程发信号、只读打开热库，因此**必须与
`chatd` 同机、通常用 root 运行**。`install.sh` 不安装 CLI，从发布包或源码装一次：

```bash
# 从源码构建（任何安装方式都适用）
go build -o /usr/local/bin/opendiscordctl ./server/cmd/opendiscordctl
go build -o /usr/local/bin/adminctl    ./server/cmd/adminctl

# 用 release.sh 发布过的机器：current/bin 里已带 CLI，直接 install
sudo install -m 0755 /opt/opendiscord/current/bin/opendiscordctl /usr/local/bin/opendiscordctl
```

| 命令 | 用途 | 关键参数 |
|---|---|---|
| `opendiscordctl status` | 进程状态 + `SIGUSR1` dump 摘要 + 热库统计 | `--unit opendiscord`、`--hot-db /var/lib/opendiscord/chatd.db`、`--signal usr1\|usr2\|none`、`--no-journal`、`--journal-lines 50` |
| `opendiscordctl doctor` | 体检：热库完整性、磁盘余量、近 1h error 数、冷库 TCP 连通 | `--hot-db PATH`、`--unit NAME`、`--since -1h`、`--no-logs`、`--no-cold` |
| `opendiscordctl backup --dry-run` | 只校验备份环境，不写文件 | `--db PATH`、`--script PATH`、`--to DIR` |
| `opendiscordctl backup` | 调 `deploy/backup.sh`；找不到脚本则内置 `sqlite .backup` | 同上 |
| `opendiscordctl archive --dry-run` / `--run` | 经管理 API 触发/预估归档 | 需先 `adminctl login` |
| `opendiscordctl flags list` / `flags set KEY=VALUE` | 读/写 feature flag | `--json` 统一 JSON |
| `opendiscordctl ban <uid> --days 7 --level 2 --reason spam` / `unban <uid>` | 封禁/解封（与管理 API 同一链路） | — |

全局：`--json`、`--admin-url`、`--token`、`--csrf-token`、`--timeout`（默认 20s）。
退出码：`0` 成功 / `1` 错误（doctor 有 FAIL）/ `64` 语法 / `77` 未授权或未登录。
审计链校验没有 CLI 子命令，走管理后台「审计日志 → 校验哈希链」。

### 9.2 adminctl（走管理 API）

> `admin_listen` 默认 `127.0.0.1:7780`，`adminctl` / `opendiscordctl` 的内置默认地址也是
> `http://127.0.0.1:7780`，开箱即用；如需连接其它端口（如经 SSH 隧道），显式传 `--server`
> 或导出环境变量覆盖：
>
> ```bash
> export OPENDISCORD_ADMIN_URL=http://127.0.0.1:7780   # 与 opendiscord.toml 的 admin_listen 一致
> ```
>
> `adminctl` 的路径前缀是 `/api/v1/*`（不经 Caddy 的 `/api/admin/*` 白名单），因此只在
> 本机/内网使用；远程运维先开隧道：`ssh -L 7780:127.0.0.1:7780 ops@chat.example.com`。

```bash
# 登录（交互式密码 + TOTP；凭据默认落 ~/.config/opendiscord/adminctl.json）
adminctl --server http://127.0.0.1:7780 login

# 我是谁 / 当前 token 是否有效
adminctl --server http://127.0.0.1:7780 whoami

# 用户查询与封禁（uid 是位置参数；level 1..3，days=0 永久）
adminctl --server http://127.0.0.1:7780 users list --q alice --limit 20
adminctl --server http://127.0.0.1:7780 users ban 10086 --level 2 --days 7 --reason "spam"
adminctl --server http://127.0.0.1:7780 kick 10086

# 举报队列（status: 0 待处理 / 1 处理中 / 2 已结）
adminctl --server http://127.0.0.1:7780 reports list --status 0 --limit 20
adminctl --server http://127.0.0.1:7780 reports handle 42 --action resolve --note "已警告"

# 审计游标流（since_id 只进不退；JSON 便于 jq）
adminctl --server http://127.0.0.1:7780 audit --since-id 0 --limit 100 --output json

# 归档与开关
adminctl --server http://127.0.0.1:7780 archive status
adminctl --server http://127.0.0.1:7780 archive run --dry-run
adminctl --server http://127.0.0.1:7780 flags list
adminctl --server http://127.0.0.1:7780 flags set registration false
```

全局开关：`--output json|table`、`--token`（env `OPENDISCORD_ADMIN_TOKEN`）、`--csrf-token`、
`--idempotency-key`（写操作 5 分钟内幂等）。退出码：0 ok / 1 error / 64 usage / 65 not found / 77 权限。

### 9.3 journalctl（五条够用）

```bash
journalctl -u opendiscord -f                                   # 实时跟随
journalctl -u opendiscord --since '10 min ago' -p warning      # 近 10 分钟告警
journalctl -u opendiscord -g 'trace_id=01H...'                 # 单条消息全链路
journalctl -u opendiscord -o cat | jq -c 'select(.level=="ERROR")' | tail -20
journalctl --disk-usage && sudo journalctl --vacuum-size=200M   # 配额 200M，满了先查循环重启
```

日志是 JSON lines：顶层 `ts/level/component/msg`，业务字段在 `fields` 里，即
`jq -c 'select(.level=="ERROR") | {ts,component,msg,fields}'`。

### 9.4 systemd 与一次性检查

```bash
systemctl status opendiscord --no-pager -l
systemctl reload opendiscord          # SIGHUP：热载 policy + 白名单（失败保留旧策略）
systemctl restart opendiscord         # 重启：TOML/env 变更后

sqlite3 /var/lib/opendiscord/chatd.db "PRAGMA wal_checkpoint(TRUNCATE);"
sqlite3 /var/lib/opendiscord/chatd.db "PRAGMA quick_check;"
du -sh /var/lib/opendiscord/media /var/lib/opendiscord/backup && df -h /var/lib/opendiscord
ss -lntp | grep -E ':(8080|7780|9090)'
```

---

## 10. 故障排查

### 10.1 起不来（unit 不 active）

按顺序做，每步都有明确判据：

```bash
# 1) 看退出码与重启次数
systemctl status opendiscord --no-pager -l | head -20
systemctl show opendiscord -p ExecMainStatus -p NRestarts -p Result

# 2) 看最后 100 行结构化日志（先找 msg/fields，不要只看 systemd 摘要）
journalctl -u opendiscord -n 100 --no-pager
journalctl -u opendiscord -o cat | jq -c 'select(.level=="ERROR" or .level=="WARN")' | tail -20

# 3) 手工前台跑（绕过 systemd，直接看 stderr；先停 unit 释放端口）
sudo systemctl stop opendiscord
sudo -u opendiscord env $(grep -v '^#' /etc/opendiscord/env | xargs) \
  /opt/opendiscord/current/bin/chatd --config /etc/opendiscord/opendiscord.toml
```

| 现象 | 最可能原因 | 处置 |
|---|---|---|
| `status=1/FAILURE`，日志 `config:` | TOML 语法/引号/policy 非法 | 对照 `deploy/opendiscord.toml` 逐键核对；`journalctl -u opendiscord -o cat \| jq -c 'select(.msg\|test("config"))'` |
| `status=2/FAILURE`，日志 `assembly failed` | 层装配失败（kind 冲突/配置缺项） | 回滚上一版本，保留 `journalctl` 现场 |
| `status=203/EXEC` | 二进制不存在/无执行位 | `ls -l /opt/opendiscord/current/bin/chatd`；确认 `current` 链接 |
| `status=226/NAMESPACE` | `ReadWritePaths` 指向的目录不存在 | `install -d -o opendiscord -g opendiscord /var/lib/opendiscord` |
| `start operation timed out` + 反复重启 | `Type=notify` 未收到 `READY=1` | 见附录 A.1（临时 `Type=exec`） |
| `address already in use` | 旧进程/别的服务占用 8080/7780/9090 | `ss -lntp \| grep :8080`，`systemctl stop` 冲突单元 |
| `permission denied` 打开 DB | 文件 owner/mode 被改（曾用 root 跑过） | 按 §6 恢复：`chown opendiscord:opendiscord`、`chmod 0600` |

### 10.2 连不上（浏览器/客户端）

自内向外排查，五层：

```bash
# L1 DNS + TCP
dig +short chat.example.com
curl -vI https://chat.example.com 2>&1 | head -20

# L2 Caddy 有没有证书/上游错误
sudo journalctl -u caddy -n 100 --no-pager | grep -iE 'error|502|dial'
sudo caddy validate --config /etc/caddy/Caddyfile

# L3 上游是否监听（在目标机执行）
ss -lntp | grep -E ':(8080|7780|9090)'
curl -sI http://127.0.0.1:8080/ | head -1

# L4 防火墙
sudo nft list ruleset | head -40
```

| 症状 | 原因 | 处置 |
|---|---|---|
| 502 Bad Gateway | Caddy 指向的端口没有进程 | 启 `opendiscord`；核对 `OPENDISCORD_WS_PORT`/`listen` 一致 |
| `/admin/` 404，其它正常 | 来源 IP 不在白名单（Caddy 故意 404） | 改 `remote_ip` 后 `systemctl restart caddy` |
| `/admin/` 503 `admin UI is not embedded` | 二进制是未 `make web` 的构建 | 用发布包/重新 `make release` |
| `/ws` 握手 401 | token 放在 query（红线）、Cookie 域不符、access 过期 | token 只放 Cookie 或 `Sec-WebSocket-Protocol`；核对 `public_origin` |
| 登录 429 / 一直失败 | 失败锁定：5 次→15min→1h→24h（§A-1） | 等锁定期；查 `auth.login_failed` 审计；不要绕过限流 |
| 浏览器报 `Unexpected token '<'` | 静态资源 404 被回成 index.html | 确认用的是本仓库新二进制（`web.spa` 对带扩展名路径返回 404） |
| TOTP 全部失败 | 服务器时间漂移 > 30s | `timedatectl status`；开 NTP/chrony |

### 10.3 消息不落库（发得出去、刷新就没了）

按「写管道 → SQLite → 磁盘 → 冷库」顺序查：

```bash
# 1) 写队列是否打满（BUSY 会回 retry_after，不会丢已 ack 消息）
curl -s 127.0.0.1:9090/metrics | grep -E 'writech_queue_depth|writech_busy_total'

# 2) SQLite 是否卡住/损坏
sqlite3 /var/lib/opendiscord/chatd.db "PRAGMA wal_checkpoint(PASSIVE); PRAGMA quick_check;"
ls -lh /var/lib/opendiscord/chatd.db*
journalctl -u opendiscord -o cat | jq -c 'select(.msg|test("write|flush|busy";"i"))' | tail -30

# 3) 磁盘与 inode（WAL 写不进去最常见的根因）
df -h /var/lib/opendiscord; df -i /var/lib/opendiscord

# 4) 文件权限（root 跑过之后 owner 变 root 会写失败）
stat -c '%a %U:%G %n' /var/lib/opendiscord/chatd.db /var/lib/opendiscord

# 5) 冷库归档滞后（只影响历史，不影响新消息落热库）
adminctl --server http://127.0.0.1:7780 archive status
```

| 现象 | 原因 | 处置 |
|---|---|---|
| `writech_queue_depth` 持续 > 64 | 磁盘慢 / 单条写入长事务 | 查 `iostat -x 1`、WAL 大小；降低归档 batch；临时只读 |
| `database is locked` 持续 | 有外部进程直连 SQLite（sqlite3 CLI 长事务/备份） | 结束外部连接；备份用 `.backup`，不要 `cp` 正在写的库 |
| `disk I/O error` / `SQLITE_FULL` | 磁盘满/inode 满/卷只读 | 清 journal、旧备份；扩盘；`mount -o remount,rw` |
| 消息写入成功但刷新消失 | 客户端读的是冷库副本且归档未完成 | 查归档 cutoff；确认 `SYNC` 走热库 |
| `PRAGMA quick_check` 返回非 ok | 页损坏（断电/坏盘） | 立即停服、保全文件、用最近备份恢复（§8.5），不要 `VACUUM` |

**禁止**：`kill -9` 杀 chatd（丢最后一组提交）、`cp` 热库做备份（不一致）、
在服务运行时 `rm -f chatd.db-wal`（等于丢已提交事务）。

### 10.4 审计断链（P0）

触发：管理后台显示「审计链断裂 @#N」、`audit_chain_broken=1`、或审计页「校验哈希链」失败。
这是**红线告警**（security-spec §6.4.2），按事故处理，不要先尝试"修复数据"。

```bash
# 1) 立即冻结写入风险：开全站只读（后台开关或 adminctl）
adminctl --server http://127.0.0.1:7780 flags set readonly true

# 2) 保全现场（顺序很重要：先快照/拷贝，再分析）
sudo systemctl stop opendiscord
sudo cp -a /var/lib/opendiscord/chatd.db /var/lib/opendiscord/chatd.db.incident.$(date -u +%s)
sudo journalctl -u opendiscord -n 2000 --no-pager > /var/log/opendiscord-incident-$(date -u +%s).log

# 3) 定位断点：从 broken_at 往前取 5 条，记录 id/ts/type/prev_hash
#    （走管理 API，不依赖热库表名；N 换成页面显示的断链 id）
adminctl --server http://127.0.0.1:7780 audit --since-id $((N-5)) --limit 11 --output json \
  | jq -c '.events[] | {id,ts,type,prev_hash}'

# 4) 判断性质（三选一，必须写进事故记录）
#    a. 合法恢复：最近做过 §8.5 恢复 → 备份点之后的事件缺失，属预期
#    b. 旁路写入：有人直连 SQLite 改/删了审计表 → 安全事故，升级
#    c. 版本错配：回滚到旧二进制但库已升级 → 核对迁移版本，前向修复

# 5) 上报：安全负责人 + 值班；保留 hash 链证据，禁止重建/清空审计表
# 6) 恢复服务前：重新基线需双人复核并写入事故报告；只读开关关闭前确认链已可验证
```

### 10.5 一键状态 dump（贴给维护者前排敏）

```bash
{
  date -u; uname -a
  systemctl is-active opendiscord; systemctl show opendiscord -p NRestarts -p ExecMainStatus
  adminctl --server http://127.0.0.1:7780 whoami 2>/dev/null || true
  sqlite3 /var/lib/opendiscord/chatd.db "PRAGMA wal_checkpoint(PASSIVE); PRAGMA quick_check;"
  df -h /var/lib/opendiscord; du -sh /var/lib/opendiscord/*
  ss -lntp | grep -E ':(8080|7780|9090)'
  curl -s 127.0.0.1:9090/metrics | grep -E 'writech|ratelimit|audit_chain|go_goroutines'
  journalctl -u opendiscord -n 200 --no-pager
} > /tmp/opendiscord-dump.txt 2>&1
```

外发前删除用户 ID/会话 ID/手机号；**不要**打包 `keys/`、`env`、数据库文件。

---

## 11. 安全基线 checklist（10 条）

部署完成逐项打勾，任何一条不通过 = 未上线。编号引用 [security-spec.md](security-spec.md)。

- [ ] **1. systemd 加固**：`NoNewPrivileges=true`、`ProtectSystem=strict`、`ProtectHome=true`、
      `PrivateTmp/PrivateDevices=true`、`ProtectKernel{Tunables,Modules,Logs,ControlGroups,Clock}=true`、
      `RestrictSUIDSGID/LockPersonality/RestrictNamespaces=true`（§6.1 红线）。
      验证：`systemd-analyze security --offline=true /etc/systemd/system/opendiscord.service`。
- [ ] **2. 非 root 独立用户 + EnvironmentFile 0600**：embedded 用 `opendiscord`（embedded-only）；
      `EnvironmentFile=/etc/opendiscord/env`（§6.1 红线）。
      验证：`ps -o user= -p $(systemctl show -p MainPID --value opendiscord)`。
- [ ] **3. 最小权限边界**：`CapabilityBoundingSet=` 与 `AmbientCapabilities=` 为空；
      `ReadWritePaths` 只含本进程数据目录；配置与二进制 `ReadOnlyPaths`（§6.1 红线）。
      验证：`systemctl cat opendiscord | grep -E 'Capability|ReadWritePaths|ReadOnlyPaths'`。
- [ ] **4. 文件权限矩阵**：§6 表逐项一致；`find /etc/opendiscord /var/lib/opendiscord -perm /o+rwx` 为空
      （§6.2 红线）。验证：§6.1 四条命令。
- [ ] **5. SSH 基线**：`PermitRootLogin no`、`PasswordAuthentication no`、仅 ed25519 公钥、
      `MaxAuthTries 3`、`AllowUsers` 白名单（§6.3 红线）。
      验证：`sshd -T | grep -E 'permitrootlogin|passwordauthentication|maxauthtries'`。
- [ ] **6. 防火墙与时间同步**：默认 DROP，仅放行 22/80/443；出站白名单；NTP 已启用
      （§6.3 红线，TOTP/JWT 时间窗依赖）。验证：`nft list ruleset`、`timedatectl status`。
- [ ] **7. 管理面三重门**：强制 TOTP（6 位/30s/±1，10 次失败锁 30min，A-4 红线）、
      `/api/admin/*` IP 白名单（非白名单 404）、所有非 GET 强制 CSRF（§3.3 红线）。
      验证：白名单外 `curl -o /dev/null -w '%{http_code}' https://域名/api/admin/v1/whoami` 应为 404。
- [ ] **8. 边缘限流与体积上限**：WS upgrade ≤ 10 次/分钟/IP + 失败熔断（E-3/E-4 红线）；
      非上传路径请求体 ≤ 1 MB、预签名申请 ≤ 64 KB（E-7 红线）。
      验证：`curl --data-binary @2MB.bin https://域名/api/v1/auth/login` 应 413。
- [ ] **9. 密钥清单与轮换**：§6.4.1 清单齐全、`kid` 支持双钥轮换、私钥不入库不进镜像、
      `backup.key`/`db_disk_key` 离线保管（§6.4.1 红线）。
      验证：`ls -l /etc/opendiscord/keys`；`git grep -n 'BEGIN PRIVATE KEY'` 应为空。
- [ ] **10. 备份与审计**：备份加密 + 异地 + 至少一次成功恢复演练（§8.5）；审计链每日校验，
      断裂按 P0 处置（§6.4.2 红线）。验证：`systemctl list-timers opendiscord-backup.timer`；
      后台审计页「校验哈希链」返回 ok。

**密钥轮换日历**（§6.4.1）：`jwt_sign_key` 180 天、`admin_jwt_key` 90 天、`ipc_key`/`hmac_key` 90 天、
`pii_key` 180 天（需重加密迁移）、`phone_lookup_key` 365 天（需重建索引）、
`download_key`/`upload_secret` 90 天、`audit_chain_key` 365 天（换盐=全链重验）、
`backup_key` 180 天（旧备份作废重做）。

---

## 附录 A：骨架阶段差异（联调必读）

部署件按**目标形态**编写；当前仓库的 `chatd` 已具备 1.0 装配（管理 REST、冷库与归档作业、媒体上传/下载、
内嵌 SPA 均已接线）。下表只列仍会被实测到、且会让"照文档做却起不来"的差异，逐条给绕过方式：

| # | 差异 | 现象 | 临时绕过 / 上线前要求 |
|---|---|---|---|
| A.1 | 未实现 `sd_notify(READY=1)`，但 unit 是 `Type=notify` + `WatchdogSec=15` | `start operation timed out`，`Restart=always` 反复重启 | **仅联调**：`sudo systemctl edit --full opendiscord`，把 `Type=notify` 改 `Type=exec`、`WatchdogSec=15` 改 `WatchdogSec=0`；**上线前必须恢复**并实现 notify + watchdog 心跳 |
| A.2 | Prometheus `/metrics` 与 `metrics_listen` 未实现（管理 API 只有 `/api/v1/metrics` 摘要） | Caddy `/metrics` 对白名单内来源 502、对公网 404 | 先从管理 API 取指标；上线前接入 Prometheus handler 并监听 9090 |
| A.3 | `install.sh` 只安装 `chatd`，不安装 `adminctl` / `opendiscordctl` | `command not found` | 按 §9.1 从 `current/bin` 或源码 `install` 到 `/usr/local/bin` |
| A.4 | 管理 REST 已挂载：`/api/v1/*` 在 `admin_rest_listen`（默认 `127.0.0.1:7781`） | 启动日志出现 `rest_layer="admin_rest_listen"`，管理 API 由该监听提供（不再有 `not_wired`） | 无需绕过；本机/内网按 §4.4 验证。注意 REST 端口与 `admin_listen`（7780，帧注入）是两个监听 |
| A.5 | `deploy/opendiscord.toml` 中部分契约字段当前二进制未消费 | 改了 `metrics_listen`/上传上限等无效果 | 见 §3.1 表后的「消费进度」注记 |
| A.6 | SPA 已内嵌（`server/web`，`go:embed`），`chatd` 在 `listen` 为 TCP 时同时托管 `/` 与 `/admin/` | 发布包无需额外 web 目录；从源码构建若未执行 `make web`，前端资源为空，访问返回 503 `UI is not embedded` | 用发布包，或 `make web && make release`；不要手改 `server/web/*`，它由 `make web` 整目录替换 |
| A.7 | 冷库与归档作业已接线（jobber 按 `policy.archive.cron` 调度），但 Postgres driver 需部署层注册 | `cold_dsn` 已配置但 driver 未注册时，启动日志报 `cold store disabled: driver not registered`；归档作业仅 dry-run/报错，审计降级为「只写日志不落盘」 | 部署层 import `github.com/lib/pq`（注册 `postgres` driver，匹配 `sql.Open("postgres", …)`）或注册同名 driver 的 pgx 包装（二进制按架构约定不携带 driver）；未注册前不要开启归档，审计按 security-spec §5.5 补接 |
| A.8 | 媒体上传/下载已提供（MVP，无会话 ACL） | `POST /api/media/upload` 与 `GET /api/media/{id}` 可用；下载只校验登录态 + 不可枚举 `media_id`，无会话级 ACL，无异步病毒扫描 | 1.0 可用于可信小规模部署，勿把 `media_id` 外传给非会话成员；1.1 补会话 ACL 与 ClamAV |

**发布前必须关闭的临时措施**：A.1 的 `Type=exec`/`WatchdogSec=0`（改回 notify 前先确认
`READY=1` 与 `WATCHDOG=1` 已实现）。A.7 的 driver 注册与审计落盘、A.8 的媒体会话 ACL 属于生产
安全基线，上线前必须补齐或明确接受风险；其余项只影响功能完整性。

---

## 附录 B：目录速查

```
/opt/opendiscord/current/bin/chatd                 # 单二进制（含内嵌前端）
/opt/opendiscord/current/{deploy,web}              # release.sh 打包时附带
/etc/opendiscord/{opendiscord.toml,policy.json,env,Caddyfile.reference}
/etc/opendiscord/keys/{*.pem,*_key,backup.key}
/etc/opendiscord/config/chatd.toml -> ../opendiscord.toml
/var/lib/opendiscord/{chatd.db,admin.db,media/,backup/}
/etc/systemd/system/opendiscord*.service
/etc/systemd/journald.conf.d/opendiscord.conf     # SystemMaxUse=200M
/usr/local/sbin/opendiscord-backup                 # §8.2 手动安装
```
