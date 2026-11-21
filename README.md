# OpenDiscord

**自托管即时通讯：消息与文件留在你自己的服务器上，由你掌控数据。**

单聊、群聊、图片/语音/文件、WebRTC 语音视频通话、多设备同步。一套发布包提供服务端、
用户端界面与管理后台，2 核 2G 即可运行，不依赖容器编排。
（注：通话信令经本服务器转发；系统推送经浏览器推送服务中转；自托管不等于所有流量都不出网。）

- **给你自己用/小团队用**：按文档快速部署 → [快速开始](#快速开始)
- **要部署到生产**：平台/产物/升级 → [部署与升级](#部署与升级)
- **想读代码或参与开发**：架构与协议 → [面向开发者](#面向开发者)

> **商标声明**：OpenDiscord 是独立开发的自托管即时通讯项目，与 Discord Inc. 及 Discord 品牌
> **没有任何关联、背书或授权关系**。"Discord" 及相关标识是其权利人的商标；本项目名称中的
> "Discord" 字样仅用于描述性目的，不主张任何商标权利。如权利人提出异议，我们将配合调整名称。

当前版本 **v2.0.0**（架构重设第一步：容量契约 + 写吞吐翻倍，正式版）。容量有明确边界
（2 核约 512 并发 / ~3,500 msg/s 工作点，见[实测容量](#容量实测非估算)）；v2.0 把过载行为
改为**显式拒绝**（超限立即失败，不再静默挂起）、修复"ACK 撒谎"，并将消息写入吞吐提升约
一倍——但单进程 SQLite 的物理天花板仍在，容量门槛未达标，详见
[发布说明 v2.0.0](docs/release-notes-v2.0.0.md) 与 [架构重设路线](docs/v2.0-roadmap.md)。
移动端系统推送与消息举报入口已于 v1.5 实现。本文档如实标注，不夸大、不隐藏。

## 为什么选它

- **数据自主**：热数据留在本机 SQLite（默认 90 天），更早归档到外部 PostgreSQL；
  备份、归档、清理都是独立作业，可 dry-run 预演；审计日志哈希链 + append-only，篡改可被检测。
- **运维友好**：一个二进制部署，Web 管理后台 + 终端管理台（`opchat`）+ 可编排的
  命令行（`opendiscordctl` / `adminctl`），退出码可脚本化。
- **性能有实测**：不做估算——所有容量数字来自 CI 压测，方法与复现脚本都在仓库里
  （见[实测容量](#容量实测非估算)），包括明确的拐点和不适用的场景。
- **说清楚它不是什么**：不是 Slack 替代品，不适合超大群（2 核上 1000 人大群不可行）；
  要更高容量需加核或多实例，架构级重设在 [docs/architecture-reset-v2-plan.md](docs/architecture-reset-v2-plan.md) 规划中。

## 特性

**通讯**
- 单聊与群聊，消息已读/送达回执、输入中提示、在线状态
- 2 分钟内可撤回自己发的消息（管理员可撤回他人）
- 离线消息自动补拉，断网重连后消息不丢不乱序
- 多设备同时在线（默认上限 3 台），消息全端同步

**多媒体**
- 图片、语音、文件消息；缩略图与语音波形在浏览器本地生成，服务端不做转码
- WebRTC 一对一语音/视频通话（媒体流点对点直连，服务器只转发信令）
- 媒体存储带配额（默认单用户 500MB、全局 20GB），缩略图在文件系统而非数据库

**账号**
- 手机验证码、邮箱魔法链接、密码、游客模式
- 登录失败指数退避、验证码发送限流、多设备管理
- 管理端强制 TOTP 二步验证

**管理与运维**
- Web 管理后台：仪表盘（实时指标 + 趋势图）、用户/举报/广播/开关/归档/审计
- `opchat`：纯终端管理台，7 个标签页，适合 SSH 里快速处理
- `opendiscordctl` / `adminctl`：脚本化命令行，退出码可编排
- 全中英双语，界面语言自动跟随系统

## 容量（实测，非估算）

> **1.4 数据待 S-1~S-7 压测刷新**：1.4 的普遍拉取 + 自适应 + v3 总线代码已合入
> 但全部 flag 默认关闭，下表数字未受影响；flag 灰度启用后由 CI 压测刷新本表，
> 当前请继续按表中实测值做容量规划。
>
> **v1.4 知情发布的容量缺口**：v1.4 首轮双规格压测 PASS=0 / FAIL=6 / BLOCKED=8，
> S-1（500 人群×1000 在线，实测 0.93%，目标 95%）与 S-2（1000×50 人群，实测 16.38%，
> 目标 90%）未达标，属单进程 SQLite 架构的物理包络。详见
> [docs/release-notes-v1.4.0.md](docs/release-notes-v1.4.0.md) 与 [docs/known-issues.md](docs/known-issues.md)。

### 三列对照：v1.1.1 基线 → 负优化版（已回滚）→ 7cb04fc（当前）

CI 2 核 runner、无限流、20 秒稳态、mixed 负载。7cb04fc = 发送路径回退到 v1.1.1 形态后的提交（见 `docs/perf-postmortem-v1.2-draft.md`）：

| 场景 | v1.1.1 基线 | 负优化版（已回滚） | 7cb04fc（当前） |
|---|---|---|---|
| 512 连接 × 10 人群 | 98.1% | 73.0% | **91.7%**（p95 120ms） |
| 1000 连接 × 50 人群 | 32.3% | 14.6% | **23.7%**（p95 468ms） |
| 1000 连接 × 500 人群 | 2.2% | 0.9% | 2.0% |

| 并发 WS 连接 | 消息/秒 | 全局 RPM | 可用性(ACK/发送) | 端到端 p50/p95 |
|---|---|---|---|---|
| 256 | ~1,790 | 107,520 | 99.6% | 65 / 85 ms |
| **512** | **~3,535** | **212,130** | **98.1%** | **80 / 126 ms** |
| 1024 | ~6,950 | 417,114 | 65.9% | 1,260 / 1,410 ms |

**结论：2 核机器上稳定的工作点是 ~512 并发连接 / ~3,500 消息每秒。** 1024 连接时可用性掉到 66%，这是本机型的实际拐点。

容量由**扇出写放大**决定，不是消息本身：一条消息要写给 N 个成员。50 人群是 10 人群的 5 倍写入，实测可用性从 100% 掉到 38%。要更高容量：加核、或把大群拆成多实例。

### 1000 人规模实测（v1.1.1 基线；更大群 = 更重扇出）

固定 1000 并发连接，只改群成员数（无限流、20 秒稳态）。下表为 v1.1.1 基线；负优化版与 7cb04fc（当前）的三列对照见上文：

| 群成员数 | 群数量 | 消息/秒(尝试) | 消息/秒(成功) | 可用性 | p50/p95 |
|---|---|---|---|---|---|
| 50 | 20 | ~6,790 | ~2,196 | **32.3%** | 212 / 350 ms |
| 500 | 2 | ~6,760 | ~150 | **2.2%** | 1,642 / 4,227 ms |

两个关键结论：

1. **1000 个并发连接本身没问题**——两次测试的 WS 握手都是 1000/1000 成功，连接建立延迟 <100 ms。
2. **决定容量的是「消息数 × 群成员数」= 帧扇出总量**，2 核机器的上限约 **40,000 帧/秒**：

   | 场景 | 帧/秒 | 可用性 |
   |---|---|---|
   | 512 连接 × 10 人群 | ~35,000 | 98% |
   | 1000 连接 × 50 人群 | ~110,000 | 32% |
   | 1000 连接 × 500 人群 | ~1,000,000（需求） | 2% |

   超上限后不是崩溃，是排队变长 + 服务端 NACK（`srv_drop` 列）。

**实践建议（按群规模选核数）**：
- 2 核：群成员数 ≤50 且消息频率 ≤1 条/秒/人（512 连接 × 10 人群可用性 91.7%，1000 连接 × 50 人群 23.7%）。
- 8 核以上：帧扇出上限按核数近似线性扩展（2 核 ≈ 40k 帧/秒），1000 人大群在此档位才可行。
- 超大群正解是 v1.2 的拉取模式（消息不扇出、客户端按 seq 拉取）；LAGGING 降级与真合批两条优化路线已被压测证伪并回滚，勿再尝试（见 `docs/perf-postmortem-v1.2-draft.md`）。

其他实测基线：1000 并发连接握手 1000/1000 成功（多轮一致），连接建立 <100 ms；帧扇出上限 ~40,000 帧/秒（2 核）；写路径上限 ~5-6k 消息/秒。

复现：`scripts/stress/main.go`（`--conv-members` 设扇出宽度，`-ladder start=256` 自动倍增到可用性 <70%）。

## 快速开始

1. 到 [Releases](https://github.com/yingo-server/OpenDiscord/releases) 下载对应平台的产物（见下表，共 9 个）。
2. `sudo ./deploy/install-interactive` 交互式安装（无 TTY 时自动降级为参数模式）。
3. `sudo systemctl enable --now opendiscord` 启动，打开 `https://你的域名` 即可使用。

### 支持的平台

| 平台 | 架构 | 产物 |
|---|---|---|
| Linux | x86_64 / x86 / arm64 / armv7 / riscv64 | `.tar.gz`（静态链接 SQLite） |
| Windows | x86_64 / x86 | `.zip` |
| 源码 | — | `opendiscord-<ver>-src.tar.gz` |

Windows on ARM / ARM64 暂无产物（缺 mingw CGO 工具链），见 [已知问题](docs/known-issues.md#3-windows-arm--arm64-不提供构建产物)。

### 要求

- Linux 服务器（2 核 2G 起），systemd
- 域名（TLS 由 Caddy 自动签发）
- PostgreSQL（冷存储，可选；不配则只保留本机热数据）

### 安装

```bash
# 1. 下载（版本号见 https://github.com/yingo-server/OpenDiscord/releases）
V=1.4.0
curl -LO https://github.com/yingo-server/OpenDiscord/releases/download/v$V/opendiscord-v$V-linux-amd64.tar.gz
curl -LO https://github.com/yingo-server/OpenDiscord/releases/download/v$V/SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing
tar -xzf opendiscord-v$V-linux-amd64.tar.gz

# 2. 安装（交互式向导；无 TTY 时自动降级为参数模式）
sudo ./deploy/install-interactive

# 或者非交互：
# sudo ./deploy/install.sh --domain https://chat.example.com \
#      --admin-user admin --admin-pass-file /root/admin.pass --lang zh

# 3. 启动
sudo systemctl enable --now opendiscord
```

首次启动会创建管理员账号并打印一次性的
TOTP 密钥（用 `journalctl -u opendiscord | grep otpauth` 查看），请立即在认证器里登记。

## 部署与升级

安装细节、Caddy 配置、备份恢复、故障排查与安全基线见 [部署手册](docs/deploy.md)。

### 升级

```bash
# 备份 → 替换二进制 → 重启（详见 docs/deploy.md §7）
sudo opendiscordctl backup
sudo systemctl stop opendiscord
# 替换 releases/<ver> 下的二进制并切换 current 符号链接
sudo systemctl start opendiscord
sudo opendiscordctl doctor
```

> **从 v1.0 升级到 v1.1 必须做一次热库转换**（启用 auto_vacuum，否则磁盘只涨不落），
> 命令见 `docs/release-notes-v1.1.0.md` 与 `docs/deploy.md §7.4`。

### 常用运维

| 命令 | 用途 |
|---|---|
| `opendiscordctl status` | 服务状态、进程信息、热库统计 |
| `opendiscordctl doctor` | 体检：数据库完整性、磁盘空间、日志错误、冷库连通、审计存储 |
| `opendiscordctl backup [--dry-run]` | 备份热库与配置 |
| `opendiscordctl ban <uid> --days 7` | 封禁用户 |
| `opendiscordctl unban <uid>` | 解封 |
| `opendiscordctl flags list` / `flags set k=v` | 查看/修改功能开关 |
| `opendiscordctl archive --dry-run` / `--run` | 归档预演 / 执行 |
| `opendiscordctl migrate thumbs [--dry-run]` | 把旧版存在数据库里的缩略图迁到文件系统 |
| `opchat` | 打开终端管理台（交互式） |
| `adminctl users list --q 张` | 检索用户（脚本化） |
| `adminctl broadcast send --text "..."` | 全站广播 |

日志：`journalctl -u opendiscord -f`　　Dump 内部状态：`kill -USR1 $(pidof opendiscord)`

日常运维、内容治理与应急处理见 [管理员指南](docs/admin-guide.md)；日常使用（账号、消息、隐私、数据导出）见 [用户指南](docs/user-guide.md)。

## 面向开发者

- [开发文档索引](docs/README.md)：架构、协议、各层设计（从这里进入）
- [架构](docs/architecture.md) / [API 参考](docs/api-reference.md) / [CLI 参考](docs/cli-reference.md) / [WS 协议](docs/ws-protocol.md)
- [压测方法与基准](docs/perf-spec-v1.2.md)：本文容量数据的测量方法
- 贡献流程与构建方式见开发文档索引；变更日志见 [CHANGELOG.md](CHANGELOG.md)

## 文档

面向使用者：
- [用户指南](docs/user-guide.md)：账号、消息、隐私、数据导出与注销
- [管理员指南](docs/admin-guide.md)：日常运维、内容治理、应急处理

面向部署者：
- [部署手册](docs/deploy.md)：安装、配置、Caddy、备份恢复、故障排查、安全基线
- [升级说明 v1.1](docs/release-notes-v1.1.0.md)：升级步骤、配置变更、回滚
- [发布说明 v2.0.0](docs/release-notes-v2.0.0.md)：容量契约 + ACK 诚实 + 写吞吐翻倍（诚实数据）
- [发布说明 v1.5.0](docs/release-notes-v1.5.0.md)：文案/文档/功能补齐
- [发布说明 v1.4.0](docs/release-notes-v1.4.0.md)：生产化转默认 + 已知容量限制（知情发布）

面向开发者：
- [开发文档索引](docs/README.md)：架构、协议、各层设计
- [API 参考](docs/api-reference.md) / [CLI 参考](docs/cli-reference.md) / [WS 协议](docs/ws-protocol.md)
- [路线图](docs/roadmap.md) / [变更日志](CHANGELOG.md) / [已知问题](docs/known-issues.md)
- [1.4 LTS 规划](docs/lts-1.4-plan.md)：普遍拉取 + 自适应 + v3 总线的决策与验收门槛
- [v1.5 路线图](docs/v1.5-roadmap.md)：推送/举报等功能补齐与发布计划
- [v2.0 架构重设规划（草案）](docs/architecture-reset-v2-plan.md)：容量瓶颈归因与分片/异步写/会话 Actor 路线

## 许可

[Apache-2.0](LICENSE)
