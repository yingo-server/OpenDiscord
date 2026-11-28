<p align="center"><img src="brand/ufot-wordmark.svg" alt="UFOT — Unfettered Freedom Of Thought" width="320"></p>

# UFOT

> *They demanded unfettered freedom of thought.*
> 他们要求不受束缚的思想自由。

**自托管即时通讯后端：消息与文件留在你自己的服务器上，由你掌控数据。**

**纯后端仓库**——单聊、群聊、图片/语音/文件、多设备同步、Web Push、管理面
（REST + RBAC + 审计哈希链）全部由一个 Go 二进制提供服务；2 核 2G 即可运行，
不依赖容器编排。**客户端（Web / 桌面 / 移动）是独立程序**，不在本仓库：
它通过 env（API base / WS url）指向本服务，接入契约见
[docs/client-contract.md](docs/client-contract.md)。
（注：通话信令经本服务器转发；系统推送经浏览器推送服务中转；自托管不等于所有流量都不出网。）

- **要部署服务**：[快速开始](#快速开始) → [部署手册](docs/deploy.md)
- **要接入一个客户端**：[客户端契约](docs/client-contract.md)
- **想读代码或参与开发**：架构与协议 → [面向开发者](#面向开发者)

当前版本 **v2.1.0**。v2.1 起前端代码全部移出仓库（架构决策见
[docs/architecture.md](docs/architecture.md) 的 ADR）；跨域客户端由 CORS 支持
（默认 `allow_any`）。容量有明确边界（2 核约 512 并发 / ~3,500 msg/s 工作点，
见[实测容量](#容量实测非估算)）；过载行为为**显式拒绝**（超限立即失败，不再静默挂起）。
单进程 SQLite 的物理天花板仍在：S-1（500 人群×1000 在线）与 S-2（1000×50 人群）
目标未达，如实保留，详见 [docs/known-issues.md](docs/known-issues.md)。
本文档如实标注，不夸大、不隐藏。

## 为什么选它

- **数据自主**：热数据留在本机 SQLite（默认 90 天），更早归档到外部 PostgreSQL；
  备份、归档、清理都是独立作业，可 dry-run 预演；审计日志哈希链 + append-only，篡改可被检测。
- **运维友好**：一个二进制部署，终端管理台（`opchat`）+ 可编排的命令行
  （`ufotctl` / `adminctl`），退出码可脚本化。
- **客户端自由**：后端不托管 UI、不绑定任何前端技术栈；HTTP + WS 契约字段级精确
  （[client-contract.md](docs/client-contract.md)），自研/第三方客户端平等接入。
- **性能有实测**：不做估算——所有容量数字来自 CI 压测，方法与复现脚本都在仓库里
  （见[实测容量](#容量实测非估算)），包括明确的拐点和不适用的场景。
- **说清楚它不是什么**：不是 Slack 替代品，不适合超大群（2 核上 1000 人大群不可行）；
  要更高容量需加核或多实例，架构级重设在 [docs/architecture-reset-v2-plan.md](docs/architecture-reset-v2-plan.md) 规划中。

## 特性（服务端）

**通讯**
- 单聊与群聊，消息已读/送达回执、输入中提示、在线状态（WS 帧协议见 [docs/ws-protocol.md](docs/ws-protocol.md)）
- 消息编辑、撤回、删除、转发、回复、@ 提及（v2 消息域，flag 门控）
- 离线消息自动补拉（水位线 + SYNC），断网重连后消息不丢不乱序
- 多设备同时在线，消息全端同步

**多媒体**
- 图片、语音、文件消息（HTTP 直传 `/api/media/*`：图片 8MB / 音频 2MB / 文件 20MB，魔数白名单）
- WebRTC 一对一语音/视频通话信令（flag 门控，默认关闭；媒体流点对点直连，服务器只转发信令）
- 媒体存储带配额（默认单用户 500MB、全局 20GB）

**账号**
- 手机验证码、邮箱魔法链接、密码、游客模式（四种登录，`GET /api/auth/methods` 探测开放状态）
- 登录失败指数退避、验证码发送限流、多设备管理
- 管理端强制 TOTP 二步验证
- Web Push 系统推送（VAPID；未配置时静默降级）

**管理与运维**
- 管理面 REST `/api/v1/*`：用户/举报/广播/开关/归档/审计/指标（RBAC + CSRF + 幂等）
- `opchat`：纯终端管理台，适合 SSH 里快速处理
- `ufotctl` / `adminctl`：脚本化命令行，退出码可编排

## 容量（实测，非估算）

> **容量缺口如实保留**：S-1（500 人群×1000 在线，实测 0.93%，目标 95%）与 S-2
> （1000×50 人群，实测 16.38%，目标 90%）未达标，属单进程 SQLite 架构的物理包络。
> 详见 [docs/known-issues.md](docs/known-issues.md) 与 [docs/release-notes-v1.4.0.md](docs/release-notes-v1.4.0.md)。

### 工作点（2 核 CI 实测，mixed 负载、无限流、20 秒稳态）

| 并发 WS 连接 | 消息/秒 | 可用性(ACK/发送) | 端到端 p50/p95 |
|---|---|---|---|
| 256 | ~1,790 | 99.6% | 65 / 85 ms |
| **512** | **~3,535** | **98.1%** | **80 / 126 ms** |
| 1024 | ~6,950 | 65.9% | 1,260 / 1,410 ms |

**结论：2 核机器上稳定的工作点是 ~512 并发连接 / ~3,500 消息每秒。** 1024 连接时可用性掉到 66%，这是本机型的实际拐点。

容量由**扇出写放大**决定：决定容量的是「消息数 × 群成员数」= 帧扇出总量，2 核上限约 **40,000 帧/秒**；超上限后不是崩溃，是排队变长 + 服务端 NACK。1000 连接实测：50 人群可用性 32.3%（~2,196 msg/s 成功）、500 人群 2.2%——1000 个连接本身握手 1000/1000 成功，瓶颈在扇出。

**实践建议**：
- 2 核：群成员数 ≤50 且消息频率 ≤1 条/秒/人。
- 8 核以上：帧扇出上限按核数近似线性扩展，1000 人大群在此档位才可行。
- 超大群正解是拉取模式（消息不扇出、客户端按 seq 拉取）。

复现：`scripts/stress/main.go`（`--conv-members` 设扇出宽度，`-ladder start=256` 自动倍增到可用性 <70%）。

## 快速开始

1. 到 [Releases](https://github.com/yingo-server/UFOT/releases) 下载对应平台的产物。
2. `sudo ./deploy/install-interactive` 交互式安装（无 TTY 时自动降级为参数模式）。
3. `sudo systemctl enable --now ufot` 启动；`curl https://你的域名/healthz` 应返回 `{"ok":true,...}`。
4. 服务器只提供 API/WS，不托管 UI。把你的客户端 env 指向本服务即可：

```bash
# 客户端侧（变量名按你的前端框架）
API_BASE=https://chat.example.com
WS_URL=wss://chat.example.com/ws
```

### 支持的平台

| 平台 | 架构 | 产物 |
|---|---|---|
| Linux | x86_64 / x86 / arm64 / armv7 / riscv64 | `.tar.gz`（静态链接 SQLite） |
| Windows | x86_64 / x86 | `.zip` |
| 源码 | — | `ufot-<ver>-src.tar.gz` |

Windows on ARM / ARM64 暂无产物（缺 mingw CGO 工具链），见 [已知问题](docs/known-issues.md#3-windows-arm--arm64-不提供构建产物)。

### 要求

- Linux 服务器（2 核 2G 起），systemd
- 域名（TLS 由 Caddy 自动签发）
- PostgreSQL（冷存储，可选；不配则只保留本机热数据）

### 安装

```bash
# 1. 下载（版本号见 https://github.com/yingo-server/UFOT/releases）
V=1.4.0
curl -LO https://github.com/yingo-server/UFOT/releases/download/v$V/ufot-v$V-linux-amd64.tar.gz
curl -LO https://github.com/yingo-server/UFOT/releases/download/v$V/SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing
tar -xzf ufot-v$V-linux-amd64.tar.gz

# 2. 安装（交互式向导；无 TTY 时自动降级为参数模式）
sudo ./deploy/install-interactive

# 3. 启动
sudo systemctl enable --now ufot
```

首次启动会创建管理员账号并打印一次性的
TOTP 密钥（用 `journalctl -u ufot | grep otpauth` 查看），请立即在认证器里登记。

## 部署与升级

安装细节、Caddy 配置、CORS 与环境变量、备份恢复、故障排查与安全基线见
[部署手册](docs/deploy.md)（含「独立前端如何指向本服务」一节）。

### 升级

```bash
# 备份 → 替换二进制 → 重启（详见 docs/deploy.md §7）
sudo ufotctl backup
sudo systemctl stop ufot
# 替换 releases/<ver> 下的二进制并切换 current 符号链接
sudo systemctl start ufot
sudo ufotctl doctor
```

### 常用运维

| 命令 | 用途 |
|---|---|
| `ufotctl status` / `doctor` | 服务状态；体检（DB 完整性、磁盘、冷库连通、审计存储） |
| `ufotctl backup [--dry-run]` | 备份热库与配置 |
| `ufotctl ban <uid> --days 7` / `unban <uid>` | 封禁 / 解封 |
| `ufotctl flags list` / `flags set k=v` | 查看/修改功能开关 |
| `ufotctl archive --dry-run` / `--run` | 归档预演 / 执行 |
| `opchat` | 打开终端管理台（交互式） |
| `adminctl users list --q 张` / `broadcast send --text "..."` | 检索用户 / 全站广播 |

日志：`journalctl -u ufot -f`　　Dump 内部状态：`kill -USR1 $(pidof ufot)`

日常运维、内容治理与应急处理见 [管理员指南](docs/admin-guide.md)；服务使用者见 [服务使用者指南](docs/user-guide.md)。

## 面向开发者

- [开发文档索引](docs/README.md)：架构、协议、各层设计（从这里进入）
- [架构](docs/architecture.md) / [API 参考](docs/api-reference.md) / [CLI 参考](docs/cli-reference.md) / [WS 协议](docs/ws-protocol.md)
- [压测方法与基准](docs/perf-spec-v1.2.md)：本文容量数据的测量方法
- 贡献流程与构建方式见开发文档索引；变更日志见 [CHANGELOG.md](CHANGELOG.md)

## 文档

面向客户端开发者：
- [客户端契约](docs/client-contract.md)：外部客户端接入本服务所需的一切（env 解析、认证、WS 入口、媒体、推送、错误码、限流）

面向部署者：
- [部署手册](docs/deploy.md)：安装、配置、Caddy、CORS、备份恢复、故障排查、安全基线
- [管理员指南](docs/admin-guide.md)：日常运维、内容治理、应急处理
- [服务使用者指南](docs/user-guide.md)：部署、验证、接客户端的最短路径
- [发布说明 v2.0.0](docs/release-notes-v2.0.0.md)：容量契约 + ACK 诚实 + 写吞吐翻倍（诚实数据）
- [发布说明 v1.5.0](docs/release-notes-v1.5.0.md)：推送/举报等功能补齐
- [发布说明 v1.4.0](docs/release-notes-v1.4.0.md)：生产化转默认 + 已知容量限制（知情发布）

面向开发者：
- [开发文档索引](docs/README.md)：架构、协议、各层设计
- [API 参考](docs/api-reference.md) / [CLI 参考](docs/cli-reference.md) / [WS 协议](docs/ws-protocol.md)
- [品牌规范](docs/brand-guidelines.md)：名称、口号、字标与视觉基调
- [路线图](docs/roadmap.md) / [变更日志](CHANGELOG.md) / [已知问题](docs/known-issues.md)

## 许可

[Apache-2.0](LICENSE)
