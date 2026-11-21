# 服务端设计文档符合性审查报告

- 审查对象：`docs/architecture.md`、`docs/bridge-design.md`、`docs/admin-data-design.md` vs `server/`（含工作区 47 个未提交文件的现状）
- 审查方式：只读审查 + `rg` / `go build` / `go test` 实证。构建与测试均通过（`go build ./...` OK；`go test ./server/internal/bridge/... ./server/internal/data/...` 全绿）。
- 日期：基于工作区当前状态（git status 含 47 个未提交修改/新增文件）。

---

## P0（红线违反 / 文档承诺的机制不存在）

### P0-1 import-lint 完全不存在，且规则已被事实违反

- **文档出处**：bridge-design.md §2.2（「CI 加 import-lint 强制」）、§4.1 禁令清单（「CI import-lint：`bridge/*` 互相 import → 编译失败；`user/ data/ admin/` import `bridge/` → 编译失败；任何包 import `os/signal` 除 pkg/signalset 与 bridge/supervisor → 编译失败」）；architecture.md 设计律 1。
- **代码证据**：
  - `.github/workflows/ci.yml` 的 go job 只有 `make lint`；`Makefile` 的 `lint` 目标 = `go vet` + `gofmt -l`。全仓库 `rg -n "import-lint|importlint|depguard|golangci"` 只命中三个文档/注释文件，无任何工具配置。
  - 规则 1（兄弟组件不互相 import）：手动验证目前成立——`rg -n "bridge/(framerw|verifier|router|egress|ack|session|adminapi|supervisor)" server/internal/bridge/ --type go` 除 `pipeline.go` 外零命中。
  - 规则 2（层不得 import bridge）：**已违反**——`server/internal/admin/server.go:26` import `internal/bridge/adminapi`（用其 `Authorizer` 接口类型，`server.go:86`）。
  - 规则 3（os/signal 只许 signalset+supervisor）：成立——`rg -ln '"os/signal"' server/` 只命中 `pkg/signalset` 与 `bridge/supervisor`。
  - 规则 4（不许直接写日志文件）：成立——`rg -n "fmt\.Print" server/internal server/cmd` 零命中（CLI 输出除外，属正常）。
- **建议**：改代码。补一个约 60 行的 `scripts/importlint`（或 golangci-lint depguard 配置）挂进 `make lint`，并把 `admin/server.go` 对 `adminapi.Authorizer` 的依赖倒置（接口移到 `pipedef` 或 admin 侧自定义，bridge/adminapi 反向实现）。文档红线写了「CI 强制」却没有 CI 步骤，是最典型的文档-代码落差；且无 lint 时规则 2 已经破口。

### P0-2 数据层 policy 的 rate_limit 策略无人消费（策略真源断链）

- **文档出处**：admin-data-design.md §4（「限流：rate_limit 表=计数桶原语（机制），各 bucket 阈值/窗口（策略，外置）」）、§10 红线 3（「policystore 是唯一策略入口」）。
- **代码证据**：
  - `server/internal/data/policy/types.go:62` 定义 `MsgPerUserPerMin` / `LoginPerIPPerMin`，`config/policy.json` 有 `"msg_per_user_per_min": 60`。
  - `rg -n "MsgPerUserPerMin" server/ --type go -g '!*_test.go'` 除 `data/policy/` 自身外**零命中**。
  - 实际限流参数硬编码在 `server/internal/user/net/limit/limiter.go:82` `DefaultConfig()`（`KindMsgSend: {Rate: 20, Burst: 40...}`），注释虽称「可由 policy 热更」，但 `server/internal/user/core/policy/policy.go:51` 的默认值就是 `limit.DefaultConfig()`，`user/app/server.go:65` 直接采用——没有任何代码把 `policy.json` 的 `rate_limit` 段接进 limiter（`data/policy` 与 `user/core/policy` 是两个互不相通的 policy 包，本身也是漂移，见 P1-9）。
- **影响**：管理员改 `policy.json` 的限流值毫无效果；按文档「代码出现策略数字即评审打回」的判定标准，limiter.go:82 的整张参数表就是放错位置的策略（它有 security-spec §1.2.2 出处，但那也是文档不是策略文件）。
- **建议**：改代码（把 `rate_limit` 段桥接到 `user/core/policy` 的 Limits），或在文档中明确 v1.0 限流参数以 security-spec 表为冻结基线（那是更弱的选项，不推荐）。

---

## P1（偏差：实现了但与文档不一致）

### P1-1 消息写路径 ack 用 kind 103，文档时序图写 911

- **文档出处**：architecture.md「跨层全链路时序」：`→outbound 911 ack{msg_id,seq}`、`Bridge Router(kind 911→user层 + 911投递→conv.{cid}订阅者)`。
- **代码证据**：`server/internal/data/pipe/ack.go:25`：`return &env{Kind: uint32(kinds.KindMsgAck), ...}`（=103 MSG_ACK）。kinds.go 中 911 = `KindFlagSync`（FEATURE_FLAGS + flag 写库 ack），语义完全不同。
- **建议**：改文档。代码用 103（MSG_ACK，号段表本就属于消息层）是正确设计；总纲时序图把 911 当消息 ack 是笔误。architecture.md 时序节两处 `911 ack` 应改为 `103 MSG_ACK`。

### P1-2 热库表名带域前缀，与 §3.1 表名清单不一致

- **文档出处**：admin-data-design.md §3.1（表名 `users, identities, devices, conversations, members, conversation_seq, messages_hot, media, presence, rate_limit, feature_flags, admin_users, admin_sessions`）。
- **代码证据**（`rg -n "CREATE TABLE" server/store/migrations`）：实际为 `identity_users, identity_identities, identity_devices, identity_bans, conv_conversations, conv_members, conv_seq, msg_messages_hot, media_media, sys_presence, sys_rate_limit, sys_feature_flags, adm_admins, adm_admin_sessions`；冷库 `identity.login_history, msg.messages_archive` ✓ 但多出 `msg.messages_text`（trgm 搜索表，文档没有）。
- **建议**：改文档。SQLite 单文件多域共存必须前缀防撞名，方向合理且「域前缀」与「分域迁移目录」三处一致（REST 路径 `/users /reports /messages /flags /audit` 与域名对齐，§10 红线 2 事实上满足）。§3.1 应改写为「表名 = `{域}_{实体}`」。

### P1-3 reports 落在热库，文档说冷库

- **文档出处**：admin-data-design.md §3.1 冷库 schema：`sys: audit_log, reports, archive_runs`。
- **代码证据**：`server/store/migrations/hot/sys/0002_reports.sql:10` `CREATE TABLE sys_reports`；`server/internal/admin/prod/reports.go:45` 管理面直接查热库 `sys_reports`。冷库 `cold/sys/0001_audit_log.sql` 也建了 `sys.reports` 但无人读写。
- **建议**：改文档（热库报表是合理的就近读设计），并删除冷库 DDL 中无人使用的 `sys.reports`，或注明预留。二选一，当前是两处 DDL 一真一假。

### P1-4 REST 动词子资源超出文档白名单

- **文档出处**：admin-data-design.md §8（「动作动词仅允许在状态机子资源（`/reports/{id}/handle`）；禁止 RPC 化路径」）。
- **代码证据**：`server/internal/admin/handler/api.go:119-138`：`/users/{id}/ban`、`/users/{id}/unban`、`/users/{id}/kick`、`/messages/{id}/recall`、`/archive/run`（run 为动词）。均为名词+id+动作子资源形态，比 `/doBan` 好，但不在文档白名单内。
- **建议**：改文档，把白名单扩为「状态机与大影响动作子资源：`/{resource}/{id}/{action}`」，代码形态无需动。

### P1-5 CLI 三件套名称与职责划分不符

- **文档出处**：architecture.md 总目录结构 + admin-data-design.md §7：`chatd-ctl / datactl / adminctl`。
- **代码证据**：`ls server/cmd` = `adminctl, chatd, ufotctl`。`ufotctl`（usage 自述「系统级运维 CLI」）承担了文档中 chatd-ctl 与 datactl 的全部职责（status/doctor/backup/archive/flags/ban），且 `datactl migrate status` 不存在（迁移只有启动自动 up，`server/internal/data/store/migrate/`）。
- **建议**：改文档（合并为 `ufotctl` + `adminctl` 两件套是更少二进制的合理选择），`migrate status` 可作为 P3 功能缺漏补进 ufotctl doctor。

### P1-6 split 部署形态：systemd units 引用不存在的二进制

- **文档出处**：architecture.md「部署形态」（split：同代码编译出 5 个二进制）；bridge-design §4（`cmd/chatd-bridge/main.go` 等）。
- **代码证据**：`deploy/systemd/ufot-{bridge,user,data,admin}.service` 的 ExecStart 指向 `bin/chatd-bridge` 等，但 `server/cmd` 下只有 `chatd`，且 `server/cmd/chatd/main.go:71` 明确拒绝非 embedded mode（「this build is embedded-only」）。units 一旦启用即启动失败。
- **建议**：短期改文档（标注 split 为 1.1 计划、units 为预留），长期补 `cmd/chatd-{bridge,user,data,admin}`。至少应在 units 加注释，避免运维误启。

### P1-7 cmd/chatd 装配远超「<200 行」

- **文档出处**：architecture.md 总目录结构（「chatd # embedded 单进程装配(<200行,零逻辑)」）；bridge-design §4（「装配代码 < 200 行」）。
- **代码证据**：`wc -l server/cmd/chatd/*.go` = 2502 行（去测试 1844 行；assemble.go 566、wire.go 502、adminrest.go 69…）。内容确为接线+失败映射，未见业务规则，但量级承诺失效。
- **建议**：改文档，删除具体行数承诺或改为「只装配、零业务规则」的定性表述。

### P1-8 data 层目录结构与 §3.3 包结构不一致

- **文档出处**：admin-data-design.md §3.3：`data/{conn,pipe/{seqalloc,dedup,flusher},read,cache,policy,job,domain/{identity,conv,msg,safety,system},store/...}`。
- **代码证据**：`ls server/internal/data` = `cache, job, pipe, policy, store`——无 `conn/`（connector 职责并入 `transport/remote` 的 ReconnectingPeer）；无 `read/`（`pipe/readrouter.go`）；无 `domain/`（930/940 域 handler 是 `pipe/conv.go` 475 行内的 `convDomain` + `identityDomain` 两个 struct，`pipe.go:196-204` 分发）。`pipe` 是单包多文件而非三个子包。`store/pg` 为单包（`cold.go` 同时实现 ArchiveTarget+AuditJournal+search），文档要求 `pg/{archive,search,audit,report}.go` 分文件。
- **附带 god-component 观察**：`pipe/conv.go` 475 行承载 conv+identity 两个域 + 成员校验 + seq 分配调用，是设计律 1「每组件一包一职责」下最接近违规的文件（尚可辩护为「域 handler 聚合层」，但域分包是文档明文）。
- **建议**：改代码（把 conv/identity 域拆出 `data/domain/`，顺带解决 P0-1 的接口倒置）或改文档收敛包结构描述。倾向前者：域分包是 §10 红线 2「三处一致」的 Go 侧锚点，现在锚点缺失。

### P1-9 策略热载走本地直读，未经「桥接帧下发」链路；912 CONFIG_UPDATE 无发布者

- **文档出处**：admin-data-design.md §4「策略真源与流转」：桥接读 policy → 帧下发 → 数据层装载；SIGHUP 桥接重读 → version+1 → 增量帧下发。architecture.md 设计律 3 同。
- **代码证据**：`server/cmd/chatd/assemble.go:487` `reload()`：`a.policy.Reload()`（进程内直读 policy.json）+ 重新广播 FEATURE_FLAGS(911/12) 帧。`KindConfigUpdate`(912) 在 `rg -n "KindConfigUpdate" server/internal` 只命中 kinds 定义、defaults 能力表、router 路由表——**没有任何代码构造/广播 912 帧**。
- **建议**：embedded 单进程下直读是同构简化（无跨进程下发需求），合理；但文档描述的是 split 形态的目标机制。改文档标注「embedded 形态策略为进程内直读热载；912 帧为 split 形态预留」，或补 912 广播。另 `data/policy`（数据层）与 `user/core/policy`（用户层）两个 policy 包互不引用，文档应说明这是两个域各自的策略面。

### P1-10 backup 不走 job.Unit / archive_runs 账本

- **文档出处**：admin-data-design.md §5（三种触发共享 job.Unit 与账本；backup 是 job 之一）、§1.1（backuper：VACUUM INTO）。
- **代码证据**：`server/internal/data/job/` 只有 `archiver.go + jobber.go`；`server/cmd/chatd/jobs.go` 只注册 archiver。备份由 `ufotctl backup` 调 `deploy/backup.sh` 外部脚本实现（`ufotctl/backup.go`）。`config/policy.json` 的 `backup` 与 `trim` 段定义了但无消费者（`rg -n "Trim|Backup" server/internal/data/job` 零命中）。
- **建议**：改代码（补 backup/trim job.Unit）或改文档把 backup 明确为「部署层脚本 + CLI 触发」的 v1.0 形态、trim 为预留。当前 policy.json 里两段死配置会误导运维。

### P1-11 store 包外 SQL：admin/prod 整包是 SQL

- **文档出处**：admin-data-design.md §10 红线 1（「handler 中出现 SQL 字符串即打回；store 包外无 SQL」）。
- **代码证据**：`server/internal/admin/prod/reach.go:81-144`、`reports.go:45-90`、`bootstrap.go` 等含大量 SQL 字面量。包头注释自我豁免：「this package IS a store-side adapter package — the only place outside internal/{admin/store,data/store} allowed to hold SQL」。
- **建议**：改文档，把 prod 明确列为 admin 侧的 store 实现包（它实现了 service 层 ports，等价于 data/store/sqlite 的地位）。代码把 SQL 收敛在一个包内、全参数化，实质符合红线精神，只是包名不在白名单。

### P1-12 systemd 基线两份文档互相矛盾，代码取其一

- **文档出处**：bridge-design §5（Type=notify、Restart=always、RestartSec=1、WatchdogSec=15、LimitNOFILE=16384）vs admin-data-design §9（Type=exec、Restart=on-failure、RestartSec=5、WatchdogSec=60、LimitNOFILE=65535）。
- **代码证据**：`deploy/systemd/*.service` 全部为 notify/always/1s/15s/16384（admin 为 8192）。
- **建议**：改文档。两份文档对同一数据层 unit 给出两套参数本身就是缺陷；代码取 crash-only 更激进的 bridge 版与整体设计律一致。admin-data §9 的基线段应删除或改为引用 bridge-design §5。

### P1-13 SIGUSR1/SIGUSR2 语义：admin-data §9 与 bridge-design §5 冲突且均未完整落地

- **文档出处**：bridge-design §5（SIGUSR1=状态 dump、SIGUSR2=预留 GC/heap dump）；admin-data §9（SIGUSR1=触发备份、SIGUSR2=WAL checkpoint+指标 dump）。
- **代码证据**：`server/cmd/chatd/assemble.go:539` `dump()` = SIGUSR1 状态快照（bridge 语义）✓；SIGUSR2 在 `signalset` 中映射为 `Custom`，但 `main.go` 的 watch switch 只处理 Quit/Reload/Snapshot——**Custom 无任何 handler**（`rg -n "Custom" server/cmd/chatd` 零命中）。
- **建议**：改文档（两处统一为 bridge 版：USR1=dump、USR2=预留），SIGUSR2 handler 作为 P3 预留即可，但文档不应给数据层单独发明一套信号语义。

---

## P2（漂移：实现合理，文档该更新）

### P2-1 authsvc / mediasvc 走 HTTP（用户面直连入口）

- **文档出处**：architecture.md 总体架构图只画了「WS(用户通道)」到 usergw，HTTP 仅标注「仅登录/管理后台」。
- **代码证据**：`server/internal/authsvc/authsvc.go`（`/api/auth/*`，挂用户端口）、`server/internal/mediasvc/mediasvc.go`（`/api/media/upload`、`/api/media/{id}`，自述「media bytes never travel the WS channel, frame codec caps 64KB」）。security-spec §2.3/§4.3 有出处，known-issues.md #1 已记录 media ACL 缺陷。
- **判定**：不构成「绕过 Envelope」的红线违反——帧上限 64KB（`user/app/config.go:74`）使媒体走 HTTP 是物理必然，登录在 WS 握手前也必须走 HTTP。属架构图未反映现实的漂移。
- **建议**：改 architecture.md 架构图，在 usergw 旁标注 `/api/auth/*` 与 `/api/media/*` 两个同端口 HTTP 面。

### P2-2 MEDIA_PRESIGN 号段（300-304）定义齐全但空转

- **代码证据**：`rg -n "MediaPresign" server/internal` 仅命中 kinds 常量、bridge 能力表、limiter 规则三处注册性引用，无任何 handler；实际媒体上传/下载全走 mediasvc HTTP（P2-1）。客户端 `apps/client/src/net/protocol/kinds.ts:50` 仍声明 300/301。
- **建议**：文档 architecture.md 号段表 300-304 加「v1.0 未启用（HTTP 直传替代），presign 为 1.1 预留」注记。

### P2-3 limit.KindPriority = 1_000_000 是号段表外的合成 kind

- **代码证据**：`server/internal/user/net/limit/limiter.go:50` `const KindPriority uint32 = 1_000_000`（Envelope.Flags 的 PriorityFlag 映射）。architecture.md 号段表（0-999）未覆盖。
- **建议**：改文档，在号段表加一行「≥1_000_000：限流器内部合成 kind（非线上帧）」。

### P2-4 /healthz HTTP 端点与「健康检查=HEARTBEAT 帧」设计相悖

- **文档出处**：bridge-design §3（「健康检查 HTTP /health → 改为 HEARTBEAT 帧，systemd watchdog 由 Supervisor 转译；少一个端口」）。
- **代码证据**：`server/cmd/chatd/main_test.go:117` 与 `assemble.go:253` 证实 user 端口与 admin 端口各挂 `/healthz`。
- **建议**：改文档或改代码皆可；现实是 Go 生态与探针工具对 HTTP healthz 的依赖使保留更务实。文档 §3 该行应改写。

### P2-5 jobber cron 是受限子集（@daily / interval），非完整 cron

- **代码证据**：`server/internal/data/policy/types.go:33-41` 注释明言：经典 5 字段 `0 4 * * *` 被降级为 daily 语义，`*/n` 与列表拒载。文档 §1.1 只说「按 policy 中的 cron 调度」。
- **建议**：改文档 §4/§5 注明受支持的 cron 方言语法。

### P2-6 banRecorder：管理面在 admin/prod 层镜像写热库 ban 表

- **代码证据**：`server/internal/admin/prod/banrecorder.go`（960 帧 dispatched 后镜像写 `identity_bans`，注释自述「frame is the authority, the row is the local record」，best-effort 不失败主操作）。
- **判定**：与 architecture.md 封禁时序（「data 层 960 写 ban 记录」）路径不同，但它是无数据层部署时的降级补丁，append-only 且不阻塞。属应记录的漂移。
- **建议**：改文档（admin-data §4 封禁行加注管理面镜像机制），或在 split 形态落地后移除。

### P2-7 flusher / archiver 的机制默认值与「策略外置」并存

- **代码证据**：`server/internal/data/pipe/flusher.go:15` `defaultFlushBatch = 1000`（policy `batch_size` 可覆盖）；`server/cmd/chatd/jobs.go:50` cutoff 兜底 `days = 90 // matches policy.Default()`；`archiver.go:97` `maxBatches := 10000` 硬上限。
- **判定**：均为「policy 缺失时的安全兜底」而非策略主导，注释明确指向 policy.Default()，不违反 §4 判定标准（判定标准针对的是「策略值出现在机制代码里当唯一来源」）。但 `rg -n "86400"` 的两个命中（mediasvc.go:282 Cache-Control、admin/handler/auth.go:122 cookie maxAge 上限）是 HTTP 机制参数，非运营策略，合规。
- **建议**：无需改代码；可在 admin-data §4 补一句「机制代码允许携带与 policy.Default() 一致的兜底值」。

### P2-8 ufotctl 缺退出码 65

- **文档出处**：admin-data §7「目标不存在 65」。
- **代码证据**：`ufotctl/cli.go:42-43` 只有 `exitUsage=64 / exitDenied=77` + 兜底 1；adminctl 有 `exitNotFound=65`（`client.go:26`）。
- **建议**：小改代码补齐，或文档注明 65 仅 adminctl。

---

## P3（缺漏：文档有、代码没有）

| # | 缺漏 | 文档出处 | 代码证据 | 判定 |
|---|---|---|---|---|
| P3-1 | **audit-verify 作业**（每月 cron + 手动，断链告警） | admin-data §6 | `rg -n "audit.?verify" server/` 只命中 cold.go 两个注释（RecordHashTail "exposed for the audit-verify job"）；jobs.go 只注册 archiver | 真缺（校验函数已备好，job.Unit 未接） |
| P3-2 | **trim job**（通用 `(表,过期列,TTL)` 删除器） | admin-data §1.1/§4 | `server/internal/data/job/` 无 trim；policy.json `trim` 段无消费者 | 真缺（配置已预留） |
| P3-3 | **TOPOLOGY 913 帧发布者**（bridge 指标=帧，exporter=订阅者） | bridge-design §3 | `rg -n "KindTopology" server/internal/bridge` 仅命中 defaults 能力表与 router 路由表，无构造/发布代码 | flag 预留性质（split 形态才有意义） |
| P3-4 | **诊断环回压测 990-999**（chatd-ctl bench） | architecture 号段表 / bridge-design §7 | kinds.go 只定义 `KindDiagLoopback=990`；router.go:63 `kind >= 910 && kind < 990`——990 落 NO_ROUTE，无 handler | 真缺 |
| P3-5 | **INFO 日志按 kind 采样（消息 10%）** | bridge-design §6 | `log.go:143` 注释「采样由调用方负责」，`rg -n "sample|采样"` 无实现 | 真缺（journald RateLimitBurst 部分兜底） |
| P3-6 | **SIGHUP 白名单含「路由表」热更** | bridge-design §5 | `router.Set()` 存在（router.go:74）但 `assemble.go reload()` 只重载 policy+flags，未调 Set | 半缺：机制在、未接线 |
| P3-7 | **datactl migrate status / policy show / doctor 的 migrate 部分** | admin-data §7 | ufotctl doctor 覆盖热库/磁盘/冷库连通，无 migrate status 子命令 | 真缺（小） |
| P3-8 | **SIGUSR2 handler** | bridge-design §5（stdout 摘要） | signalset 定义 Custom 语义，chatd watch 不处理（见 P1-13） | 预留 |
| P3-9 | **exit 70 在 embedded 模式的可达性** | bridge-design §5 | supervisor.go:26 定义 `ExitInvariant=70`（AckTracker panic→Recover 转换），但 embedded 下 supervisor 信号通道为 `neverFires()`（assemble.go:563），chatd 自身只返回 0/1/2/75——AckTracker panic 在 embedded 进程是否映射为 70 **待确认**（panic 未被 chatd main 捕获时应崩进程退出码 2，而非 70） | 待确认，建议补一条 e2e 验证 |

## 通过项（有证据的符合）

以下红线逐条核对为**符合**，列出以示覆盖：

- **审计单入口/只追加**：`store/api.go:333` AuditJournal 单方法接口；写路径唯一实现 `pg/cold.go:263-274`（哈希链 INSERT）；`rg "INSERT INTO sys.audit_log" server/` 仅此一处；DDL 侧 `cold/sys/0001_audit_log.sql` 有 UPDATE/DELETE 拒绝触发器。审计读为 since_id 顺序游标（`service/audit.go:30`、`pg/auditreader.go:30`）✓。auditshim 是唯一 service 侧写入口，dry-run 抑制审计行（`auditshim/shim.go` Meta.DryRun）✓。
- **幂等**：`handler/idempotency.go` Stripe 语义 5min/10000 键 ✓；jobber `ErrRunning`→409 ✓；archiver copy-then-delete 幂等重跑 + watermark 防死循环 ✓；`client_msg_id` dedup（pipe/dedup.go，LRU 10 万）✓。
- **机制/策略分离（数据层主链）**：archiver 只消费 `Cutoff func() int64`（archiver.go:31），cutoff 由装配层从 policy 换算（jobs.go:44）；flusher 1000/50ms 为机制默认、policy.batch_size 覆盖 ✓。
- **一切皆帧（层间）**：usergw→bridge→data 全部 Envelope 帧路径；AdminAPI 是文档允许的唯一非 socket 入口（§2.2 表内）；authsvc/mediasvc 的 HTTP 属前端→用户层入口，见 P2-1 判定。
- **crash-only**：无优雅容错状态机；AckTracker panic 即崩；SQLite WAL 隐含恢复；archiver SIGTERM 停在批边界并标 failed 供下轮幂等续跑（archiver.go:103-109）✓。
- **日志红线**：无 `fmt.Print` 绕过；`log.RedactPhone`（138\*\*\*\*1234）防手机号 PII（log_test.go:39）；authsvc 日志只打 HMAC 索引短串（otp.go:93 `subjectShort`）✓。
- **kind 号段**：kinds.go 77 个 kind 全部落在文档号段内，无越界、无未登记线上 kind（KindPriority 见 P2-3）；350-354 CALL 常量存在但 flag 关闭，与 known-issues #3 一致 ✓。
- **信号（chatd/main.go）**：SIGTERM/SIGINT→usergw→pipe→bridge 顺序排空（assemble.go:426）✓；SIGHUP→policy+flags 热载、失败保旧不退出（assemble.go:487-494）✓；SIGUSR1→stdout 单行 JSON dump ✓。
- **退出码包**：exitcodes 0/1/2/70/75/78/143 与两份文档并集一致；main.go 实际用 0/1/2/75 ✓。
- **分域迁移**：`hot/{identity,conv,msg,media,sys}`、`cold/{msg,identity,sys}`、`admin/adm` 目录与 §3.2 一致；`_migrations(domain,version,...,checksum)` + `ErrChecksumMismatch` 拒启 ✓。
- **systemd**（bridge 版基线）：notify/always/1s/15s/16384 全符 ✓。
- **桥接组件禁令（运行时事实）**：兄弟组件互不 import；Router 纯查表无状态（router.go:20-22 注释即禁令复述）✓。

---

## 文档更新清单

必须修改才能反映现实的文档节：

1. **architecture.md**
   - 时序图：`911 ack` → `103 MSG_ACK`（P1-1）。
   - 架构图：usergw 加 `/api/auth/*`、`/api/media/*` HTTP 面（P2-1）。
   - 号段表：300-304 加「v1.0 未启用」注记；补 ≥1_000_000 合成 kind 行（P2-2/3）。
   - 目录结构：`chatd-ctl/datactl` → `ufotctl`；`<200 行` 改定性（P1-5/7）；标注 split 二进制与对应 systemd units 为预留（P1-6）。
2. **bridge-design.md**
   - §2.2/§4.1：import-lint 从「CI 强制（现状）」改为「计划中的 CI 步骤」，或推动补 lint 后保留（P0-1）。
   - §3：/healthz 行改写为「v1.0 保留 HTTP healthz，HEARTBEAT 帧为 split 形态方案」（P2-4）。
   - §5/§7：SIGUSR2 标预留；bench（990-999）标未实现（P1-13/P3-4）。
3. **admin-data-design.md**
   - §3.1：表名改 `{域}_{实体}` 前缀制；reports 移到热库（P1-2/3）。
   - §4：限流策略段标注「v1.0 未接线」（若不修代码，见 P0-2）；补机制兜底值说明（P2-7）、cron 方言限制（P2-5）。
   - §5：backup 标注 v1.0 为脚本+CLI 形态（P1-10）。
   - §7：CLI 名称改 ufotctl/adminctl；migrate status 标缺（P1-5/P3-7）。
   - §8：动词子资源白名单扩容（P1-4）。
   - §9：整节 systemd/信号基线与 bridge-design §5 对齐，删除矛盾参数（P1-12/13）。
   - §10 红线 1：「store 包外无 SQL」改为「store 适配包（data/store、admin/store、admin/prod）外无 SQL」（P1-11）。

优先行动建议（代码侧）：P0-1 import-lint + 接口倒置 > P0-2 限流策略接线 > P1-8 data/domain 分包 > P1-6 split 二进制或 units 注释。
