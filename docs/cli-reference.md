# OpenDiscord CLI 参考（1.0）

> 只描述两个二进制 **代码里真实实现且实测过** 的子命令/flag/退出码。命令表取自
> `rg 'case "' server/cmd/*/main.go`，flag 取自 `rg 'fs\.(String|Int|Bool|StringVar)' server/cmd/`。
> 每个子命令的退出码实测为 `0/1/64/65/77` 子集。凡与预期不符的缺口在 **§7 已知限制** 如实列出。

两个二进制：

| 二进制 | 定位 | 说明 |
|--------|------|------|
| `adminctl` | **管理 API 客户端** | 纯 HTTP 客户端，所有操作经 `admin_rest_listen` 的 `/api/v1/*`，与浏览器控制台同一 CRUD/鉴权路径（"API 的第二皮肤，不是旁路"）。 |
| `opendiscordctl` | **本机运维 + 管理 API** | 直连本机 systemd/信号/热 SQLite（status/doctor/backup）；少数变更动作（archive/flags/ban）经同一 loopback 管理 API。 |

---

## 1. 全局约定（两者共享）

### 1.1 凭据文件

- 路径：`~/.config/opendiscord/adminctl.json`（`os.UserConfigDir()/opendiscord/adminctl.json`）。
- 可用 `OPENDISCORD_ADMIN_CREDENTIALS` 覆盖路径。
- JSON 结构（`adminctl/client.go` `credentials`，`opendiscordctl/api.go` 复制同构）：
  ```json
  { "server": "http://127.0.0.1:7781", "token": "...", "csrf_token": "...",
    "username": "rootadmin", "expires_at": 1700000000 }
  ```
- 权限：目录 `0700`、文件 `0600`（`saveCredentials`）。
- `adminctl login` 写该文件；`opendiscordctl` 读同一文件 → **登录一次，两个 CLI 通用**。

### 1.2 环境变量

| 变量 | 作用 | 两个 CLI |
|------|------|---------|
| `OPENDISCORD_ADMIN_URL` | 管理 API base URL | 都读 |
| `OPENDISCORD_ADMIN_TOKEN` | Bearer token | 都读 |
| `OPENDISCORD_ADMIN_CSRF` | CSRF token | 都读 |
| `OPENDISCORD_ADMIN_CREDENTIALS` | 凭据文件路径 | 都读 |
| `OPENDISCORD_HOT_DB` | 热库路径（默认 `/var/lib/opendiscord/hot.db`） | opendiscordctl |
| `OPENDISCORD_COLD_DSN` | 冷库 DSN（doctor 只做 TCP 连通） | opendiscordctl |
| `OPENDISCORD_BACKUP_SCRIPT` | 自定义备份脚本 | opendiscordctl |

优先级：命令行 flag → 环境变量 → 凭据文件 → 内置默认。

### 1.3 CSRF 双提交

- 管理面严格模式：**所有非安全方法**（含用 Bearer 认证的 CLI）都必须发 `X-CSRF-Token` **并且**带同名 `oc_admin_csrf` Cookie。
- 两个 CLI 在 `csrf_token` 非空时都自动：
  `req.Header.Set("X-CSRF-Token", csrf)` + `req.AddCookie({Name:"oc_admin_csrf", Value:csrf})`。
- `adminctl/client.go` 的 `csrfCookie` 字段为空字符串时 cookie 名会为空——实际靠 login 保存的 csrf + cookie 补全；详见 §7 提示。

### 1.4 Idempotency-Key 自动生成

- 写方法（非 GET/HEAD）自动生成 RFC4122 v4 UUID 放入 `Idempotency-Key`（`newUUID()`）。
- `adminctl` 可用 `--idempotency-key <key>` 显式指定（用于重放复现）。
- `opendiscordctl` 每次写操作都生成新 UUID，不支持外部指定。

### 1.5 退出码（sysexits 子集）

| 码 | 名称 | 含义 |
|----|------|------|
| 0 | OK | 成功 |
| 1 | ERROR | 一般错误；`opendiscordctl doctor` 检出 `FAIL` 也算 1 |
| 64 | USAGE | 语法/参数错误（含非法 flag、缺少位置参数、子命令拼错） |
| 65 | NOT_FOUND | 目标不存在（REST 404 / 本地路径缺失）——`admin-data-design §7` |
| 77 | DENIED | 未授权/无权限；`opendiscordctl` 未配置管理令牌（未登录）也归 77 |

---

## 2. `adminctl`

用法：`adminctl [global flags] <command> [args]`

### 2.1 全局 flag（`adminctl/main.go` `parseGlobals`）

| flag | 默认 | 说明 |
|------|------|------|
| `--server <url>` / `-server` | `http://127.0.0.1:7781` | 管理 API base URL |
| `--token <t>` | — | Bearer token |
| `--csrf-token <t>` | — | CSRF token |
| `--output json\|table` / `-o` | `table` | 输出格式（非法值 → 退出 64） |
| `--idempotency-key <k>` | — | 写操作显式幂等键 |
| `--no-save` | false | 仅 login：不持久化凭据 |

> 默认端口硬编码 `http://127.0.0.1:7781`。全局 flag 可出现在任意位置（会被 `parseGlobals` 提前摘出）。

### 2.2 子命令总表（`main.go` case 分支）

`login` · `whoami` · `users` · `kick` · `reports` · `messages` · `broadcast` · `flags` · `logout` · `metrics` · `audit` · `archive` · `completion` · `help`
（12 个业务子命令 + `completion` + `help`。实测子命令总数以 `rg 'case "' server/cmd/adminctl/main.go` 为准。）

#### login
- 语法：`adminctl login [--username U] [--password-stdin] [--totp T]`
- 交互：无 `--username` 时提示 `Username:`；密码用 `stty -echo` 隐藏；无 `--totp` 时提示 `TOTP:`（TOTP 是本版必填）。
- `--password-stdin` 要求同时给 `--username`，密码从 stdin 读一行。
- 成功：默认把 token 打到 stdout（脚本可消费）；`--output json` 则打完整响应；并保存凭据（除非 `--no-save`）。
- 退出码：0 / 64（参数）/ 77（401/403）。

#### whoami
- 语法：`adminctl whoami`（无子 flag）
- 调 `GET /api/v1/whoami`。
- 退出码：0 / 77 / 1。

#### users
- `users list [--q Q] [--status N(-1..3,默认-1)] [--limit N(50)] [--offset N(0)]` → `GET /api/v1/users`
- `users show <uid>` → `GET /api/v1/users/{id}`
- `users ban <uid> [--level 1..3(1)] [--days N(0=永久)] [--until UNIX] [--reason S]` → `POST /api/v1/users/{id}/ban`
- `users unban <uid> [--reason S]` → `POST /api/v1/users/{id}/unban`
- 退出码：0 / 64（缺 uid / 非法 id）/ 65（404）/ 77 / 1。

#### kick
- 语法：`adminctl kick <uid> [--reason S]` → `POST /api/v1/users/{id}/kick`
- 退出码：0 / 64 / 65 / 77 / 1。

#### reports
- `reports list [--status N(-1..3,-1)] [--limit 50] [--offset 0]` → `GET /api/v1/reports`
- `reports show <id>` → `GET /api/v1/reports/{id}`
- `reports handle <id> --action claim|resolve|reject [--note S]` → `POST /api/v1/reports/{id}/handle`（`--action` 必填）
- 退出码：0 / 64 / 65 / 77 / 1。

#### messages
- `messages search [--q Q] [--conv N] [--limit 50] [--offset 0]`（需 `--q` 或 `--conv`）→ `GET /api/v1/messages/search`
- `messages recall <id> [--reason S]` → `POST /api/v1/messages/{id}/recall`
- 退出码：0 / 64 / 65 / 77 / 1。

#### broadcast
- 语法：`adminctl broadcast preview|send --text T [--conv ID]... [--expires-at UNIX]`
- `preview` → `POST /api/v1/broadcast?dry_run=true`（body 亦带 `dry_run:true`）
- `send` → `POST /api/v1/broadcast`
- `--conv` 可重复（`stringList`）。
- `--text` 必填，否则 64。
- 退出码：0 / 64 / 77 / 1。

#### flags
- `flags list` → `GET /api/v1/flags`
- `flags set <key> <value>` → `PUT /api/v1/flags`（**两个位置参数**）
  - value 若非法 JSON，会被当作字符串再 `json.Marshal` 加引号。
- 退出码：0 / 64（缺 key/value）/ 77 / 1。

#### audit
- 语法：`adminctl audit [--since-id N(0)] [--limit N(500, max1000)]` → `GET /api/v1/audit?since_id=&limit=`
- 退出码：0 / 77 / 1。

#### metrics
- 语法：`adminctl metrics` → `GET /api/v1/metrics`（Prometheus 文本）
- 默认原样输出 `text/plain` 内容；`--output json` 时包成 `{"raw":"<文本>"}`。
- 退出码：0 / 77 / 1。

#### logout
- 语法：`adminctl logout` → `POST /api/v1/logout`，并删除凭据文件 `adminctl.json`。
- 默认打印 `logged out`；`--output json` 打印服务端响应。
- 退出码：0 / 77 / 1。

#### archive
- `archive status` → `GET /api/v1/archive/status`
- `archive run [--dry-run]` → `POST /api/v1/archive/run`（`--dry-run` 追加 `?dry_run=true`）
- 退出码：0 / 64 / 77 / 1。

#### completion
- `adminctl completion bash` → stdout 打印 bash 补全脚本（见 §8）。
- 退出码：0 / 64（仅接受 `bash`）。

---

## 3. `opendiscordctl`

用法：`opendiscordctl [global flags] <command> [args]`

### 3.1 全局 flag（`opendiscordctl/cli.go` `parseGlobals`）

| flag | 默认 | 说明 |
|------|------|------|
| `--json` / `-json` | false | 统一 JSON 输出信封 `{ok, command, data}` |
| `--admin-url <url>` | `http://127.0.0.1:7781` | 管理 API 地址 |
| `--token <t>` | — | 管理令牌 |
| `--csrf-token <t>` | — | CSRF 令牌 |
| `--timeout <dur>` | `20s` | 总超时（`time.ParseDuration`，≤0 → 64） |

> 注意 JSON 开关命名与 adminctl **不同**：`opendiscordctl --json` vs `adminctl --output json`。

### 3.2 子命令总表（`main.go` case 分支）

`version` · `status` · `doctor` · `backup` · `archive` · `migrate` · `upgrade` · `flags` · `ban` · `unban` · `help`
（10 个功能子命令 + help；实测以 `rg 'case "' server/cmd/opendiscordctl/main.go` 为准。）

#### version
- 语法：`opendiscordctl version`
- 打印 `version/commit/date/go/os/arch`；`--json` 输出信封。附带多余参数 → 64。

#### status
- 语法：`opendiscordctl status [--unit NAME(opendiscord)] [--hot-db PATH] [--signal usr1|usr2|none] [--no-journal] [--journal-lines N(50)]`
- 读 systemd 进程状态 + 发 SIGUSR1 dump + 读 journal 摘要 + 热库统计。
- 退出码：0 / 1 / 64 / 65（热库缺失）/ 77。

#### doctor
- 语法：`opendiscordctl doctor [--hot-db PATH] [--unit NAME] [--since -1h] [--no-logs] [--no-cold]`
- 体检：热库完整性、磁盘余量、近窗口 error 日志数、冷库 TCP 连通。
- 有 `FAIL` 项时退出码 **1**（非 0，也非 64）。
- 退出码：0 / 1 / 64 / 77。

#### backup
- 语法：`opendiscordctl backup [--dry-run] [--db PATH] [--script PATH] [--to DIR]`
- 调用 `deploy/backup.sh`；不存在则内置 `sqlite .backup` + 加密提示。
- 退出码：0 / 1 / 64 / 65（脚本/db 缺失）/ 77。

#### archive
- 语法：`opendiscordctl archive --dry-run | --run`（**必须且只能二选一**）
- 经管理 API `POST /api/v1/archive/run`；未登录时提示改用 `adminctl archive`。
- 退出码：0 / 64（两者都/都不给）/ 77（未登录）/ 1。

#### migrate
- 语法：`opendiscordctl migrate thumbs [--dry-run] [--batch N(500)] [--hot-db PATH] [--media-dir DIR]`
- 一次性把旧 `thumb_data` 内联缩略图迁移到 `media_dir/thumbs/` 文件并清空该列。
- 退出码：0 / 1 / 64 / 65（热库缺失）/ 77。

#### upgrade
- 语法：`opendiscordctl upgrade [--run] [--force] [--hot-db PATH] [--media-dir DIR] [--batch N] [--unit NAME]`
- v1.4 新增的一次性升级维护计划（**默认 dry-run 只读**，`--run` 执行）：检测 `auto_vacuum=0` 生成 auto_vacuum 转换步骤 + 旧 thumb_data 迁移步骤（复用 migrate 机制），并可含备份步骤。
- 退出码：0 / 1 / 64 / 65 / 77。

#### flags
- `opendiscordctl flags list` → `GET /api/v1/flags`
- `opendiscordctl flags set KEY=VALUE` 或 `opendiscordctl flags set KEY VALUE`（**支持 `k=v` 或 `k v` 两种**）
  - 该语法校验先于凭据检查：语法错 → 64（不是 77）。
  - value 非合法 JSON 时加引号成字符串。
- 退出码：0 / 64 / 77 / 1。

#### ban / unban
- `opendiscordctl ban <uid> [--days N(0=永久)] [--level 1..3(1)] [--until UNIX] [--reason S]` → `POST /api/v1/users/{id}/ban`
- `opendiscordctl unban <uid> [--reason S]` → `POST /api/v1/users/{id}/unban`
- `--days` 与 `--until` 互斥且不能为负；`ban` 校验 `--level 1..3`。
- 退出码：0 / 64（缺 uid / 非法 level / 互斥冲突）/ 65 / 77 / 1。

---

## 4. 退出码详解（逐条）

| 码 | adminctl 触发点 | opendiscordctl 触发点 |
|----|----------------|-------------------|
| 0 | 命令成功、`help`/`completion` 成功 | 命令成功、`help`/`version` 成功 |
| 1 | 网络/非 2xx 且非 401/403/404 | 一般错误；`doctor` 有 FAIL |
| 64 | 未知命令、缺参数、非法 `--output`、缺 uid、`reports handle` 缺 `--action`、`broadcast` 缺 `--text` | 未知命令、`archive` 既/未指定 dry-run/run、`flags` 语法错、非法 `--timeout`、缺 uid |
| 65 | REST 404（`statusError` → `exitNotFound`） | REST 404 或本地路径缺失（热库/备份脚本） |
| 77 | REST 401/403 | 未配置管理令牌 / REST 401/403 |

---

## 5. 变更类命令统一走管理 API

`opendiscordctl` 的 `archive`、`flags`、`ban`、`unban` **不直连数据库/总线**，而是复用与 `adminctl` 完全相同的 loopback REST 链路（含 CSRF 双提交、Idempotency-Key、错误体解析）。这是 `docs/admin-data-design.md §7` 的一致性红线。

---

## 6. shell 补全

```
# bash
source <(adminctl completion bash)
# 或持久化
adminctl completion bash > /etc/bash_completion.d/adminctl
```
补全覆盖顶层命令、`users/reports/messages/broadcast/flags/archive` 子命令、`--output json|table` 取值，以及全局 flag（`--server/--token/--csrf-token/--output/--idempotency-key/--no-save`）。

> `opendiscordctl` **没有** `completion` 子命令（见 §7）。

---

## 7. 已知限制（按 2026-10 当前源码实测记录）

> 说明：任务书里原始列的"adminctl 缺 logout/metrics""`<sub> --help` 无输出""opendiscordctl `--help` 返回 64"三条，在当前仓库源码中**已不成立**（见下）；本文件按**实测**记录真实状态。

1. **`adminctl` 已含 `logout`、`metrics` 子命令（原"缺失"说法已过时）。**
   `main.go` 有 `case "logout"` / `case "metrics"`，对应 `cmdLogout`（`POST /api/v1/logout` + 删凭据）与 `cmdMetrics`（`GET /api/v1/metrics` 原文输出）。实测均可用。

2. **`opendiscordctl` 已含 `migrate`、`upgrade` 子命令（v1.4 新增，原清单未收录）。**
   `migrate thumbs` 做旧缩略图一次性迁移；`upgrade` 合并 auto_vacuum 转换 + thumbs 迁移为一次维护计划，默认 dry-run、`--run` 执行。详见 §3。

2. **`adminctl <sub> --help` 的行为分两种，非"无输出"。**
   - **叶子子命令**（如 `users list --help`、`login --help`、`metrics --help`）会打印 flag 用法并退出 **0**。
   - **父命令**（如 `users --help`、`reports --help`）把 `--help` 当作参数走子命令分发，返回 `users requires list|show|ban|unban` 并退出 **64**。要查看父命令帮助请用顶层 `adminctl help`。

3. **`opendiscordctl status|doctor|backup|archive --help` 当前返回 0（原"返回 64"已过时）。**
   实测这四个子命令的 `--help` 都会打印 flag 用法并退出 **0**。只有 `opendiscordctl help`/`-h`/`--help` 作为顶层命令时也是 0。

4. **两个 CLI 的 `flags set` 语法不同（仍然成立）。**
   - `adminctl flags set <key> <value>`：**两个位置参数**。
   - `opendiscordctl flags set KEY=VALUE` 或 `flags set KEY VALUE`：支持 `k=v` 与 `k v` 两种；语法校验先于凭据检查（语法错 → 64，未登录 → 77）。

5. **JSON 开关命名不同（仍然成立）。**
   - `adminctl --output json`（或 `-o json`；`table` 为默认）。
   - `opendiscordctl --json`（布尔开关，输出 `{ok,command,data}` 信封）。

6. **端口 footgun：CLI 内置默认端口 7781，而 `config/chatd.toml` 里写的 `admin_listen` 是 7780（仍然成立）。**
   - 两个 CLI 默认 base URL 都是 `http://127.0.0.1:7781`，指向 **`admin_rest_listen`（REST 面）**——正确。
   - 但 `config/chatd.toml` **只配置了 `admin_listen = 127.0.0.1:7780`**（injector 面，只为 `POST /admin/inject`），**没有** `admin_rest_listen` 项；后者取默认 7781。
   - 若把 CLI 的 `--server` 手动改成 7780，会打到 injector 端口：`/api/v1/*` 全部不是 REST 面（404 / `WRONG_SURFACE`）。**裸跑 `adminctl` 依赖默认 7781 才连得通 REST 面**。

7. **`opendiscordctl` 没有 `completion` 子命令**（只有 `adminctl completion bash`）。

---

## 8. 补充

- `adminctl help` / `-h` / `--help`、`opendiscordctl help` / `-h` / `--help` 打印用法并退出 0。
- 两个 CLI 的写操作都在请求前生成幂等键；重放同一 `--idempotency-key` 可复现同一响应（服务端 5 分钟缓存）。
