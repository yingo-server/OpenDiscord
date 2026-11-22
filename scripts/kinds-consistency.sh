#!/bin/sh
# kinds-consistency.sh — T-CL-3：kinds.ts ↔ kinds.go 常量一致性（v3-bus-design §9 P0/P1）。
# 双端唯一权威：server/internal/proto/kinds/kinds.go + apps/client/src/net/protocol/kinds.ts。
# 用法：sh scripts/kinds-consistency.sh   （repo root 或任意目录，自动定位仓库根）
#
# 临时文件说明：使用仓库内相对临时文件（.kinds-consistency.$$），
# 不用 /tmp —— BusyBox/PRoot 沙箱下 /tmp 可能不可读写。trap 保证清理。
set -u

# 定位仓库根（本脚本位于 <root>/scripts/）
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" 2>/dev/null && pwd) || script_dir=""
if [ -n "$script_dir" ] && [ -f "$script_dir/../server/internal/proto/kinds/kinds.go" ]; then
  cd "$script_dir/.." || exit 1
fi

GO=server/internal/proto/kinds/kinds.go
TS=apps/client/src/net/protocol/kinds.ts
TMP=".kinds-consistency.$$"
tmp_go="$TMP.go"
tmp_ts="$TMP.ts"
tmp_bad="$TMP.bad"
cleanup() { rm -f "$tmp_go" "$tmp_ts" "$tmp_bad"; }
trap cleanup EXIT INT TERM

[ -f "$GO" ] || { echo "kinds-consistency: missing $GO" >&2; exit 1; }
[ -f "$TS" ] || { echo "kinds-consistency: missing $TS" >&2; exit 1; }
fail=0

# 从 kinds.go 提取 数值 → NAME（register(KindX, "NAME") 需要先解常量值）
go_map() {
  awk '
    /^[[:space:]]*Kind[A-Za-z0-9_]+[[:space:]]+Kind[[:space:]]*=[[:space:]]*[0-9]+/ {
      name=$1; val=$4; gsub(/^Kind/, "", name);
      const[name]=val
    }
    /register\(Kind[A-Za-z0-9_]+, "[A-Z0-9_]+"\)/ {
      n=$0; sub(/.*register\(Kind/, "", n); sub(/, ".*/, "", n);
      s=$0; sub(/.*, "/, "", s); sub(/"\).*/, "", s);
      if (const[n] != "") print const[n], s
    }' "$GO" | sort -n -u
}

# 从 kinds.ts 提取 NAME: 数值（每行可有多个键值对）
ts_map() {
  grep -oE '[A-Z0-9_]+:[[:space:]]*[0-9]+' "$TS" \
    | sed -E 's/([A-Z0-9_]+):[[:space:]]*([0-9]+)/\2 \1/' | sort -n -u
}

go_map > "$tmp_go"
ts_map > "$tmp_ts"

# TS 独有常量（Go 未注册）→ 报错；Go 独有（如 40x 内部帧）忽略（前端无对应 handler 属正常）。
ts_only=$(comm -13 "$tmp_go" "$tmp_ts")
go_only=$(comm -23 "$tmp_go" "$tmp_ts")

echo "== kinds.ts entries not in kinds.go =="
# 401/402 在 kinds.go 有意保留双名注册（PRESENCE_ONLINE + SUBSCRIBE 别名，
# 见 kinds.go 注释），TS 只用别名——不视为不一致。
printf '%s\n' "$ts_only" | grep -v -E '^40[12] ' | grep -v '^[[:space:]]*$' > "$tmp_bad"
if [ -s "$tmp_bad" ]; then
  sed 's/^/  /' "$tmp_bad"; fail=1
else
  echo "  (none, ignoring documented aliases)"
fi

# 同号异名（T-CL-3 核心）：value 相同但 name 不一致
dup_val_go=$(awk 'seen[$1]++ {print $1}' "$tmp_go")
if [ -n "$dup_val_go" ]; then
  echo "== duplicate values in kinds.go =="; echo "$dup_val_go" | sed 's/^/  /'; fail=1
fi

# 15/16/17 前置锁定：v3 号段必须两端存在且一致（v3-bus-design §4.1）
for kv in "15 MAILBOX_ACK" "16 MAILBOX_PAUSE" "17 MAILBOX_RESUME"; do
  v=${kv%% *}; n=${kv##* }
  go_has=$(awk -v v="$v" -v n="$n" '$1==v && $2==n' "$tmp_go" | wc -l)
  ts_has=$(awk -v v="$v" -v n="$n" '$1==v && $2==n' "$tmp_ts" | wc -l)
  if [ "$go_has" -ne 1 ] || [ "$ts_has" -ne 1 ]; then
    echo "== v3 kinds missing/mismatched: $v $n (go=$go_has ts=$ts_has) =="
    fail=1
  fi
done

[ "$fail" -eq 0 ] && echo "kinds consistency: OK" || exit 1
