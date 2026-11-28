package main

import (
	"os"
	"strings"
)

// i18n: two builtin locales, zh-CN and en. Keys are stable identifiers;
// translations live in the tables below. No external dependency.
const (
	kTitle           = "title"
	kTabOverview     = "tab_overview"
	kTabUsers        = "tab_users"
	kTabReports      = "tab_reports"
	kTabBroadcast    = "tab_broadcast"
	kTabFlags        = "tab_flags"
	kTabArchive      = "tab_archive"
	kTabAudit        = "tab_audit"
	kQuit            = "quit"
	kConnected       = "connected"
	kLang            = "lang"
	kStatusBar       = "status_bar"
	kSelectHint      = "select_hint"
	kConfirmHint     = "confirm_hint"
	kUsage           = "usage"
	kNoCreds         = "err_no_creds"
	kLoginHint       = "err_login_hint"
	kLoginURL        = "login_url_prompt"
	kLoginUser       = "login_user_prompt"
	kLoginPass       = "login_pass_prompt"
	kLoginTOTP       = "login_totp_prompt"
	kLoginOK         = "login_ok"
	kLoginFail       = "login_failed"
	kWillCall        = "will_call_api"
	kOverviewOnline  = "overview_online"
	kOverviewUsers   = "overview_users"
	kOverviewMsgs    = "overview_msgs_today"
	kOverviewArchive = "overview_archive"
	kRefreshIn       = "refresh_in"
	kSearchPrompt    = "users_search_prompt"
	kNoResults       = "no_results"
	kUserDetail      = "user_detail"
	kMaskedPhone     = "masked_phone"
	kMaskedEmail     = "masked_email"
	kOpBan           = "op_ban"
	kOpUnban         = "op_unban"
	kOpKick          = "op_kick"
	kBanLevel        = "ban_level_prompt"
	kBanDays         = "ban_days_prompt"
	kBanReason       = "ban_reason_prompt"
	kReportsFilter   = "reports_status_prompt"
	kReportsAction   = "reports_action_prompt"
	kReportsNote     = "reports_note_prompt"
	kBroadcastTitle  = "broadcast_title"
	kBroadcastText   = "broadcast_text"
	kBroadcastReach  = "broadcast_reach"
	kBroadcastSent   = "broadcast_sent"
	kFlagsValue      = "flags_value_prompt"
	kFlagsBadJSON    = "flags_invalid_json"
	kArchiveDry      = "archive_dryrun"
	kArchiveRun      = "archive_run_confirm"
	kAuditMore       = "audit_more"
	kAuditExpand     = "audit_expand"
	kHelpTitle       = "help_title"
	kToastOK         = "toast_ok"
	kToastErr        = "toast_err"
	kBack            = "back"
	kYesNo           = "yes_no"
	kRefresh         = "refreshing"
	kSoundOn         = "sound_on"
	kSoundOff        = "sound_off"
	kCancelled       = "cancelled"
	kAPIErr          = "api_error"
	kSuggestion      = "suggestion"
	kLoginWizard     = "login_wizard"
	kUncommitted     = "uncommitted_kept"
)

var zhCN = map[string]string{
	kTitle:           "opchat 管理控制台",
	kTabOverview:     "概览",
	kTabUsers:        "用户",
	kTabReports:      "举报",
	kTabBroadcast:    "广播",
	kTabFlags:        "Flags",
	kTabArchive:      "归档",
	kTabAudit:        "审计",
	kQuit:            "退出",
	kConnected:       "已连接",
	kLang:            "语言",
	kStatusBar:       "状态栏",
	kSelectHint:      "↑↓选择 Enter确认",
	kConfirmHint:     "y 确认 / N 取消（默认 N）",
	kUsage:           "用法: opchat [--lang zh-CN|en] [--sound]\n  opchat 是纯 TUI 管理控制台，需要在 TTY 中运行。\n  非交互环境（如管道）仅打印本用法并以退出码 64 退出。",
	kNoCreds:         "未找到管理员凭据",
	kLoginHint:       "请先登录（向导已启动），或运行: adminctl login --server <URL>",
	kLoginURL:        "管理 API 地址 [http://127.0.0.1:7780]: ",
	kLoginUser:       "用户名: ",
	kLoginPass:       "密码: ",
	kLoginTOTP:       "TOTP 验证码: ",
	kLoginOK:         "登录成功",
	kLoginFail:       "登录失败",
	kWillCall:        "即将调用",
	kOverviewOnline:  "在线连接",
	kOverviewUsers:   "总用户",
	kOverviewMsgs:    "今日消息",
	kOverviewArchive: "归档",
	kRefreshIn:       "刷新倒计时",
	kSearchPrompt:    "输入搜索关键字 q: ",
	kNoResults:       "（无结果）",
	kUserDetail:      "用户详情",
	kMaskedPhone:     "手机（掩码）",
	kMaskedEmail:     "邮箱（掩码）",
	kOpBan:           "封禁",
	kOpUnban:         "解封",
	kOpKick:          "踢下线",
	kBanLevel:        "封禁等级 1-3 [1]: ",
	kBanDays:         "封禁天数（0=永久）[0]: ",
	kBanReason:       "原因: ",
	kReportsFilter:   "状态过滤（-1 全部）: ",
	kReportsAction:   "处置动作 claim|resolve|reject: ",
	kReportsNote:     "处理备注: ",
	kBroadcastTitle:  "广播标题: ",
	kBroadcastText:   "广播内容: ",
	kBroadcastReach:  "预计触达",
	kBroadcastSent:   "广播已发送",
	kFlagsValue:      "新值（JSON）: ",
	kFlagsBadJSON:    "不是合法 JSON，未保存",
	kArchiveDry:      "dry-run 预演：将搬移行数",
	kArchiveRun:      "确认执行归档？这将搬移历史数据",
	kAuditMore:       "↓ 加载更多",
	kAuditExpand:     "u 展开详情 JSON",
	kHelpTitle:       "快捷键帮助",
	kToastOK:         "操作成功",
	kToastErr:        "操作失败",
	kBack:            "返回",
	kYesNo:           "y/N",
	kRefresh:         "刷新中",
	kSoundOn:         "声效已开启（终端 bell）",
	kSoundOff:        "声效已关闭",
	kCancelled:       "已取消，未做任何修改",
	kAPIErr:          "API 错误",
	kSuggestion:      "建议",
	kLoginWizard:     "登录向导",
	kUncommitted:     "未提交的修改已保留",
}

var en = map[string]string{
	kTitle:           "opchat admin console",
	kTabOverview:     "Overview",
	kTabUsers:        "Users",
	kTabReports:      "Reports",
	kTabBroadcast:    "Broadcast",
	kTabFlags:        "Flags",
	kTabArchive:      "Archive",
	kTabAudit:        "Audit",
	kQuit:            "Quit",
	kConnected:       "connected",
	kLang:            "lang",
	kStatusBar:       "status",
	kSelectHint:      "↑↓ select, Enter confirm",
	kConfirmHint:     "y to confirm / N to cancel (default N)",
	kUsage:           "Usage: opchat [--lang zh-CN|en] [--sound]\n  opchat is a pure-TUI admin console and requires a TTY.\n  In non-interactive contexts (pipes) it prints this usage and exits 64.",
	kNoCreds:         "no admin credentials found",
	kLoginHint:       "login first (wizard starting), or run: adminctl login --server <URL>",
	kLoginURL:        "Admin API URL [http://127.0.0.1:7780]: ",
	kLoginUser:       "Username: ",
	kLoginPass:       "Password: ",
	kLoginTOTP:       "TOTP code: ",
	kLoginOK:         "login ok",
	kLoginFail:       "login failed",
	kWillCall:        "about to call",
	kOverviewOnline:  "online connections",
	kOverviewUsers:   "total users",
	kOverviewMsgs:    "messages today",
	kOverviewArchive: "archive",
	kRefreshIn:       "refresh in",
	kSearchPrompt:    "search query q: ",
	kNoResults:       "(no results)",
	kUserDetail:      "user detail",
	kMaskedPhone:     "phone (masked)",
	kMaskedEmail:     "email (masked)",
	kOpBan:           "Ban",
	kOpUnban:         "Unban",
	kOpKick:          "Kick",
	kBanLevel:        "ban level 1-3 [1]: ",
	kBanDays:         "days (0=permanent) [0]: ",
	kBanReason:       "reason: ",
	kReportsFilter:   "status filter (-1 all): ",
	kReportsAction:   "action claim|resolve|reject: ",
	kReportsNote:     "note: ",
	kBroadcastTitle:  "broadcast title: ",
	kBroadcastText:   "broadcast text: ",
	kBroadcastReach:  "estimated reach",
	kBroadcastSent:   "broadcast sent",
	kFlagsValue:      "new value (JSON): ",
	kFlagsBadJSON:    "invalid JSON, not saved",
	kArchiveDry:      "dry-run: rows to move",
	kArchiveRun:      "confirm archive run? this moves historical rows",
	kAuditMore:       "↓ load more",
	kAuditExpand:     "u expand detail JSON",
	kHelpTitle:       "keybindings",
	kToastOK:         "success",
	kToastErr:        "failed",
	kBack:            "back",
	kYesNo:           "y/N",
	kRefresh:         "refreshing",
	kSoundOn:         "sound on (terminal bell)",
	kSoundOff:        "sound off",
	kCancelled:       "cancelled, nothing changed",
	kAPIErr:          "API error",
	kSuggestion:      "suggestion",
	kLoginWizard:     "login wizard",
	kUncommitted:     "uncommitted changes kept",
}

type locale struct {
	m map[string]string
}

// L returns the translation for key. Missing keys fall back to the key itself
// so the UI never renders an empty label.
func (l locale) L(key string) string {
	if s, ok := l.m[key]; ok {
		return s
	}
	return key
}

func detectLang() string {
	for _, v := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		s := strings.ToLower(os.Getenv(v))
		if strings.Contains(s, "zh") {
			return "zh-CN"
		}
		if s != "" {
			return "en"
		}
	}
	return "en"
}

func localeFor(lang string) locale {
	if strings.HasPrefix(strings.ToLower(lang), "zh") {
		return locale{m: zhCN}
	}
	return locale{m: en}
}
