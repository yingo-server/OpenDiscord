#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# OpenDiscord 单机安装 / 升级引导脚本（幂等，可反复执行）
#
#   sudo ./deploy/install.sh                 # embedded 单进程（1.4 起 embedded-only）
#   sudo ./deploy/install.sh --no-start      # 只装文件，不 enable/start（CI/镜像构建）
#
# 做四件事：建用户与目录（按 security-spec §6.2 权限矩阵）→ 生成密钥
# → 安装 systemd unit 与配置 → daemon-reload + enable --now。
#
# 幂等约定：已存在的配置/密钥/环境文件绝不覆盖；目录权限每次强制校正。
# 依赖：bash、coreutils、systemd；密钥优先用 openssl（≥1.1.1），
#       缺 openssl 时回退到仓库内 Go 工具 `go run ./deploy/keygen`（需 Go ≥1.20）。
# ---------------------------------------------------------------------------
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "$0")" && pwd)
REPO_DIR=$(cd -- "$SCRIPT_DIR/.." && pwd)

PREFIX="${OPENDISCORD_PREFIX:-/opt/opendiscord}"
ETC="${OPENDISCORD_ETC:-/etc/opendiscord}"
STATE="${OPENDISCORD_STATE:-/var/lib/opendiscord}"

MODE="embedded"
START=1
SKIP_KEYS=0
DOMAIN=""
ADMIN_USER=""
ADMIN_PASS_FILE=""
TURN_SERVER=""
LANG_FLAG=""

# Color helpers: green ✓ / yellow ⚠ / red ✗ — auto-stripped when stdout is
# not a TTY (CI/logs).
if [ -t 1 ]; then
  ok()   { printf '\033[32m✓\033[0m %s\n' "$*"; }
else
  ok()   { printf 'ok: %s\n' "$*"; }
fi
log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[error]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
用法: install.sh [选项]

  --no-start       不执行 systemctl enable --now（仅落盘）
  --skip-keys      跳过密钥生成（密钥由 KMS/配置管理下发时使用）
  --prefix DIR     安装前缀（默认 /opt/opendiscord）
  --etc DIR        配置目录（默认 /etc/opendiscord）
  --state DIR      数据目录（默认 /var/lib/opendiscord）
  --domain URL     公网来源，写进 opendiscord.toml 的 public_origin 注释头
  --admin-user U   首启管理员用户名（写入 /etc/opendiscord/env，0600）
  --admin-pass-file PATH
                   管理员密码文件（首启引导，写入 env，0600）。
                   绝不接受命令行明文密码——只收文件，或在 TTY 下用
                   deploy/install-interactive 后置设置。
  --cold-dsn DSN   冷库 Postgres DSN（写入 env 注释/值，可选）
  --turn-server U  TURN 服务器（可选，写入配置注释）
  --lang zh|en     输出语言（默认跟随 LANG/LC_ALL，含 zh → 中文）
  -h, --help       显示本帮助

退出码：0 成功 / 64 用法错误 / 77 权限不足 / 78 配置错误
TODO(i18n): 存量帮助文本本轮不改造成 i18n key，仅新增部分走 i18n。
EOF
}

# t_msg: minimal i18n for new messages (en / zh); key falls back to itself.
t_msg() {
  local lang="${LANG_FLAG:-${LC_ALL:-${LANG:-en}}}"
  case "$lang" in *zh*) ;; *) lang=en ;; esac
  case "$1:$lang" in
    admin_env_written:zh) printf '管理员引导已写入（0600，绝不入库/日志）' ;;
    admin_env_written:en) printf 'admin bootstrap written (0600, never in VCS/logs)' ;;
    domain_set:zh)        printf 'public_origin 已记录' ;;
    domain_set:en)        printf 'public_origin recorded' ;;
    *) printf '%s' "$1" ;;
  esac
}

while [ $# -gt 0 ]; do
  case "$1" in
    --split)     die "--split 已移除：split 多进程部署未实现，OpenDiscord 为 embedded-only（lts-1.4-plan §5 #4）" ;;
    --no-start)  START=0 ;;
    --skip-keys) SKIP_KEYS=1 ;;
    --domain)    DOMAIN="${2:?--domain 需要参数}"; shift ;;
    --admin-user) ADMIN_USER="${2:?--admin-user 需要参数}"; shift ;;
    --admin-pass-file) ADMIN_PASS_FILE="${2:?--admin-pass-file 需要参数}"; shift ;;
    --cold-dsn)  COLD_DSN="${2:-}"; shift ;;
    --turn-server) TURN_SERVER="${2:-}"; shift ;;
    --lang)      LANG_FLAG="${2:?--lang 需要参数}"; shift ;;
    --prefix)    PREFIX="${2:?--prefix 需要参数}"; shift ;;
    --etc)       ETC="${2:?--etc 需要参数}"; shift ;;
    --state)     STATE="${2:?--state 需要参数}"; shift ;;
    -h|--help)   usage; exit 0 ;;
    *)           warn "未知参数: $1"; usage; exit 64 ;;
  esac
  shift
done

[ "$(id -u)" -eq 0 ] || die "需要 root 权限（退出码 77）"

# 命令可用性
need() { command -v "$1" >/dev/null 2>&1 || die "缺少命令: $1"; }
need install
need useradd
need groupadd
need getent

# ---------------------------------------------------------------------------
# 1. 用户与组
# ---------------------------------------------------------------------------
log "创建系统用户与组（embedded-only）"

if ! getent group opendiscord >/dev/null 2>&1; then
  groupadd --system opendiscord
  log "  创建组 opendiscord"
fi

make_user() {
  _u="$1"
  if getent passwd "$_u" >/dev/null 2>&1; then
    log "  用户 $_u 已存在"
  else
    useradd --system --gid opendiscord --home-dir "$STATE" \
      --shell "$(command -v nologin || echo /usr/sbin/nologin)" \
      --comment "OpenDiscord service ($_u)" "$_u"
    log "  创建用户 $_u"
  fi
}

make_user opendiscord

# ---------------------------------------------------------------------------
# 2. 目录与权限（security-spec §6.2）
# ---------------------------------------------------------------------------
log "创建目录并校正权限"

# /etc/opendiscord 0750 root:opendiscord —— 配置根
install -d -m 0750 -o root -g opendiscord "$ETC"
# 密钥目录 0750，密钥文件 0640（单密钥单文件）
install -d -m 0750 -o root -g opendiscord "$ETC/keys"
# 兼容当前 chatd 骨架（从工作目录读 config/chatd.toml）
install -d -m 0750 -o root -g opendiscord "$ETC/config"

# 运行数据 0700；媒体 0750（经 LUKS 卷）；备份 0700
install -d -m 0700 -o opendiscord -g opendiscord "$STATE"
install -d -m 0750 -o opendiscord -g opendiscord "$STATE/media"
install -d -m 0700 -o opendiscord -g opendiscord "$STATE/backup"

# 二进制目录：/opt/opendiscord 0755 root:root，运行用户不可写
install -d -m 0755 -o root -g root "$PREFIX"
install -d -m 0755 -o root -g root "$PREFIX/releases"
install -d -m 0755 -o root -g root "$PREFIX/releases/bootstrap"
install -d -m 0755 -o root -g root "$PREFIX/releases/bootstrap/bin"
install -d -m 0755 -o root -g root "$PREFIX/releases/bootstrap/web"

# ---------------------------------------------------------------------------
# 3. 环境变量文件（0600 root:opendiscord，systemd 以 root 读取后再降权）
# ---------------------------------------------------------------------------
log "写入环境变量文件"

write_env() {
  _path="$1"; _extra="$2"
  if [ -f "$_path" ]; then
    log "  $_path 已存在，保持不变"
  else
    cat > "$_path" <<EOF
# OpenDiscord 环境变量（KEY=VALUE）。mode 0600，禁止提交版本库。
# 修改后：systemctl restart opendiscord（或对应层）
# 只放运行时参数；策略类配置放 /etc/opendiscord/policy.json
TZ=UTC
RUST_BACKTRACE=0
GOMEMLIMIT=1280MiB
OPENDISCORD_CONFIG=$ETC/opendiscord.toml
${_extra}
EOF
    log "  创建 $_path"
  fi
  chown root:opendiscord "$_path"
  chmod 0600 "$_path"
}

# 首启管理员引导（--admin-user/--admin-pass-file）：绝不接受命令行明文密码，
# 只收 0600 密码文件；写入 env（0600），由 chatd 首启消费。
admin_env_extra=""
if [ -n "$ADMIN_USER" ]; then
  admin_env_extra+="OPENDISCORD_ADMIN_USER=$ADMIN_USER"$'\n'
fi
if [ -n "$ADMIN_PASS_FILE" ]; then
  if [ ! -f "$ADMIN_PASS_FILE" ]; then
    die "admin pass file 不存在: $ADMIN_PASS_FILE（退出码 78）"
  fi
  admin_env_extra+="OPENDISCORD_ADMIN_PASS_FILE=$ADMIN_PASS_FILE"$'\n'
  ok "$(t_msg admin_env_written)"
fi
[ -n "$COLD_DSN" ] && admin_env_extra+="OPENDISCORD_COLD_DSN=$COLD_DSN"$'\n'
if [ -n "$TURN_SERVER" ]; then
  # TURN 服务器写进配置注释（未配置凭据前不生效）
  admin_env_extra+="# TURN server: $TURN_SERVER (see deploy/coturn)"$'\n'
fi

write_env "$ETC/env" "OPENDISCORD_LAYER=embedded"$'\n'"$admin_env_extra"
# ---------------------------------------------------------------------------
# 4. 密钥（security-spec §6.4.1 清单）
#    Ed25519 用 openssl genpkey（OpenSSL ≥1.1.1）；
#    无 openssl 时回退 `go run ./deploy/keygen`（等价实现，见该文件注释）。
#    已存在的密钥绝不覆盖 —— 轮换是独立流程，不能由安装脚本隐式触发。
# ---------------------------------------------------------------------------
if [ "$SKIP_KEYS" -eq 0 ]; then
  log "生成密钥到 $ETC/keys（已存在的跳过）"

  if command -v openssl >/dev/null 2>&1; then
    if [ ! -s "$ETC/keys/jwt_sign_key.pem" ]; then
      openssl genpkey -algorithm ED25519 -out "$ETC/keys/jwt_sign_key.pem" 2>/dev/null
      log "  jwt_sign_key.pem (Ed25519)"
    fi
    if [ ! -s "$ETC/keys/admin_jwt_key.pem" ]; then
      openssl genpkey -algorithm ED25519 -out "$ETC/keys/admin_jwt_key.pem" 2>/dev/null
      log "  admin_jwt_key.pem (Ed25519，管理端独立)"
    fi
    for name in ipc_key hmac_key pii_key phone_lookup_key download_key upload_secret audit_chain_key; do
      if [ ! -s "$ETC/keys/$name" ]; then
        openssl rand -hex 32 > "$ETC/keys/$name"
        log "  $name (32B hex)"
      fi
    done
    if [ ! -s "$ETC/keys/backup.key" ]; then
      openssl rand -hex 32 > "$ETC/keys/backup.key"
      log "  backup.key (备份加密口令)"
    fi
  else
    warn "未找到 openssl，回退到 Go keygen（go run ./deploy/keygen）"
    command -v go >/dev/null 2>&1 || die "openssl 与 go 均不可用：请先安装 openssl（退出码 78）"
    ( cd "$REPO_DIR" && go run ./deploy/keygen -dir "$ETC/keys" )
  fi

  # 密钥文件按矩阵：私钥 0640 root:opendiscord；对称密钥与备份口令 0600
  chown root:opendiscord "$ETC"/keys/*.pem 2>/dev/null || true
  chmod 0640 "$ETC"/keys/*.pem 2>/dev/null || true
  chown root:opendiscord "$ETC/keys"/* 2>/dev/null || true
  chmod 0600 "$ETC/keys/ipc_key" "$ETC/keys/hmac_key" "$ETC/keys/pii_key" \
             "$ETC/keys/phone_lookup_key" "$ETC/keys/download_key" \
             "$ETC/keys/upload_secret" "$ETC/keys/audit_chain_key" "$ETC/keys/backup.key" 2>/dev/null || true

  if command -v openssl >/dev/null 2>&1 && [ -s "$ETC/keys/jwt_sign_key.pem" ]; then
    fp=$(openssl pkey -in "$ETC/keys/jwt_sign_key.pem" -pubout 2>/dev/null | openssl dgst -sha256 2>/dev/null | awk '{print $NF}' || true)
    [ -n "${fp:-}" ] && log "  jwt_sign_key 指纹: $fp"
  fi

  # 备份脚本的文档化接口：/etc/opendiscord/backup.key → keys/backup.key（同一份密钥）
  ln -sfn keys/backup.key "$ETC/backup.key"
  chown -h root:root "$ETC/backup.key"
else
  log "跳过密钥生成（--skip-keys）"
fi

# ---------------------------------------------------------------------------
# 5. 配置（已存在不覆盖）
# ---------------------------------------------------------------------------
log "安装配置文件"

install_cfg() {
  _src="$1"; _dst="$2"; _mode="$3"
  if [ -f "$_dst" ]; then
    log "  $_dst 已存在，保持不变（升级请手工 merge）"
  else
    install -m "$_mode" -o root -g opendiscord "$_src" "$_dst"
    log "  安装 $_dst"
  fi
}

install_cfg "$SCRIPT_DIR/opendiscord.toml" "$ETC/opendiscord.toml" 0640

# --domain actually lands in public_origin: the previous version only printed
# "recorded" and left the config untouched, so a public deployment silently ran
# with an empty public_origin (same-origin checks and SecureCookie decisions
# then take the lenient branch).
if [ -n "$DOMAIN" ]; then
  case "$DOMAIN" in
    https://*) ;;
    http://*) warn "public_origin 使用 http://，生产请用 https://" ;;
    *) die "--domain 需要带 scheme 的 URL（例如 https://chat.example.com）" ;;
  esac
  if [ -f "$ETC/opendiscord.toml" ]; then
    if grep -q '^[[:space:]]*public_origin[[:space:]]*=' "$ETC/opendiscord.toml"; then
      sed -i "s|^[[:space:]]*public_origin[[:space:]]*=.*|public_origin = \"$DOMAIN\"|" "$ETC/opendiscord.toml"
      ok "  public_origin = $DOMAIN"
    else
      printf '\npublic_origin = "%s"\n' "$DOMAIN" >> "$ETC/opendiscord.toml"
      ok "  public_origin = $DOMAIN"
    fi
  fi
fi
install_cfg "$SCRIPT_DIR/policy.json"   "$ETC/policy.json"   0640
install -m 0640 -o root -g opendiscord "$SCRIPT_DIR/Caddyfile" "$ETC/Caddyfile.reference"

# 当前 chatd 骨架从工作目录读 config/chatd.toml；用相对符号链接指向正式配置。
# WorkingDirectory=/etc/opendiscord，因此 config/chatd.toml → ../opendiscord.toml。
ln -sfn ../opendiscord.toml "$ETC/config/chatd.toml"

# ---------------------------------------------------------------------------
# 6. 二进制与前端产物（release layout: releases/<ver> + current 符号链接）
# ---------------------------------------------------------------------------
log "安装二进制与内嵌前端"

BOOT="$PREFIX/releases/bootstrap"
installed_bin=0
for b in chatd; do
  if [ -f "$REPO_DIR/dist/$b" ]; then
    install -m 0755 -o root -g root "$REPO_DIR/dist/$b" "$BOOT/bin/$b"
    installed_bin=1
  fi
done
if [ "$installed_bin" -eq 0 ]; then
  warn "未找到 $REPO_DIR/dist/chatd —— 先执行 make build 或 make release-all，再重跑本脚本"
fi

for app in client admin; do
  if [ -d "$REPO_DIR/apps/$app/dist" ]; then
    install -d -m 0755 -o root -g root "$BOOT/web/$app"
    cp -R "$REPO_DIR/apps/$app/dist/." "$BOOT/web/$app/"
    chown -R root:root "$BOOT/web/$app"
    log "  内嵌 apps/$app/dist → $BOOT/web/$app"
  fi
done

# current / bin / web 全部用符号链接，发布只切换 current（原子 rename）
ln -sfn releases/bootstrap "$PREFIX/current"
if [ -e "$PREFIX/bin" ] && [ ! -L "$PREFIX/bin" ]; then
  mv "$PREFIX/bin" "$PREFIX/bin.pre-symlink.$$"
  warn "已把真实目录 $PREFIX/bin 移到 $PREFIX/bin.pre-symlink.$$"
fi
ln -sfn current/bin "$PREFIX/bin"
if [ -e "$PREFIX/web" ] && [ ! -L "$PREFIX/web" ]; then
  mv "$PREFIX/web" "$PREFIX/web.pre-symlink.$$"
fi
ln -sfn current/web "$PREFIX/web"

# ---------------------------------------------------------------------------
# 7. systemd units 与 journald 配额
# ---------------------------------------------------------------------------
log "安装 systemd unit"

UNIT_DIR="/etc/systemd/system"
install -d -m 0755 -o root -g root "$UNIT_DIR"
install -m 0644 -o root -g root "$SCRIPT_DIR/systemd/opendiscord.service" "$UNIT_DIR/opendiscord.service"
log "  opendiscord.service"

install -d -m 0755 -o root -g root /etc/systemd/journald.conf.d
install -m 0644 -o root -g root "$SCRIPT_DIR/journald/opendiscord.conf" /etc/systemd/journald.
# ---------------------------------------------------------------------------
# 8. 生效
# ---------------------------------------------------------------------------
if ! command -v systemctl >/dev/null 2>&1; then
  warn "systemctl 不可用：跳过 daemon-reload/启动（容器或非 systemd 环境）"
elif [ "$START" -eq 1 ]; then
  log "daemon-reload + enable --now"
  systemctl daemon-reload
  systemctl try-restart systemd-journald >/dev/null 2>&1 || true
  systemctl enable --now opendiscord.service
  sleep 1
  systemctl --no-pager --lines=0 status opendiscord.service 2>/dev/null | head -5 || true
else
  log "已跳过启动（--no-start）"
  systemctl daemon-reload 2>/dev/null || true
fi

# ---------------------------------------------------------------------------
# 9. 红线自检：other 位不得有 rwx（security-spec §6.2）
# ---------------------------------------------------------------------------
violations=$(find "$ETC" "$STATE" -perm /o+rwx 2>/dev/null || true)
if [ -n "$violations" ]; then
  warn "以下路径对 other 开放（违反 security-spec §6.2 红线）："
  printf '%s\n' "$violations"
  exit 1
fi

# ---------------------------------------------------------------------------
# 10. 后续步骤
# ---------------------------------------------------------------------------
cat <<EOF

安装完成（embedded-only）

下一步：
  1. 编辑 $ETC/opendiscord.toml
       - public_origin = https://你的域名
       - cold_dsn      = postgres://...（冷库）
       - listen/admin_listen 保持仅监听 127.0.0.1
  2. 编辑 $ETC/policy.json（保留天数/归档 cron/备份 cron/限流），改完：
       systemctl reload opendiscord
  3. 配置 Caddy（参考 $ETC/Caddyfile.reference）：
       OPENDISCORD_DOMAIN=chat.example.com
       把 /api/admin/* 的 remote_ip 白名单改成你的运维出口 IP
  4. 防火墙只放行 22 / 80 / 443（security-spec §6.3 红线）
  5. 首次登录管理后台后立即：开启 TOTP、核对 IP 白名单、确认审计链校验任务
  6. 自检：opendiscordctl status && journalctl -u opendiscord -n 50 --no-pager

升级与回滚：deploy/release.sh（保留 current/last 两个 release 目录）
备份：deploy/backup.sh（默认 05:30，由 policy.json 的 backup.cron 调度）
EOF
