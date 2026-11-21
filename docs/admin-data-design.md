# 管理层与数据层设计

> 设计律：每个组件只做一件事并做好；机制与策略分离；一切资源按域分组；一切操作有 CLI 等价物；文本流、信号、退出码是 API 的一部分。

## 1. 数据层组件拆分

```
              socket (唯一出入口)
                   │
        ┌──────────▼──────────┐
        │ connector            │ 只管帧收发/重连/心跳/背压, 不解释业务字段
        └───┬───────────▲─────┘
     inbound chan    outbound chan
            │              ▲
┌───────────▼──────────────────────────┐
│ dispatcher: kind → handler map, 无状态 │
└─┬───────────────┬───────────────────┘
  ▼ 写路径          ▼ 读路径 (两条独立管道)
┌──────────────────────────┬──────────────────────┐
│ seqalloc→dedupguard→     │ readrouter:          │
│ writech→flusher          │  hot/cold/merge 路由  │
│ (组提交: 1000条/50ms,    │ cache 作为独立组件被  │
│  独占写连接, ack挂在     │ 二者共享调用          │
│  事务提交后)             │                      │
└──────────┬───────────────┴──────────────────────┘
  ▲ job 调度触发             ▲
┌─┴────────┐ ┌──────────┐ ┌─┴────────┐
│ archiver │ │ cache    │ │ policy   │
│backup/trim│ │(分片LRU) │ │(策略装载器│
│(job 单元) │ │          │ │ 唯一真源) │
└──────────┘ └──────────┘ └──────────┘
```

### 1.1 组件契约

| 组件 | 唯一职责 | 明确不做 |
|---|---|---|
| `connector` | 帧收发、重连退避、心跳 | 不解析 payload、不区分 kind |
| `dispatcher` | kind→handler 路由、超时管理 | 不接触 SQL |
| `seqalloc` | seq 区间分配（`UPDATE...RETURNING`） | 不落消息行 |
| `dedupguard` | client_msg_id 幂等查重 | 不写库 |
| `flusher` | 组提交：攒批→事务→ack（1000 条或 50ms） | 不做成员校验 |
| `readrouter` | 热/冷/merge 路由，归并排序 | 不含业务语义 |
| `archiver` | hot→cold 搬运（幂等、断点） | **不知道归档多旧的**——截止线由 policy 提供 |
| `cache` | Get/Put/Invalidate 原语 | 不主动失效猜测 |
| `policystore` | 加载/热载策略文档 | 不执行任何策略 |
| `jobber` | 按 policy 中的 cron 调度 job | 不含具体作业逻辑 |

**背压即 channel 语义**：writech 容量即队列上限；入队 `select+default` → 满则立刻回 BUSY。channel 本身就是队列。

### 1.2 写路径精确串联

```
dispatcher
  │ 每条: cache.member.Check(cid,uid) ──失败──► 直接组 401/403 结果(不进管道)
  ▼
seqalloc: 按会话聚组, 一次区间分配, seq/msg_id 写回每条
  ▼
writech <- readyBatch        (满则 BUSY)
  ▼
flusher: timer(50ms) 或 len==1000 → 提交
   hotstore.AppendMessages(batch)  // 事务
   成功: outbound <- ack103(msg_id, seq); cache.touchConv/Member   // 103 MSG_ACK；911 是 FEATURE_FLAGS，勿混（P1-1）
   失败: 整批回 code=500
```

## 2. 管理层组件拆分

```
cmd/admin (main: 只组装)
├── middleware/{authn, rbac, reqlog, csrf}   # 各一件事：解会话/查权限/请求日志/双提交校验
├── handler/*             # 每资源一文件：解析入参→调 service→渲染 JSON；无鉴权无审计代码
├── service/*             # 每域一事：用户/封禁/审查/flag/广播/审计查询
├── bridgeproxy           # HTTP→桥接转发器，带重试/超时/熔断；唯一知道桥接地址的组件
└── auditshim             # 审计装饰器：@Audited("user.ban") 的 service 方法自动产事件
```

审计不是每个 handler 手写的样板，而是可声明的机制。

## 3. 按域分组（禁止扁平化）

### 3.1 DDL 域划分

**热库 SQLite**（表名制式 `{域}_{实体}`，单文件多域共存靠前缀防撞名）：

| 域 | 表（实际 `{域}_{实体}` 制式） |
|---|---|
| identity | identity_users, identity_identities, identity_devices, identity_bans, identity_otp, identity_refresh, identity_credentials |
| conv | conv_conversations, conv_members, conv_seq |
| msg | msg_messages_hot |
| media | media_media |
| sys | sys_presence, sys_rate_limit, sys_rate_limit_penalty, sys_feature_flags, sys_reports |
| adm（独立 admin.db） | adm_admins, adm_admin_sessions |

**冷库 Postgres**（schema）：

```sql
CREATE SCHEMA identity;   -- login_history
CREATE SCHEMA msg;        -- messages_archive (分区表), messages_text (trgm 搜索表)
CREATE SCHEMA sys;        -- audit_log, archive_runs；reports 预留未使用（见下注）
```

> 说明（P1-2/P1-3）：热库表名一律 `{域}_{实体}`（如 `identity_users` / `conv_conversations` / `msg_messages_hot` / `media_media` / `sys_presence`），与分域迁移目录、REST 域名三处对齐。**报表实际落热库 `sys_reports`**（`hot/sys/0002_reports.sql`，管理面就近读，`admin/prod/reports.go`）；冷库 DDL 里的 `sys.reports` 为**预留、无读写者**。

### 3.2 迁移文件（每域独立编号序列）

```
store/migrations/
├── hot/{identity,conv,msg,media,sys}/{NNNN}_{name}.sql
├── cold/{msg,identity,sys}/{NNNN}_{name}.sql
└── admin/adm/0001_init.sql
```

迁移记录表按域：`_migrations(domain, version, name, applied_at, checksum)`——checksum 被篡改即拒绝启动。启动自动 up。⚠️ 未落实（v1.0）：`migrate status` 子命令未实现（`ufotctl` 无 migrate 子命令；迁移仅在启动时自动 up）— 计划 v1.1（P1-5/P3-7）。

### 3.3 Go 包结构

```
internal/data/
├── conn/connector.go
├── pipe/{seqalloc,dedup,flusher}.go
├── read/router.go
├── cache/cache.go
├── policy/{store,types}.go       # ★ 策略装载
├── job/{jobber,archiver,backup,trim}.go
├── domain/{identity,conv,msg,safety,system}/   # 每域 handler+store 调用
└── store/
    ├── api.go                    # 按域小接口
    ├── sqlite/                   # 按域分文件
    └── pg/{archive,search,audit,report}.go
```

**store 接口按域拆小接口**：

```go
type MsgWriter interface{ AppendMessages(batch) error; AllocateSeq(cid, n) (Range, error) }
type MsgReader interface{ ReadHot(cid, cursor, limit, asc) (Page, error) }
type IdentityStore interface{ UpsertUser(...); LookupIdentity(...); GetBan(...) }
type ConvStore interface{ AddMember/RemoveMember/CheckMembers/GetConvMeta }
type ArchiveTarget interface{ CopyBatch(rows) (moved, error) }
type AuditJournal interface{ Append(AuditEvent) error }   // 只有一个方法：追加
```

archiver 只依赖 MsgReader + ArchiveTarget + 删除原语——hot 表结构换成任何东西，archiver 一行不改。

## 4. 机制与策略分离（核心）

**判定标准：代码里出现"90 天""4:00"这类数字常量，它是策略，放错了地方。**

| 项 | 机制（数据层提供） | 策略（外置） |
|---|---|---|
| 归档 | archiver：给定 cutoff 幂等搬 N 行 | **hot_retention_days=90**、cron `0 4 * * *`、batch_size |
| 备份 | backuper：`VACUUM INTO` 到给定路径 | 备份 cron、路径、保留份数 |
| 清理 | trimmer：按 `(表, 过期列, TTL)` 通用删除 | 各表 TTL/保留期 |
| 举报处置 | reports 表：状态机字段 + 流转原语 | 什么算违规、判罚等级 → 审核员规范 + RBAC |
| 封禁 | safety 域：写 ban 记录原语 | 时长/等级档位（配置文件，不是代码） |
| 限流 | rate_limit 表：计数桶原语 | 各 bucket 阈值/窗口 |

**策略真源与流转**：

```
policy.json (/etc/chat/policy.json)
   桥接启动读 → 帧下发 → 数据层 policystore 装载
   SIGHUP 桥接 → 重读 → version+1 → 增量帧下发 → 热生效
   管理后台改策略类配置 → 走 972 flag 链路 → flag 即策略表
   ▼
数据层各组件只问 policystore: "当前 retention 是多少"
```

坏策略（retention=0）拒载并报 `policy.invalid` 指标，继续用旧策略——机制对策略鲁棒。

**限流接线现状（v1.0，P0-2 已修）**：`policy.rate_limit.msg_per_user_per_min` **已在装配层注入 limiter**（用户级聚合预算，与 security-spec 连接级基线取 min 钳制，见 `server/cmd/chatd/limitwire.go`）；SIGHUP 热载后即时下推、原子替换规则表（不放松于文档基线）。**⚠️ 未落实（v1.0）**：`policy.rate_limit.login_per_ip_per_min`（登录按 IP 限流）尚无消费者——登录限流由 authsvc/HTTP 层按 security-spec 自带实现，`policy.json` 该值不影响行为 — 计划 v1.1（P0-2 剩余项）。

**机制兜底值（P2-7）**：机制代码允许携带与 `policy.Default()` 一致的兜底值（如 flusher `batch_size=1000`、archiver cutoff 兜底 90 天、`maxBatches` 硬上限），仅当 policy 缺失时生效、非策略唯一来源，不违反本节判定标准。

**违规示例（评审打回）**：flusher 里写 `if now-90d`；archiver 硬编码 cron；safety handler 里 `duration = 7*24*3600`。

## 5. 作业 = 独立可调度单元（cron 哲学）

每个作业是 `job.Unit`：`Run(ctx, dryRun bool, args) (Result, error)`。三种触发方式**完全等价**，共享同一实现与 archive_runs 账本：

1. **定时**：jobber 按 policy 中 cron（受支持方言 P2-5：仅 `@daily HH:MM` / `@daily HH:00` / `interval N`（秒）；经典 5 字段 `0 4 * * *` 会被降级为 daily 语义，含 `*/n` 或列表的表达式解析期拒载——复杂调度须由装配层简化为 `@daily HH:MM`，见 `data/policy/types.go`）
2. **手动 API**：`POST /api/v1/archive/run`（→桥接→980）
3. **CLI**：`ufotctl archive --dry-run`

- **`--dry-run` 是一等公民**：归档只输出"将搬 N 行、涉及 M 会话、预估 X 分钟"；备份校验目标盘空间
- 并发保护：每作业单飞（前一轮未结束不重入），手动触发运行中返回 409

> ⚠️ 未落实（v1.0）：backup 与 trim **未以 `job.Unit` 形态接入**。v1.0 备份由 `deploy/backup.sh` + `ufotctl backup`（部署层外部脚本 + CLI 触发）承担，不走 `archive_runs` 账本；trim（通用 `(表,过期列,TTL)` 删除器）尚未实现，`policy.json` 的 `backup`/`trim` 段暂无消费者 — 计划 v1.1 接入 job.Unit（P1-10/P3-2）。

## 6. 日志与审计：journalctl 哲学

- 所有进程只写 stdout（JSON lines），journald 收；退出码遵守 sysexits（78=配置错、143=被信号杀）；**作业失败≠进程失败**，不污染进程退出码
- **审计 journal**：写=`AuditJournal.Append()` 唯一入口（冷库 sys.audit_log，INSERT-only + 哈希链）；**读=顺序游标流**：

```
GET /api/v1/audit?since_id=<id>&limit=500
   → 事件按 id 升序返回, 翻页唯一方式是 since_id 游标
   → 审计查看器 = "journalctl 的 Web 化"
```

**冷库不可用时的审计降级**：审计写入口在冷库缺失时退化为「只写日志不落盘」（每事件一条 WARN + `Dropped` 计数，不落盘，见 known-issues #4）；设 `require_audit_store=true` 可令冷库缺失时**直接拒绝启动**（fail-fast），生产建议开启（该开关已实现，P1 项已修）。

链条校验作业 audit-verify（每月 cron + 手动）：断链即 `audit_chain_broken=1` 告警。⚠️ 未落实（v1.0）：校验函数已备好（`RecordHashTail`），但 `job.Unit` 未接入 jobber（只注册了 archiver）— 计划 v1.1（P3-1）。

## 7. CLI 等价物（systemctl 风格）

**原则：每个 API 操作必有 CLI 等价物；每个 CLI 皆是 API 客户端而非旁路**——CLI 与管理后台走完全相同的 HTTP/鉴权链路，没有隐藏后门。

```
ufotctl —— 系统级运维 CLI(systemd unit 状态/信号/热库文件/备份/体检;
              少数变更命令 archive/flags/ban/unban 走同回环 REST API，不另开端口)
  status / doctor    # 体检: systemd 状态/热库/磁盘/冷库连通
  backup             # 调 deploy/backup.sh（见 §5）
  archive --dry-run|--run / flags list|set / ban|unban <uid>
  # ⚠️ v1.0 未实现: `migrate status`(P3-7)、`trim`(P3-2)、`audit verify`(P3-1)

adminctl —— 说话给管理层 API
  login / whoami
  users list|show|ban|unban / kick <uid>
  # ⚠️ 未实现：monitor（无 /monitor 路由）
  reports list|show|handle
  messages search|recall
  broadcast preview|send
  flags list|set / audit --since-id
  archive status / completion bash
```

退出码：成功 0 / 错误 1 / 语法 64 / 权限 77 / 目标不存在 65 / 等幂已生效 0。`--output json` 供脚本消费。

## 8. 管理 API 规范

- **REST 资源化**：路径只出现名词与既有 id；动作动词仅允许在**状态机与大影响动作子资源**，形态 `/{resource}/{id}/{action}`；v1.0 白名单（P1-4）：`/users/{id}/ban`、`/users/{id}/unban`、`/users/{id}/kick`、`/messages/{id}/recall`、`/reports/{id}/handle`、`/archive/run`；禁止 `/api/v1/doBan` 这类 RPC 化路径
- **幂等**：写操作一律 `Idempotency-Key`（UUID）Header，5 分钟窗口重复键返回首次结果（Stripe 语义）；运行中重触发 409
- **`?dry_run=true`**：大影响操作（broadcast/archive/batch ban）支持，返回预估影响，不产审计动作行
- **错误体统一** `{code, reason, request_id}`，与内部帧错误码同号段——错误语义只有一个真相源

## 9. 信号契约

> 信号语义与 systemd unit 基线**统一见 `bridge-design.md` §5**（SIGTERM/SIGINT 优雅退出、SIGHUP 热重载、SIGUSR1=状态 dump、SIGUSR2=预留、`Type=notify` / `Restart=always` / `RestartSec=1` / `WatchdogSec=15` / `LimitNOFILE=16384`）。数据层**不得另行发明一套信号语义**（P1-12/P1-13）；此前本节的 `Type=exec`/`WatchdogSec=60`/`LimitNOFILE=65535` 与「SIGUSR1=触发备份、SIGUSR2=WAL checkpoint」均与 bridge-design 矛盾，已删除。

数据层在 SIGTERM 排空中的**专属行为细化**（不改变上述信号语义，仅描述数据层在收到统一 SIGTERM 后做什么）：connector 停读、dispatcher 停收新请求；**flusher 把缓冲所有 ReadyMsg 最后一次组提交再退出**（不丢已分配 seq）；**archiver 在当前批事务边界保存断点后退出**，下轮幂等续跑。退出码 143。

## 10. 决策红线汇总

1. 组件=单一职责包+明确 channel 边界；**handler 中出现 SQL 字符串即打回；SQL 仅允许出现在 store 适配包：`data/store`、`admin/store`、`admin/prod`，其余包出现 SQL 即打回**（`admin/prod` 于 v1.0 扩入白名单——这是**决策而非豁免**：它是管理面 service ports 的存储适配实现，与 `data/store` 地位等价，SQL 全参数化并集中于单包；评审记录见 `review-server.md` P1-11）
2. 域分组三处一致：DDL（schema/前缀+分域迁移）、Go 包（domain/*）、REST 路径（资源名=域名）。三处对不齐即设计缺陷
3. 机制/策略判定：可运营变更常量 → policy.json 或 feature_flags；policystore 是唯一策略入口
4. 作业三触发（cron/API/CLI）共享同一 job.Unit 与账本；dry-run 是机制
5. 审计读=顺序游标流（journalctl 语义），写=单入口 append-only journal
6. 日志只写 stdout；退出码遵守 sysexits
7. CLI 是 API 的第二皮肤，不是旁路
8. 幂等键全写操作覆盖；错误码单一真相源

**实施顺序第 0 步**：先落地 `envelope/kinds` + `policy/policystore` + 迁移骨架（域目录与 _migrations 校验）——它们是其余所有组件的协议与地基。
