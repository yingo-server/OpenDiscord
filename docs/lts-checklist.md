# UFOT LTS 发布前置问题清单

> 来源：三轮独立审查（协议贯通 / 性能资源 / 安全合规）+ 逐项实测复验。
> 状态：✅ 已修并验证 · 🚧 进行中 · ⬜ 待办 · ⏭️ 明确不做（有理由）

## A. 安全（上线阻断）

| # | 问题 | 状态 |
|---|---|---|
| A1 | 操作者身份取自客户端信封 user_id → 可冒名撤回/伪造回执 | ✅ 已修（actor 只来自 session） |
| A2 | presence 401/402 可客户端上行 → 伪造他人在线状态 | ✅ 已修（网关拒绝 + 数据层校验 subject==actor） |
| A3 | rate_limit_enabled=false 无生产护栏 | ✅ 已修（生产环境拒启） |
| A4 | thumb_data 只校验长度，可存 SVG/HTML data URI | ✅ 已修（校验 base64 + magic bytes） |
| A5 | install.sh --domain 解析后未落地；--admin-pass-file 无人读取 | ✅ 已修（写入 ufot.toml；bootstrap 读密码文件，0600/拒符号链接） |
| A6 ✓ | 撤回/成员变更/通话不落审计 | ⬜ 待办 |
| A7 | WS upgrade 限流在 Caddyfile 中整段注释 | ⬜ 待办（应用层已实现，需开 Caddy 侧） |
| A8 | 上传无病毒扫描 | ⏭️ 明确不做（v1.2，需 ClamAV 常驻内存，2C2G 不划算） |
| A9 ✓ | SYNC 游标未服务端签发 | ⬜ 待办 |

## B. 协议贯通（用户可见断链）

| # | 问题 | 状态 |
|---|---|---|
| B1 | 媒体 mtype 字符串 vs int32 → 对端收到空消息 | ✅ 已修（前端发数字） |
| B2 | presence payload 双形态不兼容 → 上线判成下线 | ✅ 已修（decoder 双形态） |
| B3 | presence 无序号 → 乱序永久错 | ✅ 已修（前端 at 单调守卫） |
| B4 | call_enabled 前后端判定不同源 → flag 关了按钮仍显示 | ✅ 已修（isFlagEnabled + 主动请求 flags） |
| B5 | ShellFrameSource 丢 payload → 裸帧 uid=0 被静默丢弃 | ✅ 已修 |
| B6 | buildRoutesFromData 缺 104/105/107/108/230/400-402 → 路由回自己 | ✅ 已修 |
| B7 | 媒体下行缺 thumb_inline/waveform/status | 🚧 进行中 |
| B8 | 历史拉取丢 recalled → 换设备撤回原文重现 | 🚧 进行中 |
| B9 | ConvPayload 缺 version/lastSeq/unread → 新设备不补拉历史 | 🚧 进行中 |
| B10 | usergw 直出帧 protobuf vs 客户端 JSON decode | 🚧 进行中 |
| B11 | receipts 数组只解第一条 | ✅ 已修 |
| B12 | waveform 量纲 0-100/0-1 三方不一致 | ✅ 已修（统一 0-100） |
| B13 | 212 MEMBER_UPDATE 前端无 handler | ✅ 已修（handler 已注册） |
| B14 ✓ | 401/402 命名 SUBSCRIBE/UNSUBSCRIBE 与语义相反 | ⬜ 待办（改名为 PRESENCE_CONNECT/DISCONNECT，保留旧号兼容） |
| B15 ✓ | 撤回 UI 是 "comingSoon" | ⬜ 待办 |
| B16 ✓ | 客户端不发 typing 上行（仅 mock 有） | ⬜ 待办 |
| B17 ✓ | 920/921 未进前端 kinds.ts | ⬜ 待办 |

## C. 性能与容量

| # | 问题 | 状态 |
|---|---|---|
| C1 | 单 dispatcher 内同步 DB 查询 + 同步扇出 | 🚧 进行中 |
| C2 | outbound lane 128 槽 → 1000 人群丢 87% | 🚧 进行中 |
| C3 | egress 队列按 layer 而非 topic → 扇出挤一条队列 | 🚧 进行中 |
| C4 | presence/typing 无节流（99×/999× 放大） | 🚧 进行中 |
| C5 | 成员列表无缓存（每事件一次全量查询） | 🚧 进行中 |
| C6 | 群人数无上限 → 202 快照超 64KB 帧上限（>1090 人必炸） | 🚧 进行中 |
| C7 | 热库永不缩小（归档 SQLite 侧删除缺失） | 🚧 进行中 |
| C8 | trim 作业未实现（policy 声明了但无代码） | 🚧 进行中 |
| C9 | thumb_data 8KB 存 TEXT → 百万张 10.7GB | 🚧 进行中 |
| C10 | SQLite 无连接池上限/无 cache_size/无 WAL 上限 | 🚧 进行中 |
| C11 ✓ | 通话状态 map 泄漏（answered call 永不清理）→ 用户永久 BUSY | ⬜ 待办 |
| C12 ✓ | 每呼叫一个 time.AfterFunc（1000 并发 = 1000 timer） | ⬜ 待办 |
| C13 ✓ | callId 不校验唯一性 → 可覆盖他人呼叫 | ⬜ 待办 |
| C14 ✓ | SDP 无独立大小上限 | ⬜ 待办 |
| C15 ✓ | L3 用户级惩罚未实现（overWindows 死字段） | ⬜ 待办 |
| C16 ✓ | 通话信令 350-354 无限流规则 → 落默认 10/s 打断建连 | ⬜ 待办 |
| C17 ✓ | MaxPayload=0 语义反了（注释"无 payload"，实现"不限制"） | ⬜ 待办 |

## D. 压测与证据

| # | 问题 | 状态 |
|---|---|---|
| D1 ✓ | 压测 ramp 每个 worker 注册一次 → 测的是注册吞吐不是 WS 容量 | ⬜ 待办 |
| D2 ✓ | p95 采样取共享原子变量 → 100 个点几乎同值，无统计意义 | ⬜ 待办 |
| D3 ✓ | avail% 只算 WriteMessage 失败 → 服务端丢帧不算失败 | ⬜ 待办 |
| D4 ✓ | 只测 MSG_SEND，新增 4 条广播路径零覆盖 | ⬜ 待办 |
| D5 ✓ | seedConv 只有 2 个成员 → 扇出只扇 1 帧，掩盖 O(N) | ⬜ 待办 |
| D6 ✓ | "14.5k msg/s" 无可复现证据 | ⬜ 待办 |

## E. 部署与文档

| # | 问题 | 状态 |
|---|---|---|
| E1 | 最新 release 仍是 v0.1.1（v1.0/v1.1 的 CI 未产出资产） | ⬜ 待办 |
| E2 | 只发布 linux/amd64 一个包（9 格矩阵未走到 release） | ⬜ 待办 |
| E3 | CHANGELOG / release-notes / roadmap | 🚧 进行中 |
| E4 | README 特性与安装未更新 | 🚧 进行中 |
| E5 | split 多进程部署未实现（unit 指向不存在的二进制） | ⏭️ 明确不做（v1.2，文档已标预留） |
| E6 | 短信/邮件/captcha 供应商未接 | ⏭️ 明确不做（接口缝已留，dev 可跑通） |
| E7 | 冷库 driver 需部署层注册 | ⏭️ 设计如此（文档已写清） |
| E8 | 移动端推送 | ⏭️ v1.2 |

## 发布门槛

- [ ] A/B/C 三项的 P0 全部 ✅
- [ ] 全仓 `go build` / `go vet` / `gofmt` / `importlint` / `go test` 全绿
- [ ] 前端 `npm run build` + `npm test` 全绿
- [ ] 管理后台 `npm run build` + `npm test` 全绿
- [ ] `scripts/release-check.sh` PASS
- [ ] `scripts/doccheck.sh` PASS
- [ ] 全平台构建矩阵 9 格产出资产
- [ ] 压测：常见 API + 大量操作，数据可复现
