# UFOT v2.0 CLI UX 发布准备清单

> 依据 `sota-cli-ux` AUDIT checklist（rules/01~04），基于当前源码静态核对（`rg` / `file_read` / `git log`），未运行 `go test/build`。
> 同批更新：`docs/cli-reference.md`（v2.0 终审）。本文件只记录事实与待办，不替代发布决策。

## 1. 三 CLI 定位表

| CLI | 定位 | 通道 | 脚本友好 | 命令数（顶层 case 分支） |
|-----|------|------|----------|--------------------------|
| `server/cmd/adminctl` | 管理面 API 客户端（用户/举报/消息/广播/flags/归档/审计/metrics） | loopback HTTP REST `/api/v1/*`（`admin_rest_listen`，与控制台同路） | 是（`--output json`） | 14 case 行 / 12 业务 + `completion` + `help` 组 |
| `server/cmd/ufotctl` | 本机运维（systemd/信号/热库/备份/体检）+ 少量 API 动作 | 本机文件/信号 + loopback REST | 是（`--json`） | 10 case 行 / 9 功能 + `help` 组（`upgrade` 未接线） |
| `server/cmd/opchat` | 纯 TUI 管理控制台（7 标签页） | REST（与 adminctl 共用凭据） | 否（非 TTY → 64） | 无子命令（单用途 TUI） |

命令树来源：`rg 'case "' server/cmd/{adminctl,ufotctl}/main.go`；opchat 为单入口。

## 2. 退出码契约（sysexits 子集，两个业务 CLI 一致）

| 码 | 名称 | 含义 / 触发点 |
|----|------|---------------|
| 0 | OK | 成功；`help` / `version` / leaf `--help` 成功 |
| 1 | ERROR | 一般错误；`ufotctl doctor` 有 FAIL；`opchat` 登录失败 |
| 64 | USAGE | 未知命令/flag、缺参数、非法 `--output`/`--timeout`、非 TTY 降级（opchat） |
| 65 | NOT_FOUND | REST 404 / 本地路径缺失（热库、备份脚本） |
| 77 | DENIED | REST 401/403；未配置管理令牌（ufotctl 提示 `adminctl login`） |

TUI 无 SIGINT 130 显式契约（见 §6 已知限制）。

## 3. dry-run 支持清单

| CLI/命令 | 支持 | 形式与语义 |
|----------|------|------------|
| `adminctl broadcast preview` | 是 | `?dry_run=true` + body `dry_run:true`（预览，不发送） |
| `adminctl archive run --dry-run` | 是 | `?dry_run=true`（估算，不执行） |
| `ufotctl backup --dry-run` | 是 | 只校验环境、不写文件/不改库 |
| `ufotctl archive --dry-run` | 是 | 必须且只能与 `--run` 二选一；未登录 → 77 |
| `ufotctl migrate thumbs --dry-run` | 是 | 只统计将迁移的行数/字节数，不写盘不改库 |
| `ufotctl upgrade`（**未接线**） | 设计上是 | 默认 dry-run、`--run` 执行；但无 `case "upgrade"`，实际调用 → 64 |
| `ufotctl flags/ban/unban`、`adminctl users/reports/messages/flags` | 否 | 写操作无 `--dry-run`（见 §6） |

## 4. 机器可读输出支持情况

| CLI | 能力 | 说明 |
|-----|------|------|
| `adminctl` | `--output json`（默认 table；代码另允许 `text` 别名，行为同 table） | 所有读/写命令经同一 `render()`；`metrics --output json` 包成 `{"raw":…}` |
| `ufotctl` | `--json` | 统一信封 `{ok, command, data}`；JSON 模式错误仍写 stderr（非 JSON 结构） |
| `opchat` | 无 | TUI-only，无 `--json`/`--output`，不适合 agent/脚本 |

流约定核对：两个业务 CLI 的数据走 stdout、错误/提示走 stderr（`e.errorf` / `printError` / `parseFlags` 的 flag 输出）；TUI 的状态文本走 stdout（TTY 内），非 TTY 降级用法也打印到 stdout。JSON 输出无 ANSI；`doctor` 仅在 stdin 为 TTY 且未设置内部禁用变量时使用彩色符号，管道下为纯文本（规则 02 §4 部分满足）。

## 5. AUDIT 发现与修正摘要（rules/01~04 各一句话）

- **rules/01（命令/flag/配置）**：两个业务 CLI 均为动词式命令树且子命令可 `-h`（adminctl 21 个叶子子命令有测试锁定退出 0），但 adminctl 混有 `kick`/`metrics` 动词名词且 `--output text` 与 table 同义未文档化、`-v` 被用作 version（与 SOTA verbose 语义冲突，名字与 flag 不改仅记录）；ufotctl `upgrade` 仅有实现未接入分发 —— 修正：帮助/文档如实标注 `upgrade` 未接线与 `--output text` 别名；不改名不改 flag。
- **rules/02（输出/交互/退出码）**：stdout/stderr 分流基本合规、退出码 0/1/64/65/77 全链路一致并有单测，但 JSON 模式错误不是 JSON、无 `--fields`/`--no-header`、两 CLI 无 `-q/-v`、帮助未列示例与 docs URL（名字/flag 不改）—— 修正：帮助文本补用法/退出码/命令级 `--help` 提示，其余记为限制。
- **rules/03（行为/生命周期）**：dry-run 覆盖 5 个变更入口（dry-run 一致性经测试），非 TTY 无挂起（opchat 用法+64、adminctl login 缺 flag 即 64），但写操作普遍无 `--dry-run`、无 `completion`（ufotctl）、无 `--self-test`（doctor 是 verdict 工具）、无 Sysexits 130/信号说明 —— 修正：文档补全 dry-run 清单与限制；代码逻辑未改。
- **rules/04（分发/文档）**：adminctl 有 `completion bash` 且经单测，但 `docs/cli-reference.md` 原漏第三个 CLI `opchat`、误列未接线 `upgrade`、未提 v2.0 容量契约 —— 修正：重写 cli-reference 顶部与子命令表、补 §3.3/§3.4，并新增本清单。

### 本次实际修改

- `server/cmd/adminctl/main.go`：仅 `printUsage` 帮助字符串 —— 命令说明改为「动词+宾语/资源+动作」式（如 `users list|show|ban|unban`、`archive status|run`），补 `Run 'adminctl <command> --help'` 与 `usage:` 行。
- `server/cmd/ufotctl/main.go`：仅 `usageText` 帮助字符串 —— 移除不可达的 `upgrade` 条目并加"未接线"提示；`backup` 描述对齐源码（不再声称调用不存在的备份脚本路径）；补 `--help`/JSON 说明。
- `docs/cli-reference.md`：版本头改为 v2.0、三 CLI 表、命令数核实、`--output text` 别名、非 TTY 降级、opchat 小节、容量契约入口（无专用 CLI）、upgrade 限制。
- `docs/cli-ux-readiness.md`：本文件。

### 与单测冲突而未改的项（只报告，不改）

- `ufotctl` 错误/输出中 `adminctl login`、`结果: OK`、`dump`、`age` 加密提示、`dry_run: false` 等字符串被 `cli_test.go` / `doctor_test.go` / `upgrade_test.go` 断言：未改动任何功能输出文案。
- `opchat` 的 `kUsage`（含「非交互…退出码 64」）、`已取消`、`不是合法 JSON`、`预计触达`、`将搬移行数`、`keybindings`、`adminctl login` 等被 `render_test.go` / `ui_test.go` 断言：未改动 i18n 表与 TUI 文案。
- `adminctl` 的 `complete -F _adminctl adminctl`、`example_metric 1`、`logged out` 等被 `main_test.go` 断言：未改动补全脚本、metrics/logout 输出。
- 未改命令名、flag 名、退出码语义、测试、代码逻辑；未提交 git。

## 6. 已知限制（发布须知）

1. **命令未接线**：`ufotctl upgrade` 有实现与单测但无分发（→ 64）；帮助已如实标注。
2. **机器输出面不足**：两业务 CLI 的 JSON 错误不是 JSON 对象（无 `code`/`message`/`remediation`）、无 `--fields`/`--plain`/`--no-color`/`-q`/`-v`，`ufotctl` 无 `completion`；AI agent 驱动的契约尚未达标。
3. **危险操作无 dry-run**：ban/unban、flags set、users/reports/messages 写操作无 `--dry-run`；无 TTY 确认/--force 语义（经管理 API 鉴权），不阻断发布但不符合 SOTA 全套。
4. **TUI 限制**：opchat 非 TTY 打印用法到 stdout（规则 02 要求错误/用法到 stderr）且无 130 信号契约；纯 TUI 不可脚本化。
5. **命名/一致性**：`ufotctl -v` 表示版本而非 verbose；adminctl `--output text` 未文档化曾遗漏（本次已补）；两 CLI 默认端口 7781（REST），`config/chatd.toml` 的 `admin_listen=7780` 是 injector 面，误用会 404/WRONG_SURFACE。
6. **容量契约无 CLI**：`policy.capacity` 仅由 `config/policy.json` 配置 + SIGHUP 热加载，日志可观测（`admission.reject`）；CLI 只可间接观察状态/日志。
7. **文档生成方式**：cli-reference 为人工维护（非 `--help` 自动生成），存在漂移风险；本次已逐条用 `rg` 核对，后续变更需同步帮助文本与 doc。
