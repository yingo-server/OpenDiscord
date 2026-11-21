# UFOT 安全红线审查报告（security-spec §8.2）

- 日期：2026-10-29（对照 1.0 工作区含未提交改动，git status：37 M + 10 ?? 文件）
- 方法：只读审查。所有结论均附亲自执行的命令证据。
- 分级：P0=违反红线（一票否决级）/ P1=高危偏差 / P2=文档漂移 / P3=待办缺口
- 工作量：S ≤ 0.5 天 / M ≤ 3 天 / L > 3 天

---

## 一、§8.2 十二条红线逐条结论

### 红线 1：SQL 拼接 / innerHTML —— ✅ 通过（1 处 P3 备注）

**证据**：
```
rg -n "Sprintf.*(SELECT|INSERT|UPDATE|DELETE|WHERE)|\+ *\"(SELECT|AND|WHERE)" server/ --type go -g '!*_test.go'
```
仅命中 `server/internal/data/store/migrate/migrate.go:219,281` —— 两处均为**内部迁移表名**（`r.quote(migrTable)`，常量非用户输入），非注入面。

- ORDER BY：`msgreader.go:18` 的 `orderSQL = map[bool]string{...}` 白名单二元（ASC/DESC 固定字符串），comparator 同为固定串。keyset 游标谓词 `sent_at < ?` 参数化。✅
- 媒体路径：`mediasvc.go:352` `pathFor = filepath.Join(dir, sha[:2], sha[2:4], sha)` —— sha 来自服务端 `hex.EncodeToString(sha256.Sum256(body))`，非用户输入；GET 侧 `validMediaID`（行 398）严格 base64url 字符校验后**查 DB 取 sha**，不直接拼路径。✅
- innerHTML：`rg "innerHTML|dangerouslySetInnerHTML" apps/client/src/` 仅 `main.tsx:83` 一处 —— bootstrap 失败兜底写死的中文字符串常量，无用户输入。文本渲染器 `text.tsx` 用 JSX 插值（React 自动转义）；无 Markdown/HTML 渲染路径（`rg -i markdown apps/client/src/` 无命中），无链接 scheme 白名单需求面。`dist/` 内 bundle 命中为 React 运行时自带字符串。✅

**P3**：`dist/` 构建产物进库，未来若加入 Markdown 渲染需 `html:false` + scheme 白名单，建议在 CI 加 `rg innerHTML` 门禁（S）。

### 红线 2：接口缺少服务端成员/归属校验 —— ❌ P0（mediasvc GET 无会话 ACL）

抽查 5 条链路：

| 链路 | 位置 | 结论 |
|---|---|---|
| 发送消息 | `pipe/msg.go:41` `admit` 内 `memberCheck`，失败 403 不进管道 | ✅ |
| 拉历史 | `pipe/pipe.go:269` `onReadHistory` 先 `CheckMember` | ✅ |
| 群操作（addMember/removeMember/setRole/getMeta/listMembers） | `pipe/conv.go:193/232/262/279/294` 全部先 `CheckMember` 且 setRole 校验「不能授予高于自己的角色」 | ✅ |
| 消息撤回 | `rg -rn "revoke\|recall" server/internal/`（排除 token）**无消息撤回实现** | N/A（1.0 无此功能，非缺陷） |
| **媒体 GET** | `mediasvc.go:255` `handleGet`：仅 `authenticate`（登录态）→ `validMediaID` → DB Get → 出文件。**代码内自带注释承认**：`// @authz: checked-partial — no per-conversation ACL in 1.0; ... TODO(1.1): media_id → msg → cid → membership check (security-spec §4.4 红线)` | ❌ |

**P0**：任何已登录用户拿到 media_id 即可下载任意媒体。`docs/known-issues.md` 第 1 条自认（「已知 MVP 缺口」），但 §8.2 第 2 条是**一票否决项**，已知不豁免。media_id 32 字符不可枚举（`newMediaID` = sha 前 16B + 8B 随机）是唯一防线，日志/转发/客户端缓存泄露即失守；且上传者 uid 存于 `media_media` 行却未使用。
**修复**：GET 时 `media_id → msg_messages 引用 → cid → CheckMember`；无法回溯消息的孤儿媒体仅上传者本人可取。工作量 **M**。

### 红线 3：内部端口非环回/UDS 权限 —— ✅ 通过

**证据**：
```
rg -n "0\.0\.0\.0|ListenAndServe|net.Listen" server/ --type go -g '!*_test.go'
```
- chatd：`config.go:49-51` 三监听默认 `127.0.0.1:8080/7780/7781`；usergw `app/config.go:68` `127.0.0.1:8081`。
- admin：`server.go:360` `validateListen` **硬拒** 空 host/0.0.0.0/::/非环回 IP，非环回需显式 `AllowNonLoopback`。✅
- UDS：`transport/remote/remote.go:128,200` listen 后 `os.Chmod(addr, 0600)`。✅

### 红线 4：JWT PII/alg 混淆/refresh 重放 —— ✅ 通过

**证据**：
- `authsvc.go:179`：`jwt.WithValidMethods(["EdDSA"])` + keyfunc 内二次校验 `t.Method.(*jwt.SigningMethodEd25519)`（双保险）。✅
- Claims 仅 sub/jti/did/typ/ver，`rg -n "phone|email" server/internal/user/core/authn/` 无命中。✅
- 家族撤销：`tokens.go:258-308` —— claim mismatch / replay 均 `revokeFamily(row.FamilyID)` 整族撤销；logout 走 `revokeFamilyOf`；密码改密 `nAllForUID`（handlers.go）。✅
- **P1 备注**（见定向审查）：access token 吊销不踢已有 WS 连接，known-issues #10 自认，残余窗口 ≤15min，spec §2.7 允许但网关踢线缺失。

### 红线 5：全局限流只做本地判 —— ⚠️ 分裂结论

**证据**：
- OTP（authsvc）：全部走 DB 表 `identity_otp` 行数统计（`otp.go:124-155` `countOTPSince/countOTPByIPSince/countOTPDistinctIPsSince`，SQL 在 `store.go:144-150`）——**跨重启、全局有效**。✅
- 用户登录锁定（authsvc）：`login.go:142 noteFailure` → `sys.RateLimitIncr/SetPenalty` → `sys_rate_limit`/`sys_rate_limit_penalty` SQLite 表（`sys.go:97-149`）。✅
- 管理面：`admin/middleware/ratelimit.go:42-60` `LoginLimiter` 为**进程内存 map**（`accounts/fingerprints map[string]*failureState`）。admin 默认单实例 loopback 部署（`deploy/Caddyfile` 单机反代），实际风险低，但：
  - **P2**：未在 deploy.md 明确「admin 内存限流 = 单实例假设，多实例前必须下推数据层」（spec §8.1 扩容原则要求）。文档化工作量 S。
- 用户级 uid 聚合桶（`user/net/limit/limiter.go:8` 注释「uid → 本进程聚合桶」）同样是进程本地——**spec §1.6/§8.1 允许单网关进程本地判，扩容需切换**，属既定设计非违规。

### 红线 6：日志明文 PII/token —— ✅ 通过

**证据**：
```
rg -n "13[0-9]{9}" server/ --type go -g '!*_test.go'   → 无命中
```
- authsvc 日志全部 `subjectShort`（前 8 位+…，<8 位全遮蔽 `authsvc.go:480-491`）与 `shortToken`；`rg -n '"phone"|token=' server/internal/authsvc/` 命中仅为 JSON 结构体 tag 与 magic-link 查询参数构造（非日志）。
- magic link `otp.go` 明确「URL 不携带 email」（代码注释引 §5.3）。
- Redactor 真实使用：`admin/service/user.go:72` `"phone", log.RedactPhone(u.Phone)`。✅
- **P3**：`RedactPhone/RedactToken` 全库仅 1 处调用，属「有武器没用满」——建议 lint 规则禁止日志值中出现原始 phone/token 变量（S）。

### 红线 7：密钥进代码库/明文环境 —— ✅ 通过（1 处 P1 见定向）

`find . -name "*.pem" -o -name "*.key"` 仅命中 `deploy/keygen/main.go`（生成器源码）。`deploy/install.sh:186` 部署期 openssl 生成。无硬编码密钥。✅

### 红线 8：上传 magic bytes 白名单 / SVG 直通 —— ⚠️ P1（泛型文件绕过）

**证据**（`mediasvc.go:63-105, 290-325`）：
- magicTable：JPEG/PNG/WEBP(offset8)/GIF87a/GIF89a/OggS/ID3/FFFB。✅
- SVG：`blockedExt` 显式含 `.svg`（以及 .js/.html/.php 等脚本/可执行容器 17 项）。✅
- 三元一致：magic → mtype → 扩展名白名单（image: jpg/jpeg/png/webp/gif；audio: ogg/oga/opus/mp3），不符即 415。✅
- GET 响应：`X-Content-Type-Options: nosniff` + `Content-Disposition: attachment` + 服务端记录的 Mime，无 inline 渲染面。✅

**P1**：`classify` 尾部 —— **不在 magic 表的任意文件 + 任意非空扩展名 → `mtypeFile, application/octet-stream` 放行**（如 `.mp4`、`.pdf`、`.docx`、`.7z`）。spec §8.2 第 8 条「上传文件未过 magic bytes 白名单即可下载」字面上**命中**。缓解：attachment+nosniff+blockedExt 挡掉主要执行/脚本向量。评级 P1（可辩护为「泛型文件」设计，但泛型文件也应过最小 magic/黑名单复核，如拒绝 ELF/PE/MZ 头）。修复：泛型文件加 MZ/ELF/shebang 头拒绝 + 扩展名次级白名单（S）。
另 **P1**：上传即 `Status=1` 可下载，无 ClamAV/隔离期（known-issues #9 自认，spec §6.4.2「ClamAV 检出任一次=红线告警」无从谈起）。

### 红线 9：管理面 IP 白名单/TOTP/CSRF —— ✅ 基本通过（2 处 P2）

**证据**（`deploy/Caddyfile`）：
- IP 白名单：`@adminApi` 路由 `not remote_ip 127.0.0.1/32 10.0.0.0/8 192.168.0.0/16 203.0.113.10/32 → 404`（不暴露存在性）。✅ 文档说的白名单**真实存在于 Caddyfile**。
- TOTP：`service/auth.go:146` `if cfg.RequireTOTP || acc.TOTPSecret != "" { if acc.TOTPSecret == "" || !totp.Validate(...) → 拒绝 }`，`RequireTOTP` 默认 true → 无 secret 的管理员**无法登录**（强制）。prod bootstrap 打印一次性 secret。✅
- CSRF：`middleware/csrf.go` double-submit（`oc_admin_csrf` SameSite=Strict + header 比对），Bearer CLI 也要求带。✅

**P2**：① 白名单含 `10.0.0.0/8 192.168.0.0/16` 全私网段而非运维出口窄段——若 Caddy 前还有可信代理则 remote_ip 可被 `trusted_proxies static private_ranges` + XFF 伪造影响，建议收窄到具体 IP 段（S）。② 高危操作二次 TOTP（A-3）未见独立实现（`rg totp` 仅登录处），P3 待办。

### 红线 10：E2EE 托管 / 审计链可改删 —— ✅ 无后门；⚠️ P1 审计降级

**证据**：
- `rg -n "decrypt|escrow|托管" server/ --type go`：无任何解密/托管代码路径（仅 web/embed.go 的「托管静态资源」字样，无关）。✅
- 审计链：`store/migrations/cold/sys/0001_audit_log.sql:44-53` —— `trg_audit_log_no_update` BEFORE UPDATE/DELETE 触发器 RAISE EXCEPTION，**在迁移内固化**。✅ 写入单入口 `admin/auditshim/shim.go:127 journal.Append` → `pg/cold.go:290 Append`（哈希链 RecordHash）。✅
- **P1**：known-issues #4 —— 部署层未注册 postgres driver 时冷库句柄 nil，审计**静默降级为只打日志**（auditshim nil journal 容忍 + 计数器）。审计链缺失即 spec §5.5 生产红线。修复：启动时冷库不可用 → **fail-fast 拒绝生产模式启动**（S/M）。

### 红线 11：dev 后门进生产构建 —— ⚠️ P2（合规但非编译期隔离）

**证据**：`authsvc.go:72` `devOTPCode = "123456"`；DevOTP 路径散布 `otp.go:68/86/168/333`（固定 OTP、跳过 SMS、captcha 任意 token 放行、magic link 回显）。
- 隔离方式：`authsvc.go:179` 启动断言 `if cfg.DevOTP && os.Getenv("APP_ENV") != "dev" → error`；默认 `DevOTP=false`（`chatd/config.go:64`）。
- spec §2.5 原文（行 223）：「build tag **或** 配置默认关+启动断言」——两种方案二选一即可。当前满足后者。✅ 字面合规。
- **P2**：spec §8.2 第 11 条更强硬：「生产二进制中不得存在该代码路径」——按此条的字面读法，运行时判断不满足；按 §2.5 的「或」读法满足。建议改为 build tag（`//go:build !production`）彻底消除歧义（M）。

### 红线 12：密钥泄漏应急 —— 流程性条款，代码审查不适用（§6.4.2 告警阈值多数依赖 metrics/巡检，属部署验收项，未逐条验证）。

---

## 二、定向深查

### 1. 未提交改动（37 M + 10 新文件）
- **mediasvc（新）**：结论见红线 2/8——GET ACL 缺失（P0）+ 泛型文件放行（P1）+ 无扫描（P1）为其三大问题。上传侧鉴权/大小/限流（50/天，DB 固定窗口 `sys_rate_limit`）OK；**P3**：spec D-3 要求「6 次/分钟 + 500/天」，实际只有 50/天，无分钟级限制；`checkRate` 计数器失败时**放行**（`mediasvc.go:343` log-and-allow，fail-open）——限流计数失败应拒绝（spec 限流红线精神 fail-closed），P3→建议改拒绝（S）。
- **pipe/conv.go、pipe.go、msgreader.go、flusher.go、jobtrigger.go、jobs.go**：均为授权后逻辑/归档触发，SQL 均参数化+白名单（见红线 1），未见新增注入/越权面。✅
- **客户端（media.ts、renderers、msg-new 等）**：客户端 magic/扩展名/大小预检与服务端逐字镜像（好实践）；renderers 无 innerHTML。✅

### 2. chatpb 手写 proto wire 编解码 —— ✅ 通过
`proto/gen/chatpb/chatpb.go:36-93`：
- varint：`binary.Uvarint` 返回 `n<=0` 一律 fail（`pbReadTag/pbReadVarint/pbReadBytes`），超长/截断 varint 安全拒绝；
- 长度前缀：`pbReadBytes` `uint64(len(b)-m) < n` 越界检查（无 int 溢出：比较在 uint64 域）；
- fixed32/64 有长度检查；`pbSkip` 对 wire type 3/4（group）**默认拒绝**（default → false）——无深度嵌套递归面（消息结构为扁平字段 + 单层 map entry `pbDecodeMapEntry`，无递归下降）。✅
- 帧层面有 `frame.MaxFrameBytes` 上限（mediasvc 注释引 64KB）。✅
- **P3**：无 fuzz 测试覆盖（建议 CI 加 5min go-fuzz 种子，S）。

### 3. SYNC 游标服务端签发 —— ❌ P1
spec §2（行 137）红线：「SYNC 游标必须服务端签发（HMAC 绑定 uid+会话+序号+过期）」。实际 `ReadHistory.beforeSeq` 是客户端可构造的裸序号（`readrouter.go:38` 注释「since_seq is the keyset cursor」）。**无 HMAC、无过期**。缓解：每次读取前有 `CheckMember`（pipe.go:269），且 spec D-6 的翻页限流/审计未见实现。评级 P1（越权已挡，游标伪造只能做已授权会话内的翻页枚举，但红线明文要求签发）。修复：cursor = HMAC(uid,cid,seq,exp)（M）。

### 4. E-3/E-4 WS upgrade 限流 —— ❌ P1
spec §1（行 31-32）红线：单 IP upgrade 10/min + 失败熔断。实际：Caddyfile 中 `rate_limit` **整段被注释掉**（「默认注释掉以保证 stock caddy 可直接加载」），usergw 内 `rg "upgrade.*rate" user/net/ws/` 无命中——**两层都没实现**，仅依赖注释里的 xcaddy 方案。修复：发布物强制带 caddy-ratelimit 插件构建 + fail2ban 规则落地（M）。

### 5. 首启自动生成密钥 vs §5.2 —— P1
`cmd/chatd/wire.go:395-500`：签名密钥与 HMAC key **缺失即生成**（写 0600 + WARN 日志）。§5.2 红线要求生产密钥由 `/etc/chatapp/keys/` 预置（deploy/keygen/install.sh 存在），但代码路径允许生产环境静默走「自生成」分支——一旦 key 文件被误删，重启即**换钥 → 全站 token 失效/索引作废**，且只有一条 WARN。修复：`mode=production`（或 APP_ENV!=dev）时密钥缺失 → 拒绝启动（S）。

---

## 三、汇总

| # | 级别 | 事项 | spec 出处 | 位置 | 工作量 |
|---|---|---|---|---|---|
| 1 | **P0** | 媒体 GET 无会话成员 ACL（IDOR） | §8.2-2/§4.4 | mediasvc.go:255 | M |
| 2 | P1 | 泛型文件未过 magic 白名单即可下载 | §8.2-8/§4.3 | mediasvc.go:318-322 | S |
| 3 | P1 | 上传无病毒扫描，Status 直落 1 | §6.4.2 | mediasvc.go:216 | M |
| 4 | P1 | SYNC 游标未服务端签发 | §2 行137 | readrouter.go | M |
| 5 | P1 | WS upgrade 限流（E-3/E-4）双层均未启用 | §1 行31-32 | deploy/Caddyfile | M |
| 6 | P1 | 冷库缺 driver 时审计链静默降级 | §5.5 | auditshim/assemble.go | S/M |
| 7 | P1 | 首启自动生成密钥未按生产 fail-fast | §5.2 | wire.go:395 | S |
| 8 | P1 | access token 撤销不踢 WS（残余 ≤15min） | §2.7 | tokens.go TODO-KICK | M |
| 9 | P2 | DevOTP 靠运行时 env 而非 build tag（§2.5「或」满足，§8.2 字面不满足） | §2.5/§8.2-11 | authsvc.go:179 | M |
| 10 | P2 | admin 内存限流=单实例假设未文档化 | §8.1 | middleware/ratelimit.go | S |
| 11 | P2 | admin IP 白名单含全私网段过宽 | §6.3 | deploy/Caddyfile | S |
| 12 | P3 | 上传限流无分钟级（D-3）且计数失败 fail-open | D-3 | mediasvc.go:337 | S |
| 13 | P3 | 高危管理操作二次 TOTP（A-3）未实现 | A-3 | admin/service | M |
| P3 | P3 | Redactor 全库仅 1 处调用，缺 lint 强制 | §5.3 | pkg/log | S |
| P3 | P3 | chatpb 无 fuzz；dist 产物入库 | — | proto/gen | S |

**通过的红线**：1（SQL/innerHTML）、3（监听/UDS）、4（JWT）、5（OTP/登录限流已全局）、6（日志 PII）、7（无密钥入库）、9（管理面三件套）、10（无 E2EE 后门、审计触发器在迁移内）。

### 上线阻断项（必须修复才可过评审）

1. **媒体 GET 会话 ACL**（P0 #1）——1.0 已知不豁免红线。
2. **审计链 fail-fast**（P1 #6）——生产冷库不可用必须拒绝启动，不允许「只写日志」。
3. **生产密钥缺失拒绝启动**（P1 #7）——与 #2 同属「静默降级」类。
4. **WS upgrade 限流落地**（P1 #5）——spec 明文红线，当前为纯注释。
5. **泛型文件上传收紧**（P1 #2）——至少拒绝 MZ/ELF/shebang 头与未列扩展名。
6. **SYNC 游标 HMAC 签发**（P1 #4）。

#2–#6 若以「可信小规模内网试运行」名义带病上线，须安全组书面豁免并限制注册开关（§6.4.3 禁注册预案联动）；对外/开放注册部署为硬阻断。
