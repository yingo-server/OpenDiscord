#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# OpenDiscord 发布脚本：构建 → 打包 → 上传 → 远程切换 → 健康检查 → 失败回滚
#
#   ./deploy/release.sh --target root@chat.example.com
#   ./deploy/release.sh --target root@host --version v1.4.2 --skip-build
#
# 远端布局（单机）：
#   /opt/opendiscord/releases/<version>/{bin,web}
#   /opt/opendiscord/current -> releases/<version>     # 原子切换点
#   /opt/opendiscord/last    -> releases/<上一版本>     # 回滚点
#   /opt/opendiscord/bin     -> current/bin
#   /opt/opendiscord/web     -> current/web
#
# 回滚语义：健康检查（HTTP /healthz，404 时退化为 systemd is-active）失败
#           → 把 current 指回 last → 重启 → 再次检查 → 仍失败则退出非 0 并保留现场。
# 保留策略：只保留 current 与 last 两个 release 目录（其余在成功发布后清理）。
# ---------------------------------------------------------------------------
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "$0")" && pwd)
REPO_DIR=$(cd -- "$SCRIPT_DIR/.." && pwd)

TARGET="${OPENDISCORD_DEPLOY_TARGET:-}"
REMOTE_ROOT="${OPENDISCORD_REMOTE_ROOT:-/opt/opendiscord}"
SERVICE="${OPENDISCORD_SERVICE:-opendiscord.service}"
HEALTH_URL="${OPENDISCORD_HEALTH_URL:-http://127.0.0.1:8080/healthz}"
SSH_PORT="${OPENDISCORD_SSH_PORT:-22}"
SKIP_BUILD=0
VERSION=""

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[error]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
用法: release.sh --target user@host [选项]

  --target USER@HOST   目标机（也可用 OPENDISCORD_DEPLOY_TARGET）
  --version VERSION    发布版本号（默认 git describe --tags --always --dirty）
  --skip-build         复用已有 dist/ 与 apps/*/dist
  --service NAME       systemd 单元名（默认 opendiscord.service）
  --health-url URL     健康检查地址（默认 http://127.0.0.1:8080/healthz）
  --remote-root DIR    远端根目录（默认 /opt/opendiscord）
  --ssh-port PORT      SSH 端口（默认 22）
  -h, --help
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --target)     TARGET="${2:?}"; shift ;;
    --version)    VERSION="${2:?}"; shift ;;
    --skip-build) SKIP_BUILD=1 ;;
    --service)    SERVICE="${2:?}"; shift ;;
    --health-url) HEALTH_URL="${2:?}"; shift ;;
    --remote-root) REMOTE_ROOT="${2:?}"; shift ;;
    --ssh-port)   SSH_PORT="${2:?}"; shift ;;
    -h|--help)    usage; exit 0 ;;
    *)            die "未知参数: $1" ;;
  esac
  shift
done

[ -n "$TARGET" ] || { usage; die "--target 必填"; }

for c in ssh scp tar sha256sum; do
  command -v "$c" >/dev/null 2>&1 || die "缺少命令: $c"
done

cd "$REPO_DIR"

if [ -z "$VERSION" ]; then
  VERSION=$(git describe --tags --always --dirty 2>/dev/null || date -u +%Y%m%d%H%M%S)
fi
# 版本号会进目录名与 systemd 环境，做一次白名单清洗
VERSION=$(printf '%s' "$VERSION" | tr -c 'A-Za-z0-9._-' '-')
log "发布版本: $VERSION"

# ---------------------------------------------------------------------------
# 1. 构建（默认全量；--skip-build 用于 CI 已构建的场景）
# ---------------------------------------------------------------------------
if [ "$SKIP_BUILD" -eq 1 ]; then
  log "跳过构建（--skip-build）"
else
  log "构建前端（client + admin）"
  if [ -d apps/client ]; then
    ( cd apps/client && npm ci && npm run build )
  else
    warn "apps/client 不存在，跳过用户端构建"
  fi
  ( cd apps/admin && npm ci && npm run build )

  log "构建 Go 二进制（trimpath, -s -w）"
  mkdir -p dist
  for bin in chatd; do
    if [ -d "server/cmd/$bin" ]; then
      CGO_ENABLED="${CGO_ENABLED:-1}" go build -trimpath -ldflags "-s -w" -o "dist/$bin" "./server/cmd/$bin"
      log "  dist/$bin"
    else
      warn "server/cmd/$bin 不存在，跳过"
    fi
  done
  [ -x dist/chatd ] || die "dist/chatd 未生成，构建失败"
fi

# ---------------------------------------------------------------------------
# 2. 打包（tar.gz + SHA256SUMS）
# ---------------------------------------------------------------------------
STAGE="dist/pkg-$VERSION"
rm -rf "$STAGE"
mkdir -p "$STAGE/bin" "$STAGE/web" "$STAGE/deploy"

for f in dist/chatd; do
  [ -f "$f" ] && install -m 0755 "$f" "$STAGE/bin/"
done

if [ -d apps/admin/dist ]; then
  mkdir -p "$STAGE/web/admin"
  cp -R apps/admin/dist/. "$STAGE/web/admin/"
fi
if [ -d apps/client/dist ]; then
  mkdir -p "$STAGE/web/client"
  cp -R apps/client/dist/. "$STAGE/web/client/"
fi

# 部署件随包发布，保证「发布即自包含」
cp -R deploy/systemd deploy/policy.json deploy/opendiscord.toml deploy/Caddyfile "$STAGE/deploy/"
cp deploy/install.sh "$STAGE/deploy/install.sh"
chmod 0755 "$STAGE/deploy/install.sh"

TARBALL="dist/opendiscord-$VERSION-linux-$(go env GOARCH 2>/dev/null || echo amd64).tar.gz"
tar -czf "$TARBALL" -C "$STAGE" .
( cd dist && sha256sum "$(basename "$TARBALL")" > SHA256SUMS )
log "产物: $TARBALL"
log "校验: dist/SHA256SUMS"

# ---------------------------------------------------------------------------
# 3. 传输
# ---------------------------------------------------------------------------
SSH="ssh -p $SSH_PORT -o BatchMode=yes -o ConnectTimeout=10"
SCP="scp -P $SSH_PORT -o BatchMode=yes -o ConnectTimeout=10"

log "上传到 $TARGET:$REMOTE_ROOT/incoming"
$SSH "$TARGET" "install -d -m 0755 '$REMOTE_ROOT' '$REMOTE_ROOT/incoming' '$REMOTE_ROOT/releases'"
$SCP "$TARBALL" dist/SHA256SUMS "$TARGET:$REMOTE_ROOT/incoming/"

# ---------------------------------------------------------------------------
# 4. 远端切换 + 健康检查 + 自动回滚
# ---------------------------------------------------------------------------
log "远端安装 release 并切换 current"

# 先记录回滚点（当前 current 指向的目录）
PREV=$($SSH "$TARGET" "readlink -f '$REMOTE_ROOT/current' 2>/dev/null || true")
RELEASE_DIR="$REMOTE_ROOT/releases/$VERSION"

$SSH "$TARGET" "set -euo pipefail
  cd '$REMOTE_ROOT/incoming'
  sha256sum -c SHA256SUMS
  rm -rf '$RELEASE_DIR'
  install -d -m 0755 '$RELEASE_DIR'
  tar -xzf '$(basename "$TARBALL")' -C '$RELEASE_DIR'
  chown -R root:root '$RELEASE_DIR'
  chmod 0755 '$RELEASE_DIR/bin'/* 2>/dev/null || true
"

restart_remote() {
  $SSH "$TARGET" "systemctl restart '$SERVICE'"
}

health_check() {
  # 最多等 20s：启动 → sd_notify → 端口就绪
  i=0
  while [ $i -lt 20 ]; do
    if $SSH "$TARGET" "curl -fsS --max-time 3 '$HEALTH_URL' >/dev/null 2>&1"; then
      return 0
    fi
    # /healthz 尚未实现时（当前骨架）退化为单元 active 检查
    code=$($SSH "$TARGET" "curl -s -o /dev/null -w '%{http_code}' --max-time 3 '$HEALTH_URL' 2>/dev/null || true")
    if [ "$code" = "404" ]; then
      warn "健康检查端点返回 404（未实现），退化为 systemctl is-active"
      $SSH "$TARGET" "systemctl is-active --quiet '$SERVICE'"
      return $?
    fi
    i=$((i + 1))
    sleep 1
  done
  return 1
}

switch_to() {
  $SSH "$TARGET" "set -e
    test -x '$1/bin/chatd'
    ln -sfn '$1' '$REMOTE_ROOT/current.new'
    mv -Tf '$REMOTE_ROOT/current.new' '$REMOTE_ROOT/current'
    ln -sfn current/bin '$REMOTE_ROOT/bin'
    ln -sfn current/web '$REMOTE_ROOT/web'
  "
}

rollback() {
  warn "健康检查失败，开始回滚"
  if [ -z "${PREV:-}" ]; then
    die "没有可回滚的上一版本（首次发布且启动失败，请查看 journalctl -u $SERVICE）"
  fi
  switch_to "$PREV" || die "回滚切换失败，需人工介入"
  $SSH "$TARGET" "systemctl restart '$SERVICE'" || true
  if health_check; then
    warn "已回滚到 $PREV"
    die "发布 $VERSION 失败，已回滚（退出码 1）"
  fi
  die "发布与回滚均失败，服务不可用 —— 立即人工介入（journalctl -u $SERVICE -n 100）"
}

switch_to "$RELEASE_DIR"
restart_remote || true

if ! health_check; then
  rollback
fi

# ---------------------------------------------------------------------------
# 5. 记录 last 并清理旧 release（只留 current + last）
# ---------------------------------------------------------------------------
$SSH "$TARGET" "set -e
  if [ -n '$PREV' ] && [ -d '$PREV' ] && [ '$(basename "$PREV")' != '$VERSION' ]; then
    ln -sfn '$PREV' '$REMOTE_ROOT/last.new'
    mv -Tf '$REMOTE_ROOT/last.new' '$REMOTE_ROOT/last'
  fi
  rm -f '$REMOTE_ROOT/incoming/$(basename "$TARBALL")'
  # 只保留 current/last 指向的目录 + 最近 2 个版本，其余清理
  keep1=\$(readlink -f '$REMOTE_ROOT/current' || true)
  keep2=\$(readlink -f '$REMOTE_ROOT/last' 2>/dev/null || true)
  for d in '$REMOTE_ROOT'/releases/*; do
    [ -d \"\$d\" ] || continue
    case \"\$d\" in
      \"\$keep1\"|\"\$keep2\"|'$REMOTE_ROOT/releases/bootstrap') ;;
      *) rm -rf \"\$d\" ;;
    esac
  done
"

log "发布成功: $VERSION"
log "回滚命令: ssh -p $SSH_PORT $TARGET \"ln -sfn \\\$(readlink -f $REMOTE_ROOT/last) $REMOTE_ROOT/current && systemctl restart $SERVICE\""
