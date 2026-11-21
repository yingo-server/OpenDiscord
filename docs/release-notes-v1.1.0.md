# UFOT v1.1.0 发布说明

> 面向从 v1.0.0 升级的运维与集成方。
> 版本：v1.1.0（2026-11-05）｜上一版本：v1.0.0
> 完整的逐条变更见仓库根目录 `CHANGELOG.md` 的 `[1.1.0]` 段。

---

## 1. 这版带来了什么（用户视角）

- **消息真正闭环**：发出的消息能看到"已送达 / 已读"，对方正在输入会实时提示，对方上线/下线也会显示。v1.0 这些只是协议占位，v1.1 才真正端到端打通。
- **群成员变动实时可见**：有人被拉进群、被踢出、被改角色，群里所有人立刻收到通知，不用刷新。
- **消息可撤回**：发出 2 分钟内、或作为管理员，可撤回消息；软删除落库，所有端（含换设备后拉历史）都会显示"已撤回"，不再原文重现。
- **图片更省流量、语音能拖动**：上传图片自动带缩略图、语音自动带波形，且缩略图/波形在客户端本地生成——服务端不需要装 ffmpeg，小内存机器（2C2G）也扛得住。
- **1v1 语音/视频通话**：支持 WebRTC 点对点通话，服务端只转发信令、不走媒体流（零带宽）；有忙线自动拒接、60 秒无人接自动挂断，来电全屏 UI。
- **管理员可上终端操作**：`chatd --tui` 提供运行仪表盘；`opchat` 提供纯文本管理控制台（7 个 tab，覆盖用户/举报/广播/Flags/归档/审计）。
- **管理后台 WebUI 更直观**：Dashboard 首页有指标卡、实时事件流、sparkline 趋势图，数据变化带滚动动画，轮询可随时暂停。
- **安装向导更友好**：`install.sh` 支持一条命令配好域名/管理员/冷库/TURN/语言；交互式安装是 7 步向导，中英双语。
- **更安全**：修掉了冒用身份撤回、伪造在线状态、生产限流可被关掉、缩略图可塞任意内容等 4 类安全问题。
- **更多平台**：发布覆盖 Linux x86_64/x86/arm64/armv7/riscv64 与 Windows x86_64/x86/arm64/arm，另有源码包。

---

## 2. 升级步骤

> 以下命令按默认安装位置（`/etc/ufot`、`/var/lib/ufot`、systemd 单元 `ufot`）书写，请按实际部署调整路径。
> **升级前务必先停机备份**，升级不可逆（见第 6 节回滚）。

**① 备份数据与配置**

```sh
# 停机前先跑一次一致性备份（热库 + 管理库 + 配置）
sudo bash deploy/backup.sh
# 或直接用运维 CLI：
sudo ufotctl backup

# 额外留一份配置与数据目录快照，便于回滚
sudo cp -a /etc/ufot /etc/ufot.bak-$(date +%F)
sudo cp -a /var/lib/ufot /var/lib/ufot.bak-$(date +%F)
```

**② 停止服务**

```sh
sudo systemctl stop ufot
```

**③ 替换二进制**

```sh
# 方式 A：解压官方发布包，覆盖二进制
tar -xzf ufot-v1.1.0-linux-$(uname -m | sed 's/x86_64/x86_64/').tar.gz -C /tmp/ufot-new
sudo install -m 0755 /tmp/ufot-new/chatd          /usr/local/bin/chatd
sudo install -m 0755 /tmp/ufot-new/ufotctl     /usr/local/bin/ufotctl

# 方式 B：直接重跑安装器（会自动装好二进制、单元、目录权限、管理员）
sudo bash deploy/install.sh \
  --domain https://chat.example.com \
  --admin-user admin \
  --admin-pass-file /root/admin.pass \
  --lang zh
```

**④ 检查/新增配置项**

- 确认 `public_origin` 已填（`--domain` 会写入，见第 3 节）；`install.sh --domain` 在 v1.1.0 才真正生效，v1.0 时代它被丢弃。
- 若启用冷库，配置 `cold_dsn` 并确保部署层已注册 postgres driver（见第 5 节）。
- 若要开放通话，把 feature flag `call_enabled` 置为 `true`（默认 false）。

```sh
sudo systemctl reload ufot   # SIGHUP：重读 policy.json 与白名单
```

**⑤ 启动并验证**

```sh
sudo systemctl restart ufot
sudo systemctl status ufot --no-pager

# 健康检查
curl -fsS http://127.0.0.1:8080/healthz && echo OK
# 版本与会诊
ufotctl version
ufotctl doctor
# 查看当前 feature flags（确认 call_enabled 等符合预期）
ufotctl flags list
```

---

## 3. 配置变更清单

> 说明（据实）：v1.0.0 → v1.1.0 之间，`config/chatd.toml` 与 `deploy/policy.json` 两个文件本身**没有新增 key**（两者 diff 为空）。本节的"变更"指**语义/校验行为发生变化**的既有 key，以及 `install.sh` 新增参数映射到的 key。

| key | 位置 | 默认值 | 是否必填 | v1.1.0 影响 |
|---|---|---|---|---|
| `public_origin` | `config/chatd.toml` | `""`（空） | 生产环境建议必填 | 空值时 CORS 同源校验与 Secure Cookie 判定走宽松分支。v1.0 的 `install.sh --domain` 被丢弃常导致此处为空；v1.1 起 `--domain` 会真正写入本项。 |
| `rate_limit_enabled` | chatd 配置（`config.go` 解析；样例 toml 未列出该行） | 代码默认 `true`（结构零值为 `false`） | **生产必须为 true** | v1.1 起生产环境检测到 `=false` 直接拒绝启动（此前零值 false 会静默关掉全部限流）。非生产或压测才可置 false。 |
| `cold_dsn` | `config/chatd.toml` | `""` | 可选 | 设置为非空后需要部署层注册 postgres driver；未注册时启动日志告警、冷库句柄为 nil（审计退化为只写日志）。`install.sh --cold-dsn` 会写入。 |
| `[feature_flags] call_enabled` | `config/chatd.toml` | `false` | 可选 | v1.1 起为真实开关：`false` 时所有通话信令返回 403，`true` 才允许 350–354 通话。 |

**`deploy/policy.json`（策略真源，SIGHUP 热加载；本次 key 未变，列出相关阈值供核对）**

| key | 默认值 | 是否必填 | 说明 |
|---|---|---|---|
| `rate_limit.msg_per_user_per_min` | `60` | 可选 | 每用户每分钟消息上限；v1.1 修了"生产可整体关掉限流"的漏洞后此阈值按配置生效。 |
| `rate_limit.login_per_ip_per_min` | `10` | 可选 | 每 IP 每分钟登录尝试上限。 |
| `hot_retention_days` | `90` | 可选 | 热库保留天数，超过进归档。 |
| `archive.cron` / `backup.cron` / `trim[]` | 见文件 | 可选 | 归档/备份/裁剪调度，未变。 |

**`install.sh` 新增命令行参数（非配置文件 key，仅用于生成配置）**

| 参数 | 对应落地 | 说明 |
|---|---|---|
| `--domain URL` | 写入 `public_origin` | 必须带 scheme；v1.1 修复了此前参数被丢弃的 bug。 |
| `--admin-user U` / `--admin-pass-file PATH` | 首启管理员引导（写入 `/etc/ufot/env`，0600） | 不使用命令行明文密码。 |
| `--cold-dsn DSN` | 写入 env（`UFOT_COLD_DSN`） | 可选。 |
| `--turn-server U` | 写入配置注释（`deploy/coturn` 模板） | 通话需自备 TURN，未配凭据前不生效。 |
| `--lang zh\|en` | 输出语言 | 默认跟随 `LANG`/`LC_ALL`。 |

---

## 4. 破坏性变更

据实：**无破坏性 API/协议变更**。既有 kind、既有配置 key、既有安装路径均保持兼容。

需注意的行为收紧（非破坏性，但可能影响非标准部署）：

- **生产环境不再允许 `rate_limit_enabled=false`**：若你的生产配置显式关了限流，升级后 `chatd` 会拒启。请在 `policy.json`/配置中恢复限流或将环境判定改为非生产。
- **客户端上行 401/402 presence 事件被拒绝**（返回 4003）：自研客户端若曾自行发送 presence 帧，需改为依赖服务端广播。
- **缩略图/波形上传内容做 magic bytes 校验**：不合规的 `thumb_data`（如 SVG/HTML）会被拒。

---

## 5. 已知问题与规避

- **超大群广播扇出放大**：会话成员超过 ~500 人时，广播（回执/在线/成员变更）扇出走量明显。规避：超大群暂控在读多写少场景；v1.2 引入聚合窗口。
- **冷库 driver 需部署层注册**：`chatd` 按架构约定不内嵌 postgres driver。若 `cold_dsn` 非空但部署层未注册，冷库为 nil、审计只写日志不落盘。规避：按 `docs/deploy.md` 在部署层 import `lib/pq`/`pgx` 注册 driver，或部署 `require_audit_store=true` 让缺冷库时显式报错。
- **短信/邮件/图形验证码供应商未接入**：接口已留，`dev_otp=true` + `APP_ENV=dev` 可跑通全链路；生产短信/邮件不会真正发出，验证码 fail-closed。规避：生产暂用密码/游客登录，或接入自有供应商适配器。
- **split 多进程部署未实现**：systemd 单元为 v1.2 预留，当前只支持 embedded 单进程。规避：v1.1 一律用 embedded 模式部署。
- **移动端推送未做**（计划 v1.2）。
- **通话需自备 TURN 服务器**：P2P 握手在对称 NAT 下需要 TURN 中继。已提供 `deploy/coturn` 配置模板，需自行部署 coturn 并在客户端/安装参数中配置。

---

## 6. 回滚方式

升级不可逆（热库/迁移可能已前进），回滚走"恢复备份"：

```sh
# 1) 停服
sudo systemctl stop ufot

# 2) 还原二进制
sudo install -m 0755 /path/to/v1.0.0/chatd        /usr/local/bin/chatd
sudo install -m 0755 /path/to/v1.0.0/ufotctl   /usr/local/bin/ufotctl

# 3) 还原配置与数据（使用第 2 步制作的快照）
sudo rm -rf /etc/ufot /var/lib/ufot
sudo cp -a /etc/ufot.bak-YYYY-MM-DD   /etc/ufot
sudo cp -a /var/lib/ufot.bak-YYYY-MM-DD /var/lib/ufot

# 4) 起服并验证
sudo systemctl start ufot
ufotctl version   # 应显示 v1.0.0
curl -fsS http://127.0.0.1:8080/healthz && echo OK
```

> 提示：回滚前先用 `ufotctl backup` / `deploy/backup.sh` 再抓一份当前状态，避免丢失升级后产生的新数据。
