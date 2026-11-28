#!/usr/bin/env bash
# release-check.sh —— UFOT 1.0 发布前一键自检。
# 用法：bash scripts/release-check.sh   （也可 sh scripts/release-check.sh；在仓库根目录执行）
#
# 设计约束：本脚本按 POSIX sh 编写（不用数组等 bash 扩展），因此 `bash -n`
# 与 `sh -n` 都必须通过；shebang 仍声明 bash，便于人在 bash 下直接执行。
# 逐步打印 PASS/FAIL/SKIP；任一 FAIL 则退出码 1；SKIP 不计入失败。
set -eu

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
cd "$ROOT"

export GOCACHE="${GOCACHE:-/tmp/gocache}"
export GOTMPDIR="${GOTMPDIR:-/tmp}"
export TMPDIR="${TMPDIR:-/tmp}"
export GOMODCACHE="${GOMODCACHE:-/tmp/gomod}"
export GOFLAGS="${GOFLAGS:--mod=mod}"
export GOPROXY="${GOPROXY:-direct}"

PASS=0
FAIL=0
SKIP=0
RESULTS=""

record_pass() {
  PASS=$((PASS + 1))
  RESULTS="${RESULTS}PASS  $1
"
  printf 'PASS: %s\n' "$1"
}

record_fail() {
  FAIL=$((FAIL + 1))
  RESULTS="${RESULTS}FAIL  $1
"
  printf 'FAIL: %s\n' "$1"
}

record_skip() {
  SKIP=$((SKIP + 1))
  RESULTS="${RESULTS}SKIP  $1
"
  printf 'SKIP: %s（%s）\n' "$1" "$2"
}

# step <名称> <命令...>
step() {
  step_name=$1
  shift
  printf '\n== %s\n' "$step_name"
  if "$@"; then
    record_pass "$step_name"
  else
    record_fail "$step_name"
  fi
}

TMP_SMOKE=""
CHATD_PID=""

cleanup() {
  if [ -n "${CHATD_PID:-}" ] && kill -0 "$CHATD_PID" 2>/dev/null; then
    kill "$CHATD_PID" 2>/dev/null || true
    i=0
    while [ "$i" -lt 50 ] && kill -0 "$CHATD_PID" 2>/dev/null; do
      sleep 0.1
      i=$((i + 1))
    done
    kill -9 "$CHATD_PID" 2>/dev/null || true
    wait "$CHATD_PID" 2>/dev/null || true
  fi
  if [ -n "${TMP_SMOKE:-}" ] && [ -d "$TMP_SMOKE" ]; then
    rm -rf "$TMP_SMOKE"
  fi
}
trap cleanup EXIT INT TERM

echo "######## UFOT release-check（$(date -u +%FT%TZ)）########"

# ---- Go 构建 / 静态检查 -----------------------------------------------------
step "go build ./..." go build ./...
step "go vet ./..." go vet ./...

printf '\n== gofmt -l server/\n'
UNFORMATTED="$(gofmt -l server/ || true)"
if [ -z "$UNFORMATTED" ]; then
  record_pass "gofmt -l server/ 为空"
else
  printf '%s\n' "$UNFORMATTED"
  record_fail "gofmt -l server/ 为空（需格式化：$(printf '%s' "$UNFORMATTED" | tr '\n' ' ')）"
fi

# ---- 部署脚本语法（POSIX sh） ----------------------------------------------
printf '\n== sh -n deploy/*.sh\n'
SH_OK=1
for f in deploy/*.sh; do
  [ -e "$f" ] || continue
  if ! sh -n "$f"; then
    printf '  sh -n 失败: %s\n' "$f"
    SH_OK=0
  fi
done
if [ "$SH_OK" -eq 1 ]; then
  record_pass "sh -n deploy/*.sh"
else
  record_fail "sh -n deploy/*.sh"
fi

# ---- 冒烟：构建并启动 chatd → healthz → guest → 媒体上传/下载 ---------------
printf '\n== 冒烟：准备临时配置并构建 chatd\n'
TMP_SMOKE="$(mktemp -d)"
mkdir -p "$TMP_SMOKE/data/keys"
cp config/policy.json "$TMP_SMOKE/policy.json"

cat > "$TMP_SMOKE/chatd.toml" <<EOF
mode = "embedded"
listen = "127.0.0.1:0"
admin_listen = "127.0.0.1:0"
admin_rest_listen = "127.0.0.1:0"
public_origin = ""
hot_db = "$TMP_SMOKE/data/chatd.db"
admin_db = "$TMP_SMOKE/data/admin.db"
cold_dsn = ""
media_dir = "$TMP_SMOKE/data/media"
policy_path = "$TMP_SMOKE/policy.json"
jwt_key_path = "$TMP_SMOKE/data/keys/jwt_sign_key.pem"
pii_hmac_key_path = "$TMP_SMOKE/data/keys/phone_lookup_key"
log_level = "info"
shutdown_timeout_sec = 10
access_ttl_sec = 900
refresh_ttl_sec = 2592000
dev_otp = true
[feature_flags]
video_enabled = false
e2ee = false
registration = true
readonly = false
call_enabled = false
EOF

if go build -o "$TMP_SMOKE/chatd" ./server/cmd/chatd; then
  record_pass "go build chatd"
else
  record_fail "go build chatd"
fi

PORT=""
if [ -x "$TMP_SMOKE/chatd" ]; then
  APP_ENV=dev "$TMP_SMOKE/chatd" -config "$TMP_SMOKE/chatd.toml" \
    > "$TMP_SMOKE/chatd.log" 2>&1 &
  CHATD_PID=$!

  # 等待 ready 日志（最多 60s）。
  i=0
  while [ "$i" -lt 120 ]; do
    if grep -q '"msg":"chatd ready"' "$TMP_SMOKE/chatd.log" 2>/dev/null; then
      break
    fi
    kill -0 "$CHATD_PID" 2>/dev/null || break
    i=$((i + 1))
    sleep 0.5
  done

  # listen=127.0.0.1:0 时 "chatd ready" 记录的是配置值；SIGUSR1 status dump
  # 才会输出监听后的真实端口（chatd 的 SIGUSR1 是文档化能力）。
  i=0
  while [ "$i" -lt 40 ]; do
    kill -USR1 "$CHATD_PID" 2>/dev/null || break
    sleep 0.25
    PORT="$(sed -n 's/.*"event":"status_dump".*"listen":"127\.0\.0\.1:\([1-9][0-9]*\)".*/\1/p' "$TMP_SMOKE/chatd.log" | tail -n 1)"
    [ -n "$PORT" ] && break
    kill -0 "$CHATD_PID" 2>/dev/null || break
    i=$((i + 1))
  done
fi

if [ -n "$PORT" ]; then
  record_pass "chatd 启动（实际监听 127.0.0.1:$PORT）"
else
  record_fail "chatd 启动（未能从 status dump 解析监听端口）"
  if [ -f "$TMP_SMOKE/chatd.log" ]; then
    tail -n 20 "$TMP_SMOKE/chatd.log" || true
  fi
fi

BASE="http://127.0.0.1:${PORT:-0}"

# GET /healthz 必须 200。
if [ -n "$PORT" ]; then
  HEALTH_OK=0
  i=0
  while [ "$i" -lt 60 ]; do
    CODE="$(curl -sS -m 2 -o "$TMP_SMOKE/healthz.json" -w '%{http_code}' "$BASE/healthz" 2>/dev/null || true)"
    if [ "$CODE" = "200" ]; then
      HEALTH_OK=1
      break
    fi
    i=$((i + 1))
    sleep 0.25
  done
  if [ "$HEALTH_OK" -eq 1 ]; then
    record_pass "GET /healthz 200（$(cat "$TMP_SMOKE/healthz.json")）"
  else
    record_fail "GET /healthz 200（最后状态：${CODE:-无响应}）"
  fi
else
  record_skip "GET /healthz" "chatd 未启动"
fi

# POST /api/auth/guest 必须拿到 access token（Cookie 供后续媒体接口使用）。
CJAR="$TMP_SMOKE/cookies.txt"
GUEST_OK=0
if [ -n "$PORT" ]; then
  if curl -fsS -m 5 -c "$CJAR" -H 'Content-Type: application/json' \
      -d '{"nickname":"release-check"}' "$BASE/api/auth/guest" > "$TMP_SMOKE/guest.json"; then
    TOKEN="$(sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p' "$TMP_SMOKE/guest.json")"
    USER_ID="$(sed -n 's/.*"user_id":\([0-9][0-9]*\).*/\1/p' "$TMP_SMOKE/guest.json")"
    if [ -n "$TOKEN" ]; then
      GUEST_OK=1
      record_pass "POST /api/auth/guest（uid=${USER_ID:-?}，token ${#TOKEN} 字符）"
    else
      record_fail "POST /api/auth/guest（响应缺少 access_token）"
    fi
  else
    record_fail "POST /api/auth/guest"
    tail -n 5 "$TMP_SMOKE/chatd.log" || true
  fi
else
  record_skip "POST /api/auth/guest" "chatd 未启动"
fi

# 媒体上传/下载：printf 写 1x1 合法 PNG（签名 + IHDR + IDAT + IEND）→
# POST /api/media/upload → GET /api/media/{id} → 比对 sha256。
if [ -n "$PORT" ] && [ "$GUEST_OK" -eq 1 ]; then
  printf '\211\120\116\107\015\012\032\012\000\000\000\015\111\110\104\122\000\000\000\001\000\000\000\001\010\006\000\000\000\037\025\304\211\000\000\000\015\111\104\101\124\170\332\143\144\140\370\137\017\000\002\207\001\200\353\107\272\222\000\000\000\000\111\105\116\104\256\102\140\202' > "$TMP_SMOKE/pixel.png"
  EXPECTED_SHA="$(sha256sum "$TMP_SMOKE/pixel.png" | cut -d' ' -f1)"
  if curl -fsS -m 10 -b "$CJAR" -F "file=@$TMP_SMOKE/pixel.png;type=image/png" \
      "$BASE/api/media/upload" > "$TMP_SMOKE/upload.json"; then
    MEDIA_ID="$(sed -n 's/.*"media_id":"\([^"]*\)".*/\1/p' "$TMP_SMOKE/upload.json")"
    if [ -n "$MEDIA_ID" ] && curl -fsS -m 10 -b "$CJAR" "$BASE/api/media/$MEDIA_ID" \
        -o "$TMP_SMOKE/download.bin"; then
      ACTUAL_SHA="$(sha256sum "$TMP_SMOKE/download.bin" | cut -d' ' -f1)"
      if [ "$EXPECTED_SHA" = "$ACTUAL_SHA" ]; then
        record_pass "媒体上传/下载 sha256 一致（media_id=$MEDIA_ID，$ACTUAL_SHA）"
      else
        record_fail "媒体上传/下载 sha256 不一致（expect $EXPECTED_SHA got $ACTUAL_SHA）"
      fi
    else
      record_fail "GET /api/media/{id}（media_id=${MEDIA_ID:-?}）"
    fi
  else
    record_fail "POST /api/media/upload"
    tail -n 5 "$TMP_SMOKE/chatd.log" || true
  fi
else
  record_skip "媒体上传/下载 sha256 比对" "chatd 未启动或 guest 未登录"
fi

# admin 登录：adminctl 依赖交互式 TOTP，脚本化不稳定，明确跳过。
record_skip "admin 登录冒烟" "adminctl 依赖交互式 TOTP，脚本化不可靠（见 docs/admin-guide.md §登录）"

# ---- 汇总 -------------------------------------------------------------------
echo
echo "######## release-check 汇总 ########"
printf '%s' "$RESULTS"
echo "------------------------------------"
echo "PASS=$PASS  FAIL=$FAIL  SKIP=$SKIP"
if [ "$FAIL" -gt 0 ]; then
  echo "结果：FAIL（存在失败步骤，禁止发布）"
  exit 1
fi
echo "结果：OK"
