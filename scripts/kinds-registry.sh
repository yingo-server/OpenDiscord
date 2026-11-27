#!/bin/sh
# kinds-registry.sh — 协议 kind 单一真源门禁（后端仓库版）。
#
# 历史：T-CL-3 曾校验 Go 常量表与前端 TypeScript 镜像逐项一致（双端真源）。
# 前端已从本仓库剔除，TS 镜像不复存在；kind 的唯一权威现在是
# server/internal/proto/kinds/kinds.go 一个文件。本脚本守住它的完整性：
#
#   1. 每个 Kind* 常量都必须注册了 DISPLAY 名称（register(KindX, "NAME")）；
#   2. 同一个数值不得被两个不同常量占用（号段冲突会让路由/能力表语义漂移）；
#   3. v3 号段 15/16/17 必须存在且唯一（v3-bus-design §4.1 前置锁定）。
#
# 用法：sh scripts/kinds-registry.sh
set -u

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" 2>/dev/null && pwd) || script_dir=""
if [ -n "$script_dir" ] && [ -f "$script_dir/../server/internal/proto/kinds/kinds.go" ]; then
  cd "$script_dir/.." || exit 1
fi

GO=server/internal/proto/kinds/kinds.go
TMP=".kinds-registry.$$"
cleanup() { rm -f "$TMP" "$TMP.consts" "$TMP.map" "$TMP.pairs"; }
trap cleanup EXIT INT TERM

[ -f "$GO" ] || { echo "kinds-registry: missing $GO" >&2; exit 1; }

# 常量名 → 数值
awk '
  /^[[:space:]]*Kind[A-Za-z0-9_]+[[:space:]]+Kind[[:space:]]*=[[:space:]]*[0-9]+/ {
    name=$1; val=$4; gsub(/^Kind/, "", name);
    print name, val
  }' "$GO" | sort -u > "$TMP.consts"

# 数值 → 注册名（register(KindX, "NAME")）
awk '
  /register\(Kind[A-Za-z0-9_]+, "[A-Z0-9_]+"\)/ {
    n=$0; sub(/.*register\(Kind/, "", n); sub(/, ".*/, "", n);
    s=$0; sub(/.*, "/, "", s); sub(/"\).*/, "", s);
    print n, s
  }' "$GO" | sort -u > "$TMP.map"

fail=0

# 1. 常量必须有注册名
echo "== kinds without a registered name =="
missing=0
while read -r cname cval; do
  [ -n "$cname" ] || continue
  if ! awk -v n="$cname" '$1==n {found=1} END {exit !found}' "$TMP.map"; then
    echo "  $cname ($cval)"
    missing=1
  fi
done < "$TMP.consts"
[ "$missing" -eq 0 ] && echo "  (none)"

# 2. 同一数值不得被两个常量占用
echo "== duplicate kind values (constant -> value) =="
dup=0
while read -r cname cval; do
  [ -n "$cval" ] || continue
  others=$(awk -v v="$cval" -v n="$cname" '$2==v && $1!=n {print $1}' "$TMP.consts" | tr '\n' ' ')
  if [ -n "$others" ]; then
    echo "  value $cval: $cname <-> $others"
    dup=1
  fi
done < "$TMP.consts"
[ "$dup" -eq 0 ] && echo "  (none)"

# 3. v3 号段锁定（按 DISPLAY 名 + 数值匹配）
echo "== v3 mailbox kinds =="
awk 'NR==FNR {val[$1]=$2; next} {print val[$1], $2}' "$TMP.consts" "$TMP.map" | sort -u > "$TMP.pairs"
for kv in "15 MAILBOX_ACK" "16 MAILBOX_PAUSE" "17 MAILBOX_RESUME"; do
  v=${kv%% *}; n=${kv##* }
  cnt=$(awk -v v="$v" -v n="$n" '$1==v && $2==n' "$TMP.pairs" | wc -l)
  if [ "$cnt" -ne 1 ]; then
    echo "  missing or ambiguous: $v $n"
    fail=1
  fi
done

[ "$fail" -eq 0 ] && echo "kinds registry: OK" || exit 1
