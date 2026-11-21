# 架构总纲

UFOT 由四个服务层级与一个桥接枢纽组成。所有层间通信走内部 socket（UDS/TCP），唯独管理层到桥接使用 HTTP API；前端到用户层使用 WebSocket。

## 总体架构

```
                            前端 SPA (React, embed)
         WS(用户通道) ═╗   HTTP ╗(登录/媒体: /api/auth/* /api/media/*)   HTTPS ╗(管理后台)
                      ▼        ▼                                        ▼
┌──────────────── 用户层 usergw ─────────────┐  ┌─ 管理层 admin ─┐
│ acceptor→codec→limiter→router→forwarder    │  │ middleware 链   │
│ (管道组件, channel 串联)                    │  │ handler/service│
│ HTTP 面(同端口): /api/auth/* /api/media/*  │  └───────┬────────┘
└───────────────┬────────────────────────────┘          │
          socket ══╗ (内部长连接)                 HTTP API ══╗
                   ║   socket ═══════╗                 ║ (唯一例外)
                   ▼                 ▼                  ▼
        ┌──────────────── 桥接层 Bridge(含总线 Bus) ───────────┐
        │ FrameRW→Verifier→Router→Bus→Egress (数据面流水线)      │
        │ Session/AckTracker/AdminAPI/Supervisor (控制面旁路)    │
        └───────────────┬─────────────────────────────────────┘
                  socket ═══╗ (内部长连接)
                            ▼
        ┌──────────────── 数据层 data ──────────────────┐
        │ connector→dispatcher→{写管道 / 读路由}          │
        │ seqalloc→dedup→flusher / readrouter→hot|cold  │
        │ archiver/backup/trim (job 单元) + policystore │
        ├─ 热存储: SQLite(本地 WAL)   冷存储: Postgres(远程)─┤
        └────────────────────────────────────────────────┘
```

管理层指令经 AdminAPI 翻译成 5xx 帧注入总线，与 socket 帧同构：管理指令的审计日志即帧日志。

> 说明（P2-1）：用户层 usergw 除 WS 用户通道外，还在**同一监听端口**上提供两个 HTTP 面——`/api/auth/*`（登录/刷新/登出，须在 WS 握手前完成鉴权）与 `/api/media/*`（媒体上传/下载；帧编解码上限 64KB，媒体字节不走 WS 通道，security-spec §2.3/§4.3 有出处）。此为前端→用户层入口，不构成「绕过 Envelope」的红线违反。

## 统一 kind 号段（全局唯一真相源，定义于 proto/）

| 号段 | 归属 | 内容 |
|---|---|---|
| 0-99 | 系统层 | HELLO/AUTH/PING/PONG/RESUME/SYNC/ERROR/KICKED/FEATURE_FLAGS/RATE_LIMITED |
| 100-199 | 消息层 | MSG_SEND/NEW/ACK/READ/DELIVERED/EDIT/DELETE/RECALL/FORWARD/SYNC/SEARCH |
| 200-299 | 会话层 | CONV_CREATE/UPDATE/MEMBER_*/ROLE/MUTE/PIN/TYPING/DRAFT |
| 300-349 | 媒体层 | MEDIA_PRESIGN/READY/EXPIRE/THUMB（其中 300-304：v1.0 未启用——媒体走 HTTP 直传，presign 为 1.1 预留，P2-2） |
| 350-399 | 通话预留 | CALL_OFFER/ANSWER/CANDIDATE/BUSY/HANGUP（flag 关闭） |
| 400-449 | presence | PRESENCE_UPDATE/SUBSCRIBE/UNSUBSCRIBE |
| 450-499 | 用户层 | PROFILE_UPDATE/BLOCK/UNBLOCK/REPORT |
| 500-599 | 管理下行 | ADMIN_BROADCAST/ANNOUNCE/MAINTENANCE + AdminAPI 注入的管理指令帧 |
| 600-899 | 全预留 | 未来业务（投票/卡片/支付/小程序…） |
| 900-909 | 层间控制 | 内部 HELLO(902)/SUBSCRIBE(903)/FRAME_ACK(904)/HEARTBEAT(905) |
| 910-919 | 层间系统 | 910 SHUTDOWN_NOTICE / 911 FEATURE_FLAGS+写库ack / 912 CONFIG_UPDATE / 913 TOPOLOGY |
| 920-929 | 数据读 | 920 READ_HISTORY 请求/921 应答（readrouter 热/冷/merge 透明路由） |
| 930-939 | 会话数据 | CONV 系列数据操作 |
| 940-959 | 身份数据 | identity 域 users/identities/devices 操作 |
| 960-969 | 安全域 | 封禁/审计/举报数据操作 |
| 970-979 | 系统 flag | feature flag 读写（flag 即策略表） |
| 980-989 | 运维作业 | archive/backup/trim/audit-verify 触发与状态 |
| 990-999 | 诊断 | 环回压测帧（990 已定义，handler 未实现，P3-4） |
| ≥1_000_000 | 限流器内部合成 | 限流器内部合成 kind：`limit.KindPriority`（Envelope.Flags 的 PriorityFlag 映射），**非线上帧**（P2-3） |

## 跨层全链路时序

**消息发送**（trace_id 前端生成，经 FrameMeta 全程透传）：

```
前端 MSG_SEND ─WS─► usergw: acceptor→codec→limiter(令牌桶)→router(非白名单,转发)
  →forwarder Wrap(route+ctx{conn_id,user_id,device_id}) ─socket─► Bridge
  Bridge: FrameRW→Verifier(会话READY+能力)→Router(kind 101→data层)→Bus(topic: conv.{cid})
  →Egress ─socket─► data层: connector→dispatcher→cache.member.Check(越权拦截点)
  →seqalloc(区间分配seq+snowflake msg_id)→dedupguard(client_msg_id幂等)→writech
  →flusher(1000条/50ms组提交, SQLite事务)→outbound 103 MSG_ACK{msg_id,seq}
  ──► Bridge Router(kind 103→user层 + 103 投递→conv.{cid}订阅者)
  ──► usergw router 查session表 → 全设备投递 MSG_NEW + 发送者收 MSG_ACK
  ──► 对端前端 sync引擎(seq连续性校验)→先写IndexedDB→进内存store→渲染
```

**封禁**：

```
adminctl users ban 123 / 后台按钮 ─HTTPS─► admin: authn→rbac→csrf→handler
  →auditshim(装饰器自动产审计事件)→bridgeproxy ─HTTP─► Bridge AdminAPI
  →翻译成5xx帧注入Bus→Router分发:
   ├─→data层 960: 写ban记录(热库)+审计(冷库sys.audit_log哈希链)
   └─→user层: router 查session→向uid全部连接发KICKED(reason)→断连
  ←ack汇聚→AdminAPI→HTTP响应；审计可journalctl式顺序回放
```

**重连补拉**：断线→指数退避→WS重连→upgrade前鉴权→RESUME(last_seq)→经桥接 920 读→readrouter 热库补 gap→逐帧下发→sync 引擎去重重组。

## 总目录结构（Monorepo）

```
/UFOT
├─ proto/                          # 唯一契约源(所有kind定义+Envelope)
├─ server/
│  ├─ cmd/
│  │  ├─ chatd/          # embedded 单进程装配(只装配、零业务规则)
│  │  ├─ chatd-bridge/ chatd-user/ chatd-data/ chatd-admin/   # split 多进程 —— 见下方 ⚠️(v1.0 未实现)
│  │  └─ ufotctl/ adminctl/          # 两件CLI(ufotctl=系统级运维, adminctl=管理面API; 均API客户端,非旁路)
│  ├─ internal/
│  │  ├─ proto/ pipedef/            # Frame契约 + Stage接口
│  │  ├─ transport/remote|embedded/ # 层间socket双形态(同代码两部署)
│  │  ├─ bridge/framerw|verifier|router|egress|ack|session|adminapi|supervisor/
│  │  ├─ bus/                       # topic trie+扇出
│  │  ├─ user/net/{ws,bridge,frame,limit}|core/{session,authn,router,policy}/
│  │  ├─ data/{conn,pipe/{seqalloc,dedup,flusher},read,cache,policy,job,domain/*,store/{sqlite,pg}}
│  │  ├─ admin/{middleware,handler,service,bridgeproxy,auditshim}
│  │  └─ pkg/{log,signalset,featureflags,exitcodes}
│  └─ store/migrations/{hot,cold,admin}/{域}/{NNNN}.sql   # 分域编号迁移
├─ apps/client/                    # 用户前端 ui→core→net
│  └─ src/{net/{transport,proto,protocol,sync,auth,cache},core/{stores,selectors},ui/{views,renderers,components}}
├─ apps/admin/                     # 管理后台前端
├─ config/chatd.toml policy.json
└─ deploy/systemd/ Caddyfile release.sh backup.sh
```

> ⚠️ 未实现（v1.0）：`chatd` 仅 embedded 模式——`server/cmd` 实际只有 `chatd`、`ufotctl`、`adminctl` 三个二进制，`server/cmd/chatd/main.go` 对非 embedded 的 `[mode]` 直接报错（this build is embedded-only），不存在 `chatd-bridge/chatd-user/chatd-data/chatd-admin`。`deploy/systemd/ufot-{bridge,user,data,admin}.service` 的 `ExecStart` 指向不存在的 `bin/chatd-*`，split 的 4 个 systemd unit 为 1.1 预留，**启用会启动失败** — 计划 v1.1（P1-5/P1-6）。

## 贯穿全系统的设计律（CI 强制）

1. **单一职责**：每组件一包一职责表，import-lint 禁止兄弟组件互相 import、禁止反向依赖（`cmd→app→core→net`）（已落地：`scripts/importlint.sh`，挂 `make lint` 与 CI）
2. **一切皆帧**：控制/数据/管理/指标全是 Envelope，一套编解码日志 trace
3. **机制策略分离**：`policy.json` 是策略真源（retention/限流/归档 cron），代码出现"90天/4:00"常量即评审打回；feature flag 即策略数据
4. **crash-only**：所有层零必须存活状态，故障=systemd 1s 拉起+RESYNC 恢复；唯一例外 AckTracker 不变量破坏 exit(70)
5. **Linux 契约**：SIGHUP 白名单热重载、SIGTERM 排空（数据层 flusher 最后组提交+归档断点续跑）、日志 stdout→journald、退出码 sysexits、所有操作有 CLI 等价物+`--dry-run`、审计=append-only 哈希链+顺序游标读
6. **幂等**：写操作 Idempotency-Key、client_msg_id 去重、归档可重跑

## 部署形态

- **embedded**（2C2G 起步）：单 `chatd` 二进制，四层进程内流水线+EmbeddedTransport（chan 直连），一个 systemd unit
- **split**（扩展期，⚠️ v1.0 未实现，见上方目录结构注记）：同代码编译出 5 个二进制分机部署，层间改走 UDS/TCP socket——只改配置 `[mode]` 段
- 冷库 Postgres 始终在外部，本机内存与用户量增长解耦

## 实施顺序

0. `proto` 全 kind 定义 + `policy/policystore` + 分域迁移骨架（协议与地基）
1. Bridge 流水线 + EmbeddedTransport + embedded 装配
2. data 层写管道+读路由（SQLite）+ 用户身份/登录链路
3. usergw + 前端 sync 引擎（消息闭环）
4. 媒体直传 + admin 层 + 审计
5. 归档/备份 job + 冷库 + CLI 两件套（`ufotctl` + `adminctl`）
6. 压测（`ufotctl bench`，v1.0 未实现 P3-4）→ 2C2G 调优（GOMEMLIMIT、journald 限速、SQLite cache）
