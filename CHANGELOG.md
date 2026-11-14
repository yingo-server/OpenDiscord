# 变更日志

本文件记录 OpenDiscord 的版本变更。格式参考 Keep a Changelog，版本号遵循语义化版本。

## [1.4.0] - 未发布（工作区进行中，验收以 CI S-1~S-7 为准）

1.4 LTS 主题：普遍拉取、参数自适应、v3 消息总线三条核心，加功能补齐与运维收敛。
**本段为进行中状态：所有新投递/消息域行为均 flag 默认关闭，主线行为与 1.2.1 一致；
未压测项一律不算发布完成。**

### Added
- **普遍拉取模式（CORE-2）**：`universal_pull_enabled`（默认 false）取消
  `pull_mode_threshold` 分流语义，所有会话统一走「水位线通知 + 客户端批量拉取」，
  inline 逐成员推送仅作回滚路径（逐字节一致，T-SV-5 常驻回归）。机制层参数
  硬界 batch 1~200 / interval 10~2000ms。
- **参数自适应控制器（CORE-1）**：`server/internal/bus/adaptive` AIMD + 双维
  控制器（每会话 batch/interval，变更限速 25%、冷却 1s、SafeTick panic 旁路
  回退静态默认）。控制循环（chatd/adaptive.go）已接线，`adaptive_control_enabled`
  （默认 false）时不启动/不写，开启前机制层用静态默认 50/2000ms；热生效单测待补。
- **供应商适配器（OPS-2）**：SMSSender / Mailer / CaptchaVerifier 通用 HTTP
  webhook 实现（POST JSON、3s 超时、非 2xx 失败）+ `deploy/providers.example.toml`
  示例（阿里云/腾讯云/通用，不含真实密钥）；生产未配置保持 fail-closed。
- **TURN 自检（OPS-2）**：管理面 `GET /api/v1/diagnostics/turn` 连通性探测
  （3s 超时，仅管理面权限可见），不新增端点前缀。
- **`opendiscordctl upgrade`（OPS-1）**：一次性升级计划合并 auto_vacuum 转换
  与旧 thumb_data 迁移，dry-run 默认、`--run` 执行。
- **`chatd-pg` 构建变体（OPS-1）**：内嵌 pgx driver 的冷库变体，启动自检并落
  审计；普通 `chatd` 仍不 import driver。
- **前端功能补齐（FE-1/FE-2）**：106 编辑 / 109 转发元数据 / @ 提及广播 /
  453 举报入库的服务端实现与客户端接线（`msg_domain_v2_enabled` 门控，默认 false）；
  会话列表虚拟滚动（react-virtual，无门控）；PWA（manifest + service worker，
  production-only 注册）与 Web Push VAPID 订阅封装（服务端接口未实现时 fail-closed）。
- **压测场景（REL）**：`scripts/stress` 新增 `--universal-pull` 模式（水位线驱动、
  水位线/秒、SYNC QPS、收敛延迟、下行帧三分统计）、`--slow-ratio/--slow-factor`
  慢消费者注入（S-4）、重启风暴复用（S-5）、`--adaptive-compare` static vs adaptive
  对照输出（S-6 判定证据）；stress workflow 增加 dispatch 输入（不改 ci.yml）。

### Fixed
- 删除指向不存在二进制的 systemd 多进程单元（bridge/user/data/admin），
  不再留会启动失败的运维指引；文档明确 embedded-only。
- 会话列表上千会话时首屏 DOM 偏多：接虚拟滚动。
- Windows ARM/ARM64 由「无产物」收敛为 CI 显式 skip + 源码构建文档（不再含糊）。

### Experimental（全部 flag-off，代码在位·待 CI/压测验证，发布前须 S-1~S-7 守门）
- `universal_pull_enabled=false`：普遍拉取分流已实现，未灰度、未压测。
- `adaptive_control_enabled=false`：控制器与控制循环均已落盘；热生效单测与
  S-6 对照压测未跑，开启前无行为。
- `v3_bus_enabled=false`（percent=0）：MailboxHub / DeliveryPlane / 双写旁路 /
  客户端 kind 15/16/17 在位；钩子注册、灰度关旧直投、kind 15 上行路由、背压
  指标 dump 四项接线**已落盘**，按 uid 单一路径判定避免双投递。控制器热生效
  单测、灰度切换全链路不双投递 e2e 断言未写，最终编译未复跑（交 CI）。
- `msg_domain_v2_enabled=false`：106/109/@/453 服务端实现已落，未灰度。
- Web Push 服务端（`/api/push/subscribe` 与 push 事件下发）未实现，前端
  fail-closed；手机锁屏通知在 1.4.0 内仍未解决。
- 群人数默认上限保持 100（放大到 1000 的决策待三核心压测达标后执行）。

### 明确不做（1.4 范围内，单列以免含糊）
朋友圈/动态、支付/红包、公众号/小程序、自建 SFU 群通话、split 多进程部署、
端到端加密（E2EE）——理由见 `docs/roadmap.md` 与 `docs/known-issues.md`。

## [1.2.1] - 2026-11-07

项目更名 OpenDiscord 后的首个可用版本。**v1.2 拉取模式 + 全部稳定性修复；v3 消息总线代码在位但默认关闭（零行为影响）。**

### Changed
- 项目更名 OpenChat → OpenDiscord（module、二进制、服务名、环境变量；
  旧 `OPENCHAT_*` 环境变量仍兼容读取）。
- 发送路径回到实测最优形态（负优化的 per-conn ring/假合批/去锁实验全部回滚）。

### Fixed
- 前端长按菜单 5 个「即将推出」占位（撤回/转发/多选/会话开关）接线。
- 撤回幂等（二次撤回不再回误导性 404）；recalled 在实时与历史两条下行路径均携带。
- 扇出成功路径漏 `releaseShared()` 的引用计数契约修复。
- v3 P3a 信用流控测试断言（ACK 不再凭空推进 cursor）与预存 flaky 测试。

### Internal（默认关闭，不影响运行）
- v3 消息总线 P0~P3d-1：MailboxHub、DeliveryPlane credit 流控、灰度双写旁路、
  客户端 kind 15/16/17、连接生命周期钩子机制。`v3_bus_enabled=false` 且
  所有新投递循环无人调用；旧直投仍是唯一投递路径。

### 已知限制（实测，README / known-issues 有完整数据）
- 2 核稳定工作点：512 并发 / ~3,500 消息每秒 / p95 ~120ms。
- 1000×500 人大群拉取模式 65% 可用性（基线 2%），未达 95% 目标；
  1000×50 人仍 inline 约 14~24%。大群正解是 v3 总线（开发中）。

## [1.2.0] - 2026-11-02

大群拉取模式（Pull Mode）。**单阶段启用，无灰度**。

### Added
- kind 14 `WATERMARK`：>100 人群不再逐成员推送 MSG_NEW，改为 batch=50 / 2s
  兜底的水位线通知；客户端收到后按 `lastSeq+1` 发 SYNC(8) 批量补拉。
  500 人群的下行帧需求从 ~1M/s 降到 ~2k/s。
- 客户端 `pull-scheduler`：singleflight + 2s jitter + 1s min_interval + 10s
  兜底，500 人同时收到水位线不会惊群；结果只经 sync 引擎单一写入口。
- 压测工具 `--pull-mode` 与 `-mode restart-storm`（S-5 拉取风暴验收）。
- 202 全量成员快照对大群改为 `members_omitted`，消除 64KB 帧上限风险。

### Safety
- 客户端 handler 先行落地后才启用服务端（RESYNC 事故教训制度化）。
- 99 人群逐字节回归（T-SV-1）；热回滚只需 `pull_mode_threshold=0`。

## [1.1.2] - 2026-11-02

性能实验回滚 + 前端功能补齐 + 语义修复。**无破坏性变更**，可直接升级。

### Reverted（性能实验，均被压测证伪）

- **消息合批**（把多条 Envelope 拼进一个 WS message）：protobuf wire 格式
  跨帧不自描述，相邻帧的字段会互相覆盖且尾部余量被静默丢弃——e2e 全线
  超时（客户端等不到已被合并掉的帧）。已回退为逐帧 `WriteMessage`。
- **慢消费者降级（LAGGING/RESYNC kind 14）**：降级依赖「客户端收到 RESYNC
  后自行拉取 gap」，而前端当时没有任何 RESYNC handler。500 人群压测中
  可用性从 2.2% 掉到 0.9%，约 6.9 万个 ACK 在 `sendqueue.push` 静默蒸发。
- **发送路径的其余"优化"**（无锁扇出 + 1024 有界环 + 预编码共享）：512×10
  基线从 98.1% 退化到 73.0%，p95 从 126ms 恶化到 288ms。整体回退到 v1.1.1
  的 `sendMu` 单锁 + 普通 chan + 逐帧写形态后恢复到 **91.7% / p95 120ms**。

完整证据链见 `docs/perf-postmortem-v1.2-draft.md`。**教训已制度化**：优化的
前提假设必须先用计数器证明，收益必须用同场景前后对照压测验证。

### Added（前端功能补齐）

- **撤回**：长按菜单可撤回 2 分钟内的自己消息（接服务端 108 链路，本地乐观 tombstone）
- **转发**：消息/多选 → 会话选择 → 以 `[转发]` 前缀或原媒体重新发送
- **多选**：checkbox 模式 + 已选计数 + 批量转发
- **上滑加载历史**：三段式（IndexedDB → 920/921 网络 → 3s 超时降级）+ 按滚动
  速度动态预取（200–800px）+ 滚动锚定（插入旧消息时视口零位移）+ 已到底标记
- **会话置顶/静音**占位按钮接到已有能力

### Fixed

- **撤回幂等**：重复撤回同一条消息不再返回误导性的 404 "no such message"，
  改为成功 ACK 并带 `already: true`（不重复广播、不重复审计）
- **fc.get 空 slice 防御**：`len(b)==0` 前置，避免理论上的越界 panic
- **flusher 引用泄漏**：`putBulk==nil`（测试装配）时释放共享引用
- **pipe 测试死锁**：排水 goroutine 的退出条件与 `TestPeer.Close()` 提供的
  信号不匹配，修正为 `tp.closed`

### Changed（数据与文档）

- README / known-issues 的容量表改为三列对照（v1.1.1 基线 / 负优化版 / 当前），
  实测数字：512×10 = 91.7%（p95 120ms）、1000×50 = 23.7%、1000×500 = 2.0%
- SQLite 写路径基准结论入库（`docs/perf-sqlite-write-path.md`）：mmap / 大 cache
  在内存环境下**反而更慢 13–27%**，全部候选被数据拒绝；批大小甜点 1000
- 新增 `docs/pull-mode-spec-v1.2.md`（大群拉取模式规范）与
  `docs/perf-spec-v1.2.md`（性能优化规范，含 6 项"不做"清单）
- known-issues 新增：用户举报（kind 453）服务端无处理分支

### 升级注意

无 schema 变更、无配置键变更、无协议破坏性变更。`pull_mode_threshold` 默认 0
（拉取模式客户端已就绪但服务端功能未启用，等 v1.2 灰度验证）。

## [1.1.1] - 2026-11-01

### Fixed
- **群成员默认上限从 500 降为 100**。压测证据：2 核机器上 1000 并发连接时，
  10 人群可用性 98%、50 人 32%、500 人仅 2.2%。旧默认值向运维承诺了一个
  参考硬件服务不了的规模。新默认值按帧扇出预算（约 4 万帧/秒）取值，
  `docs/known-issues.md` 补了完整的容量核算公式与实测表。

### Documented
- README 与 known-issues 增加「1000 人规模实测」章节（两个群聊 × 500 人的
  真实数据），明确「连接数不是瓶颈，帧扇出总量才是」。

## [1.1.0] - 2026-10-31

消息闭环、媒体、通话与管理界面的一轮功能补齐，并修复了 4 处安全缺陷与 8 处链路断点。升级必读见 `docs/release-notes-v1.1.0.md`。

### Added

**消息闭环（服务端已实现，含 e2e 测试）**
- 已读/送达回执（kind 104/105）：客户端发回执，服务端校验成员身份后广播给会话成员。
- 输入中提示（kind 230）：typing 状态在会话内转发，支持 `typing=false` 及时停止。
- 在线状态（kind 400/401/402）：连接建立/断开时向会话同伴广播上线/下线。
- 会话成员变更通知（kind 212）：加人/踢人/改角色实时通知会话全员。
- 消息撤回（kind 107/108）：2 分钟时间窗内本人或管理员可撤回，软删落库并对全员广播。

**媒体**
- 客户端生成图片缩略图（Canvas 缩放，超过 8KB 自动降质）与语音波形（WebAudio 峰值包络）——服务端无需 ffmpeg，2C2G 机型友好。
- 上传接口支持 `thumb_data` / `waveform` 字段，并做 base64 解码 + 图片 magic bytes 内容校验。

**通话**
- WebRTC 1v1 语音/视频：服务端只做信令转发（kind 350–354），媒体流 P2P 直连，服务器零带宽。
- 通话状态机：忙线自动回 busy、60s 无应答自动挂断，受 feature flag `call_enabled` 门控。
- 前端全屏通话 UI：来电/去电/计时/静音/免提/挂断，45s 超时与 ICE 失败提示。

**交互与界面**
- 双模式交互：flag 是唯一真源，TTY 下自动进入交互向导，非 TTY 走纯参数模式（缺参退出码 64 并列出必填项）。
- 中英双语：`--lang` 覆盖，未指定时按 `LANG`/`LC_ALL` 自动检测。
- `chatd --tui` 运行仪表盘（健康/policy/flags，热键 q/r/d）。
- opchat 纯 TUI 管理控制台：7 个 tab（概览/用户/举报/广播/Flags/归档/审计）、数字滚动动画、终端 bell 声效、破坏性操作二次确认。
- 管理后台 WebUI：Dashboard 首页（指标卡 + 实时事件流 + sparkline）、数值滚动动画、WebAudio 合成声效、轮询可暂停。
- `install.sh` 新增 `--domain`/`--admin-user`/`--admin-pass-file`/`--cold-dsn`/`--turn-server`/`--lang`；`deploy/install-interactive` 提供 7 步向导。

**工程**
- 全平台构建矩阵：Linux x86_64/x86/arm64/armv7/riscv64 + Windows x86_64/x86/arm64/arm + 源码包。
- 压测 workflow：并发阶梯自动加压直到可用性 <70%，统计全局 RPM 与延迟分布。
- 文档：`api-reference` / `cli-reference` / `ws-protocol` + `scripts/doccheck.sh` 自动校验（文档里的端点/命令/kind 必须能在源码找到）。
- `import-lint`（`scripts/importlint.sh`）落地为 CI 步骤（此前只是文档里的承诺）。

### Security

- 冒名操作：数据层曾信任客户端信封里的 `user_id`，攻击者可冒用他人身份撤回消息、伪造"已读"回执。现操作者身份只来自会话，信封里的 `user_id` 一律不采信。
- 伪造在线状态：401/402 事件曾可由客户端上行，任何人可伪造他人上线/下线。网关现拒绝客户端发来的 presence 事件（返回 4003）。
- 生产环境限流失效：`rate_limit_enabled` 结构零值为 false，生产配置可能因此关掉全部限流。现生产环境检测到 `rate_limit_enabled=false` 直接拒绝启动。
- 缩略图可存任意内容：`thumb_data` 曾只校验长度，可写入 SVG/HTML data URI。现校验 base64 解码 + 图片 magic bytes。

### Fixed

- 裸帧丢载荷：`ShellFrameSource` 曾丢弃非内部帧的 payload，在线状态类事件被解析成 `uid=0` 后静默丢弃；现完整透传 payload。
- 广播帧路由回环：104/105/107/108/230/400–402 缺 data→user 路由覆盖，广播帧被路由回数据层自身、对端收不到；现补齐路由。
- 媒体下行空消息：前端 `media.mtype` 发字符串、服务端按整数解析，整条载荷解析失败致对端收到空消息；现两端对齐为整数。
- 在线状态语义反转：presence 解码只认 `status` 字符串，上线事件被判成下线；现按事件 kind 判定上下线。
- 撤回跨设备失效：历史拉取不返回 recalled 标记，换设备后撤回消息原文重现；现历史与实时都带 `rn` 撤回标记。
- `install.sh --domain` 未生效：参数解析后被丢弃，`public_origin` 为空、同源校验与 Secure Cookie 判定走宽松分支；现 `--domain` 真正写入 `public_origin`。
- CLI flag 顺序：Go flag 遇位置参数即停止解析，`users ban 42 --level 2 --days 7` 实际发出默认值（"禁 7 天 2 级"变成"永久 1 级"）；现解析前先提升 flag token（`opendiscordctl`/`adminctl`）。
- 压测假通过：可用性分母为 0 时算成 100%，什么都没测到却判 PASS；现分母为 0 直接判失败。

## [1.0.0] - 2026-10-29

首个正式版本：`chatd` embedded 单二进制可部署，含用户端/管理端 SPA、管理 REST、媒体上传下载与冷热分层存储。

### Added

- 协议与工程骨架：`proto/chat.proto`（72 条消息）、kinds 常量注册表（78 个 kind）、Frame/FrameMeta/Reject 管道类型、Makefile（proto/build/test/lint/release）与 GitHub Actions CI。
- 文档：架构总纲、桥接/用户/管理/数据层设计、Web UI 规范与限流/安全规范。
- 总线与桥接层：进程内/远程 Peer（TCP/UDS）、HMAC 握手（双钥轮换、±30s 窗口、10 万 nonce 防重放）、8 组件桥接流水线（framerw/verifier/router/egress/ack/session/adminapi/supervisor）、16 路总线 fanout、有界队列背压与优雅排空。
- 数据层：SQLite 热库（WAL、`RETURNING` seq 分配、keyset 分页）、PostgreSQL 冷库（分区归档、trgm 全文检索、哈希链审计）、21 张分域表与版本化迁移、policy 热加载、写管道（seqalloc/dedup/flusher，1000 条或 50ms 组提交）。
- 用户层：WebSocket 网关（升级前 Cookie 鉴权、按 kind 令牌桶、16 路会话分片、七步优雅排空）；`/api/auth/*` 认证服务（手机 OTP、邮箱 magic link、密码、游客、refresh 轮换与重放检测、Ed25519 签发、登录锁定 5 次→15m/1h/24h）。
- 管理面：独立 `admin.db`、TOTP + RBAC 三角色、常量时间 CSRF 校验、幂等键、审计游标、管理 REST（`/api/v1/*` 挂载在 `admin_rest_listen`）、`adminctl` / `opendiscordctl` 运维 CLI。
- 前端：用户端 SPA（net/core/ui 分层、IndexedDB 单一真源、seq 收敛与 gap buffer、乐观 outbox、渲染器注册表、消息列表虚拟滚动）与管理控制台；两端产物经 `go:embed` 内嵌进单二进制。
- 媒体：`POST /api/media/upload` 与 `GET /api/media/{id}`（magic bytes 校验、扩展名一致、类型/大小上限、每日 50 次限流、内容寻址去重）。
- 部署件：systemd 加固单元、Caddyfile、install/backup/release 脚本、`opendiscord-<ver>-linux-<arch>.tar.gz` + `SHA256SUMS` 发布包。

### Fixed

- 浏览器 JSON 文本帧被网关丢弃导致整条浏览器链路不可用：codec 增加 JSON/protobuf 嗅探并按对端格式回包。
- 管理面能力集缺少 960/970/980 kind，ban 帧被 verifier 拒绝。
- `adminctl flags` 对 PUT 路由误用 POST；embedded bus 在瞬时无接收者时丢帧。
- 数据层评审问题：帧内容被 JSON 嵌入导致回复载荷丢失、解码器先读 magic 再读完整头、控制面帧混入数据流、TeeStage 未关闭输出、archiver 无进展时可能死循环。
- 构建/CI：`make proto` 中 `exit 0` 只结束单行导致 protoc 仍执行；`VERSION` 因 `make web` 重写 `server/web/*` 而带上 `-dirty`。
- 文档：README 的发布资产名与 `SHA256SUMS` 校验命令对齐实际 release 产物。
- CI `-race` 竞态：`admin.Server.Addr`、`adminPlane.addr`、`ws.Acceptor` 监听器注册、`OnResync` 回调、supervisor reload 测试与测试就绪等待。

### Security

- 认证：Ed25519 access（15m）/refresh（30d），refresh token family 轮换与重放撤销；PII 仅保存 HMAC 索引，明文不落库；登录失败锁定 5→15m/1h/24h。
- 管理面：强制 TOTP、RBAC 三角色、常量时间 CSRF、登录失败指数锁定；管理监听仅回环（`admin_listen` / `admin_rest_listen`）。
- 上传：magic bytes 白名单 + 扩展名一致 + 可执行/脚本/SVG 黑名单；图片 8MB / 音频 2MB / 文件 20MB 上限，每用户每日 50 次。
- 审计：冷库 append-only 哈希链；无冷库时显式告警并降级为「只写日志不落盘」，不静默伪造。
- 限流：OTP 防轰炸（60s 间隔、5 次/小时、10 次/天、单 IP 5 次/小时、3 个来源 IP 冻结 24h）；captcha 自当日第 2 次起强制，未接供应商时 fail-closed。
- 层间传输：HMAC 握手（双钥轮换、±30s 窗口、nonce 防重放）、UDS 0600。

## [0.1.1] - 2026-10-29

### Fixed

- 发布构建：`VERSION` 不再因 `make web` 重写 `server/web/*` 而带上 `-dirty`，发布资产名固定为 `opendiscord-<ver>-linux-<arch>.tar.gz`。

## [0.1.0] - 2026-10-29

### Added

- 首个可安装的 embedded 骨架：协议契约、分域迁移、总线/桥接/数据/用户/管理各层主体、用户端与管理端 SPA、systemd/Caddy/install 等部署件。
- 发布链路：tag push 触发 CI 构建 `opendiscord-<ver>-linux-<arch>.tar.gz` 与 `SHA256SUMS`，并作为 GitHub Release 资产发布。

### Fixed

- CI 发布 job 补充 `contents: write` 权限，GitHub Release 资产上传不再返回 403。
