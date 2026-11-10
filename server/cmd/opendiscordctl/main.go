package main

import (
	"context"
	"errors"
	"os"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], defaultEnv()))
}

// run is the whole CLI: parse globals, dispatch, map errors to exit codes.
func run(args []string, e *environment) int {
	g, rest, err := parseGlobals(args)
	if err != nil {
		e.errorf("%v", err)
		e.printf("%s", usageText)
		return exitUsage
	}
	if len(rest) == 0 {
		e.printf("%s", usageText)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), g.Timeout)
	defer cancel()

	cmd, sub := rest[0], rest[1:]
	switch cmd {
	case "help", "-h", "--help":
		e.printf("%s", usageText)
		return exitOK
	case "version", "--version", "-v":
		err = cmdVersion(ctx, e, g, sub)
	case "status":
		err = cmdStatus(ctx, e, g, sub)
	case "doctor":
		err = cmdDoctor(ctx, e, g, sub)
	case "backup":
		err = cmdBackup(ctx, e, g, sub)
	case "archive":
		err = cmdArchive(ctx, e, g, sub)
	case "migrate":
		err = cmdMigrate(ctx, e, g, sub)
	case "flags":
		err = cmdFlags(ctx, e, g, sub)
	case "ban":
		err = cmdBan(ctx, e, g, sub, false)
	case "unban":
		err = cmdBan(ctx, e, g, sub, true)
	default:
		e.errorf("unknown command %q", cmd)
		e.printf("%s", usageText)
		return exitUsage
	}
	if err != nil {
		if errors.Is(err, errHelpRequested) {
			return exitOK
		}
		e.errorf("%v", err)
		var ue *usageErr
		if errors.As(err, &ue) {
			e.printf("%s", usageText)
			return exitUsage
		}
		return exitCodeFor(err)
	}
	return exitOK
}

const usageText = `opendiscordctl — OpenDiscord 系统级运维 CLI（直连本机 systemd/信号/热库；不新增网络端口）

用法:
  opendiscordctl [全局选项] <命令> [参数]

命令:
  status    [--unit NAME] [--hot-db PATH] [--signal usr1|usr2|none] [--no-journal]
            进程状态 + SIGUSR1 dump 摘要（读 journal 最后一份）+ 热库统计
  doctor    [--hot-db PATH] [--unit NAME] [--no-logs]
            体检：热库完整性、磁盘余量、近 1h error 日志数、冷库连通
  backup    [--dry-run] [--db PATH] [--script PATH] [--to DIR]
            调用 deploy/backup.sh；不存在则内置 sqlite .backup + 加密提示
  archive   --dry-run | --run
            经管理 API 触发归档（未登录时提示 adminctl archive）
  migrate   thumbs [--dry-run] [--batch N] [--hot-db PATH] [--media-dir DIR]
            一次性把旧 thumb_data 内联缩略图迁移到 media_dir/thumbs/ 文件并清空该列
  upgrade   [--run] [--force] [--hot-db PATH] [--media-dir DIR] [--batch N] [--unit NAME]
            一次性升级维护计划（默认 dry-run 只读）：旧 thumb_data 迁移 +
            auto_vacuum=0 → INCREMENTAL 转换（VACUUM；执行前须停服，--force 跳过检查）
  flags     list | set KEY=VALUE
            功能开关（经管理 API，policy-as-flag）
  ban       <uid> [--days N] [--reason TEXT] [--level 1..3]
  unban     <uid> [--reason TEXT]
            经管理 API 封禁/解封（与 adminctl 同一链路）
  version   版本信息
  help      本帮助

全局选项:
  --json                    统一 JSON 输出（ok/command/data 信封）
  --admin-url URL           管理 API 地址（env OPENDISCORD_ADMIN_URL）
  --token TOKEN             管理令牌（env OPENDISCORD_ADMIN_TOKEN）
  --csrf-token TOKEN        CSRF 令牌（env OPENDISCORD_ADMIN_CSRF）
  --timeout DURATION        总超时（默认 20s）

环境变量:
  OPENDISCORD_HOT_DB           热库路径（默认 /var/lib/opendiscord/hot.db）
  OPENDISCORD_COLD_DSN         冷库 DSN（doctor 只做 TCP 连通检查）
  OPENDISCORD_BACKUP_SCRIPT    自定义备份脚本
  OPENDISCORD_ADMIN_CREDENTIALS adminctl 凭据文件路径（默认 ~/.config/opendiscord/adminctl.json）

退出码: 0 成功 / 1 错误（doctor 有 FAIL）/ 64 语法 / 65 目标不存在 / 77 未授权或未登录
未配置管理令牌时：先运行 adminctl login。
`

// trimArgs drops empty strings (defensive against shell quoting mistakes).
func trimArgs(args []string) []string {
	out := args[:0]
	for _, a := range args {
		if strings.TrimSpace(a) != "" {
			out = append(out, a)
		}
	}
	return out
}
