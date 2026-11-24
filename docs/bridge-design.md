# 总线与桥接层设计

> 设计总纲：Do One Thing Well ／ Composability ／ Everything is a Frame ／ Crash-Only ／ Text-First Observability ／ Signals as API
> 对齐版本：v2.1.0（embedded 单进程为唯一已部署形态；`chatd-bridge`/`chatd-ctl` 等 split 命名为设计期遗留，
> 实际二进制只有 `chatd`、`chatd-pg`、`adminctl`、`ufotctl`）。

## 1. Unix 哲学映射表

| Unix 原则 | 落点 |
|-----------|------|
| Do One Thing Well | Bridge 拆为 8 个单一职责组件，每组件一包一测试入口 |
| Composability（管道优先） | 帧走显式 6 级 pipeline，级间只有 `chan *Frame`，任何一级可独立替换/旁路/内嵌探针 |
| Everything is a file | **Everything is a Frame**：控制面/数据面/管理面/观测面全部是同一种 Envelope 帧 |
| Text streams | 日志只写 stdout(JSON-lines) 交 journald；trace_id 可 journalctl -g 直接 grep |
| Small utilities | `chatd-ctl` 小命令行工具；每层一个独立二进制 |
| Crash-only | 组件 panic→进程退出→systemd 拉起→REQ/ACK 补发恢复 |
| Signals as API | SIGHUP=热重载，SIGTERM=优雅退出，SIGUSR1=状态 dump |
| Mechanism, not policy | Bridge 只提供转发机制；策略在用户层/管理层 |
| Simplicity before generality | v1 不做集群路由、不做磁盘队列、不做 mTLS |

## 2. 组件化设计

### 2.1 一条流水线，七个工人

```
                          chatd-bridge 进程内部（数据面流水线，东西流向）
┌────────┐   ┌────────┐   ┌────────┐   ┌────────┐   ┌────────┐   ┌────────┐
│FrameRW │──▶│Verifier│──▶│Router  │──▶│ Bus    │──▶│Egress  │──▶│ FrameRW │
│(编解码) │ch │(鉴权)   │ch │(路由)  │ch │(分发)  │ch │(背压)  │ch │(写出)   │
└────────┘   └────────┘   └────────┘   └────────┘   └────────┘   └────────┘
     ▲ 唯一接触网络的一级                                    ▲ 与 FrameRW 共享连接写锁
     │
     │ 控制面（旁路，不走 Router/Bus）：HELLO/SUB/ACK/PING
     ▼
┌─────────────┐      ┌──────────────┐      ┌───────────────┐
│Session      │      │AdminAPI      │      │ Supervisor    │
│Manager(连接) │      │(HTTP→帧注入)  │      │(信号/生命周期)  │
└─────────────┘      └──────────────┘      └───────────────┘
```

**数据面与控制面严格分离**：HELLO、SUBSCRIBE、FRAME_ACK、HEARTBEAT 不进 Router/Bus 流水线——由 SessionManager 在 FrameRW 之后直接消费。

### 2.2 单一职责定义表（god-component 禁令）

| 组件 | 包 | 唯一职责 | 明确不做的（禁令） |
|------|-----|------|---|
| **FrameRW** | `bridge/framerw` | 字节流 ↔ `Frame` 编解码；连接读写循环 | 不看 kind 语义、不鉴权、不限流、不路由 |
| **Verifier** | `bridge/verifier` | 校验帧是否允许转发：层能力表（`LayerCaps`：layer → kind 集合）、来源层注册 | 不路由、不做用户权限、不解析 payload |
| **Router** | `bridge/router` | 纯函数查表：`(kind, header) → []Target` | 不发帧、不记状态、不知道 Bus 存在 |
| **Bus** | `internal/bus` | topic 订阅表 + 扇出 | 不落盘、不去重、不背压决策 |
| **Egress** | `bridge/egress` | 每连接有界发送队列 + 水位检测 + kind 分级溢出策略 | 不解析帧内容、不重试 |
| **AckTracker** | `bridge/ack` | seq 确认/超时/重发/接收端去重窗口（传输可靠性） | 不管业务 request_id 配对 |
| **SessionManager** | `bridge/session` | 连接生命周期：accept、HELLO 握手、能力存储、心跳判死、订阅 TTL | 不转发任何数据面帧 |
| **AdminAPI** | `bridge/adminapi` | HTTP 请求 → 帧对象的翻译器（唯一非 socket 入口），JWT/TOTP 校验 | 不执行管理动作本身 |
| **Supervisor** | `bridge/supervisor` | 信号处理、生命周期、优雅排空 | 不碰任何帧 |

**验收标准**：任何组件单测的 import 不得出现兄弟组件实现包。CI 加 import-lint 强制（已落地 `scripts/importlint.sh`，挂 `make lint` 与 CI）。**豁免范围**：仅 `internal/bridge` 根包的装配级测试（如 `pipeline_test.go`，需看到全部被装配组件），组件子包测试不豁免。

### 2.3 级间契约

```go
// 全流水线唯一流通货币（定义在 internal/proto）
type Frame struct {
    Seq     uint32
    Kind    uint16
    Envelope *Envelopepb.Envelope
    Meta    *FrameMeta   // trace_id、入口连接、入队时间戳——FrameRW 打上，全程只读
}
```

可组合性收益：
1. **可旁路**：Router 换成 echoRouter 可单独压测 Verifier
2. **可插探针**：任何两级之间插入 teeStage，不改相邻组件
3. **可内嵌**：EmbeddedTransport 只需提供 `chan *Frame`——embedded 模式=把 FrameRW 换成内存队列，其余六级照跑
4. **可替换**：Router 表是启动时 TOML 加载的纯数据

### 2.4 每级失败行为

| 组件 | panic（崩进程） | 降级（记日志继续） |
|------|------|------|
| FrameRW | — | 非法 magic / 超长帧(>256KB) → 断开该连接（连接是沙箱） |
| Verifier | — | 未 HELLO 发数据帧→断连；kind 超能力→丢帧+NACK |
| Router | 无路由条目 → NACK(NO_ROUTE) | — |
| Egress | 控制帧入队超时(200ms)→判连接死 | 可丢帧按策略丢弃 |
| AckTracker | 不变量断言失败→**exit(70)** | RTO 重发、判死 |
| SessionManager | — | 心跳 3 次超时→断连 |

原则：**连接级错误断连接，进程级错误崩进程**。

## 3. 一切皆帧

| 传统做法 | 本设计 | 收益 |
|---------|--------|------|
| 控制消息独立 API | HELLO/ACK/SUB/PING 都是 kind 9xx 帧 | 一套编解码/日志/metrics/trace 全覆盖 |
| 管理指令走内部 RPC | AdminAPI 把 HTTP 翻译成 5xx 帧注入 Bus | 管理指令审计=帧日志，与消息同构 |
| metrics 主动上报 | Bridge 指标=bridge.internal topic 的 913 TOPOLOGY 帧，exporter 是订阅者 | 指标管道与数据管道同一套代码 |
| 健康检查 HTTP /health | v1.0 保留 HTTP `/healthz`（探针生态刚需，user 端口与 admin 端口各挂一个）；HEARTBEAT 帧方案为 split 形态预留，systemd watchdog 由 Supervisor 转译（P2-4） | 探针可直接 HTTP 探活 |
| feature flag 拉取 | FEATURE_FLAGS 帧(kind 911)广播到 `sys.>` | 已有机制，无新增 |

每个连接是 Bus 上的"文件描述符"：订阅=open(topic)，退订=close，订阅 TTL=租约。SessionManager 长得像小内核的 fd 表。

## 4. 进程与包结构

```
chatd/
├── cmd/
│   ├── chatd/main.go              # embedded：装配 4 层模块（只装配，零逻辑）—— 唯一服务形态
│   ├── chatd-pg/                  # 冷库变体（v1.4+）：blank import pgx/v5/stdlib 注入 driver
│   ├── adminctl/  ufotctl/        # CLI 两件套（管理面 API 客户端）
│   └── opchat/                    # 内部工具
├── internal/
│   ├── proto/                     # 唯一契约层（kinds.go 为 kind 唯一真源）
│   ├── pipedef/                   # 流水线 channel 接口类型
│   ├── transport/
│   │   ├── remote/                # UDS/TCP + BUS_SOCKET 编解码（split 预留）
│   │   └── embedded/              # 进程内 chan *Frame 直连（v2.1 实际形态）
│   ├── bridge/
│   │   ├── framerw/ verifier/ router/ egress/ ack/ session/ adminapi/ supervisor/
│   │   └── pipeline.go            # 装配文件（唯一允许 import 全部组件的地方）
│   ├── bus/                       # topic 分片 + 尾通配(.>)扇出
│   ├── user/  data/  admin/
│   └── pkg/{log, signalset, featureflags, exitcodes}
├── config/chatd.toml
└── deploy/systemd/ufot.service    # Type=simple（chatd 不发 sd_notify）
```

**禁令清单（CI import-lint，已落地 `scripts/importlint.sh`，挂 `make lint` 与 CI；豁免仅限 `internal/bridge` 根包装配级测试）**：
- `bridge/*` 组件包互相 import → 编译失败（只允许 pipeline.go 装配）
- `user/ data/ admin/` import `bridge/` → 编译失败（只能 import transport/ + proto）
- 任何包 import `os/signal` 除 pkg/signalset 与 bridge/supervisor → 编译失败
- 任何包直接写文件做日志 → 编译失败

**embedded 模式** = 四条进程内流水线 + EmbeddedTransport：`cmd/chatd/main.go` 读 TOML → 对每个启用层构造 pipeline → 全部接到同一个 bus → supervisor.Run()。装配只做装配、零业务规则（不再承诺具体行数）。

## 5. 信号处理与生命周期

| 信号 | 语义 | 行为 |
|------|------|------|
| **SIGTERM/SIGINT** | 优雅退出 | 广播 910 SHUTDOWN_NOTICE(deadline=now+5s) → 停止 accept → Egress 排空(≤5s) → 退出码 0 |
| **SIGHUP** | 配置热重载 | 重读 TOML → diff → 只应用白名单项（背压阈值/路由表/日志级别/flags）→ 广播 912 CONFIG_UPDATE。语法错不退出，保持旧配置 |
| **SIGUSR1** | 状态 dump | 连接表/队列水位/in-flight seq 打到 stdout，不影响服务 |
| **SIGUSR2** | 预留 GC/heap dump — ⚠️ 未实现（预留） | handlers 未接线（signalset 映射为 Custom，chatd watch 未处理） |
| **SIGQUIT** | 崩溃退出 | goroutine 全栈→stderr，systemd 拉起走 crash-only 恢复 |

退出码契约：0 优雅退出；1 配置不可用；2 装配失败；70 不变量破坏（必须重启清状态）；75 临时失败（端口占用）。

systemd unit：`Type=simple`（chatd 不发 sd_notify，`deploy/systemd/ufot.service` 实际值）、`ExecReload=/bin/kill -HUP $MAINPID`、`Restart=always` + `RestartSec=1`、`LimitNOFILE=16384`。

## 6. 文本可观测：stdout + journald

- 进程不管理自己的日志文件——轮转/压缩/清理是 journald 的事（SystemMaxUse=200M）
- 每组件 logger 自带 `{"c":"egress"}` 组件名——模块归属由包结构自动注入
- 一行一事件（JSON lines）

```bash
journalctl -t chatd-bridge -t chatd-data -g "trace_id=8f3a…" -o cat | jq   # 消息全链路
kill -USR1 $(pidof chatd-bridge) && journalctl -t chatd-bridge -n 200     # 突发排障
```

INFO 转发日志按 kind 采样（消息 10%）；WARN/ERROR 全量；journald RateLimitBurst=2000/30s 防风暴。

## 7. chatd-ctl 管理工具

| 子命令 | 机制（全是已有机制的 CLI 包装） |
|--------|------|
| `chatd-ctl reload` | kill -HUP + tail journal 确认 |
| `chatd-ctl status` | SIGUSR1 dump + systemctl is-active 汇总 |
| `chatd-ctl drain` | 经 AdminAPI 广播 910 预排空 |
| `chatd-ctl peers` | 读 TOPOLOGY 帧打印层拓扑 |
| `chatd-ctl flags [--set k=v]` | flag 读写（AdminAPI→911 帧），落盘回写 TOML |
| `chatd-ctl trace <trace_id>` | 跨全部 5 个 unit grep |
| `chatd-ctl bench --layer data` | 注入诊断帧（990-999）环回压测 — ⚠️ v1.0 未实现（990 落 NO_ROUTE，无 handler，P3-4） |

ctl 只通过信号与 AdminAPI 两个既有通道通信，不额外开 socket。

## 8. crash-only 设计

**能重启解决的绝不用状态机解决。** Bridge 不持有任何必须存活的会话状态：
- 路由表=编译期 kind 段+TOML→重启重建
- 订阅表=各层重连 HELLO+SUBSCRIBE 重建（30s TTL 租约）
- 在线表=用户层内存态，不在 Bridge
- ACK in-flight 窗口=有界内存，丢则上游 RTO 重发覆盖

| 故障 | 恢复 |
|------|------|
| Bridge panic/OOM/watchdog 超时 | systemd 1s 拉起→各层重连→HELLO→RESYNC→重订阅 |
| 数据层落库 hang | 数据层 watchdog（写队列满 30s 无进展）自崩重启；期间 Bridge NACK(TARGET_DOWN) |
| 半写状态（SQLite 事务中断） | WAL 回滚天然恢复——数据库恢复能力替代应用层容错状态机 |
| 网络分区 | 心跳判死断连，同上路径 |

唯一"必须崩溃"状态：AckTracker 不变量断言失败→exit(70)，附完整内存快照（SIGUSR1 dump 复用）。

**明确不做**：无磁盘消息日志/持久队列（数据层 SQLite 是唯一事实源）；无 Raft/选主；无运行时热修复。

## 9. 关键决策理由

1. **组件间 channel 而非函数调用**：channel 让每级有独立调度边界，背压天然沿管道反向传播（Go channel 是 Unix 管道的类型化版本）
2. **日志交 journald**：自管日志=自研轮转子系统，违反 Do One Thing；journald 免费获得索引、检索、配额
3. **SIGHUP 白名单热重载**：监听地址热更需要 fd 转移的高难度机制；白名单收敛到纯内存参数，重载逻辑 <100 行
4. **exit code 契约**：最古老最稳定的进程间契约；systemd Restart 按码分派，避免配置错误被无限重启刷屏
5. **密码学不进热路径**：HMAC 只在 HELLO 握手与重连首帧，数据面 Verifier 只查会话状态+能力集
6. **心跳 RTT 喂 egress 连接健康分**：慢连接（RTT P99>200ms 且队列>firm）提前降级
