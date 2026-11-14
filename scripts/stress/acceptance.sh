#!/bin/sh
# acceptance.sh —— OpenDiscord 1.4 LTS 验收编排（S-1~S-7）
#
# 依据：docs/lts-1.4-acceptance.md §C「验收编排（文档 + workflow，不改业务代码）」
#       docs/lts-1.4-plan.md §6 发布门槛 / §9 验收记录表
#       docs/universal-pull-spec.md §4（S-6/S-7 可测试断言）
#
# 职责：
#   1. 依次触发 S-1~S-7 七组 dispatch（GitHub Actions workflow_dispatch，stress.yml 的
#      scenario 输入），等待完成并收集各组的 result JSON；
#   2. 按 §C 的门槛常量逐条判定 PASS / FAIL / BLOCKED；
#   3. 把结论写回 docs/lts-1.4-plan.md §9 验收记录表（含表尾「转生产判定」行）。
#
# 纪律（全局铁律）：
#   - 本脚本不构建、不跑本地压测、不 commit/push、不打 tag；
#   - 门槛数字写死在下面的常量区，与 docs/lts-1.4-acceptance.md §C 逐字一致，不做调整；
#   - 只有 S-1~S-7 全部 PASS 才允许把 4 个 flag 默认值改 true（属最后单独提交）。
#
# 用法：
#   sh scripts/stress/acceptance.sh                       # 真跑：dispatch + 判定 + 写表
#   sh scripts/stress/acceptance.sh --dry-run             # 只打印 dispatch 计划与判定逻辑，不发请求
#   sh scripts/stress/acceptance.sh --from-dir DIR        # 离线判定：DIR/<profile>/ 下放 s1.json … s7.json
#   sh scripts/stress/acceptance.sh --scenarios "s1 s2"   # 只跑指定场景
#   sh scripts/stress/acceptance.sh --profiles "2c2g"     # 只跑指定硬件档（默认 "2c2g 4c16g"，§E）
#
# 退出码：0 = 全部 PASS（或 --dry-run）；1 = 有 FAIL；2 = 有 BLOCKED / 用法错误。
set -eu

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)"
cd "$ROOT"

# ---------------------------------------------------------------------------
# 门槛常量（§C 2；数字不得改动）
# ---------------------------------------------------------------------------
S1_AVAIL_MIN=95            # S-1 500 人群可用性 ≥95%
S1_P95_MAX_MS=2000         # S-1 p95 ≤2s
S1_P95_STRICT_MS=100       # plan §6 更严格口径（p95 ≤100ms）——仅告警，不参与判定
S1_DATA_FRAME_DROPS_MAX=0  # S-1 数据帧丢失 = 0
S1_MIN_CONNS=1000

S2_AVAIL_MIN=90            # S-2 50 人群可用性 ≥90%
S2_MIN_CONNS=1000

S3_AVAIL_MIN=94.5          # S-3 512×10 不劣于 v1.2.1 的 94.5%
S3_MIN_CONNS=512

S4_OTHERS_AVAIL_MIN=99     # S-4 慢消费者之外的连接可用性 ≥99%
S4_DATA_FRAME_DROPS_MAX=0  # S-4 数据帧丢失 = 0
S4_SLOW_INJECTED=50        # S-4 50/1000 慢连接

S5_RESUME_QPS_MAX=800      # S-5 重启风暴续传 QPS ≤800
S5_5XX_MAX=0               # S-5 无 5xx

S6_JITTER_MAX_RATIO=2      # S-6 自适应抖动 ≤2×

S7_BYTE_IDENTICAL=1        # S-7 flag 回滚逐字节一致

export S1_AVAIL_MIN S1_P95_MAX_MS S1_P95_STRICT_MS S1_DATA_FRAME_DROPS_MAX S1_MIN_CONNS
export S2_AVAIL_MIN S2_MIN_CONNS S3_AVAIL_MIN S3_MIN_CONNS
export S4_OTHERS_AVAIL_MIN S4_DATA_FRAME_DROPS_MAX S4_SLOW_INJECTED
export S5_RESUME_QPS_MAX S5_5XX_MAX S6_JITTER_MAX_RATIO S7_BYTE_IDENTICAL

# ---------------------------------------------------------------------------
# §E 双规格：hw_profile 档位（门槛常量与档位无关，逐字一致）
#   2c2g：workflow 侧用 cgroup v2 / systemd-run / ulimit -v + taskset + GOMAXPROCS=2
#         约束 chatd 与 stressd；资源约束全部不可用时标 BLOCKED，不假跑。
#   4c16g：默认 CI runner 资源，不加约束。
# ---------------------------------------------------------------------------
HW_PROFILES="2c2g 4c16g"

hw_gate_note() {
  case "$1" in
    2c2g)   printf '2核2g 档（cgroup memory.max=2G + cpu.max 2核 + GOMAXPROCS=2 + taskset）' ;;
    4c16g)  printf '4核16g 档（默认 CI runner）' ;;
    *)      printf '-' ;;
  esac
}

# ---------------------------------------------------------------------------
# 场景定义：scenario id → (门槛描述, dispatch 用的 workflow 输入)
# ---------------------------------------------------------------------------
SCENARIOS="s1 s2 s3 s4 s5 s6 s7"

scenario_gate() {
  case "$1" in
    s1) printf '可用性 ≥95%%、p95 ≤2s、数据帧丢失 = 0' ;;
    s2) printf '可用性 ≥90%%' ;;
    s3) printf '可用性 ≥94.5%%（不劣于 v1.2.1）' ;;
    s4) printf '其余连接可用性 ≥99%%、数据帧丢失 = 0' ;;
    s5) printf '续传 QPS ≤800、无 5xx' ;;
    s6) printf '自适应抖动 ≤2×' ;;
    s7) printf 'flag 回滚逐字节一致' ;;
    *)  printf '-' ;;
  esac
}

scenario_inputs() {
  # 每组 dispatch 传给 stress.yml 的输入（-f k=v）。
  case "$1" in
    s1) printf 'scenario=s1|conv_members=500|universal_pull=true|ladder=start=1000,max=1000' ;;
    s2) printf 'scenario=s2|conv_members=50|universal_pull=true|ladder=start=1000,max=1024' ;;
    s3) printf 'scenario=s3|conv_members=10|universal_pull=true|ladder=start=512,max=512' ;;
    s4) printf 'scenario=s4|conv_members=500|universal_pull=true|ladder=start=1000,max=1000|slow_ratio=0.05|slow_factor=5' ;;
    s5) printf 'scenario=s5|conv_members=10' ;;
    s6) printf 'scenario=s6-static|conv_members=500|universal_pull=true|ladder=start=512,max=512' ;;
    s7) printf 'scenario=s7|conv_members=99' ;;
    *)  printf '' ;;
  esac
}
# ---------------------------------------------------------------------------
# 运行参数
# ---------------------------------------------------------------------------
WORKFLOW="stress.yml"
REPO="${GITHUB_REPOSITORY:-}"
RESULTS_DIR=""
FROM_DIR=""
DRY_RUN=0
ONLY=""
DO_TABLE=1

usage() {
  sed -n '2,26p' "$0" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
  case "$1" in
    --repo) REPO="$2"; shift 2 ;;
    --workflow) WORKFLOW="$2"; shift 2 ;;
    --results-dir) RESULTS_DIR="$2"; shift 2 ;;
    --from-dir) FROM_DIR="$2"; shift 2 ;;
    --profiles) HW_PROFILES="$2"; shift 2 ;;
    --scenarios) ONLY="$2"; shift 2 ;;
    --no-table) DO_TABLE=0; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [ -z "$REPO" ]; then
  REPO="$(git remote get-url origin 2>/dev/null | sed -e 's#^.*github.com[:/]##' -e 's#\.git$##')" || REPO=""
fi
if [ -z "$RESULTS_DIR" ]; then
  RESULTS_DIR="/tmp/acceptance-$(date -u +%Y%m%dT%H%M%SZ)"
fi
mkdir -p "$RESULTS_DIR"

if [ -n "$ONLY" ]; then
  SCENARIOS="$ONLY"
fi

command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 2; }
if [ -z "$FROM_DIR" ] && [ "$DRY_RUN" = "0" ]; then
  command -v gh >/dev/null 2>&1 || { echo "gh CLI is required (or use --from-dir / --dry-run)" >&2; exit 2; }
fi

# ---------------------------------------------------------------------------
# dispatch 计划
# ---------------------------------------------------------------------------
# dispatch_plan <场景> → 每行 "workflow_scenario<TAB>feature_flags"
dispatch_plan() {
  case "$1" in
    s1|s2|s3|s4|s5)
      printf 's%s\tuniversal_pull_enabled=true,adaptive_control_enabled=true,v3_bus_enabled=false\n' "${1#s}" ;;
    s6)
      # S-6 需要 static 与 adaptive 两组同场景运行（spec §4.1）。
      printf 's6-static\tuniversal_pull_enabled=true,adaptive_control_enabled=false,v3_bus_enabled=false\n'
      printf 's6-adaptive\tuniversal_pull_enabled=true,adaptive_control_enabled=true,v3_bus_enabled=false\n' ;;
    s7)
      # S-7：flag 全关（回滚态），由 workflow 用 v1.2.1 tag 基线做逐字节比对。
      printf 's7\tuniversal_pull_enabled=false,adaptive_control_enabled=false,v3_bus_enabled=false\n' ;;
    *) : ;;
  esac
}

# dispatch_run <workflow_scenario> <flags> [hw_profile] → 打印 run id（失败返回非 0）
dispatch_run() {
  wfsc="$1"
  flags="$2"
  hw="${3:-4c16g}"
  since="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  gh workflow run "$WORKFLOW" --repo "$REPO" \
    -f "scenario=$wfsc" -f "feature_flags=$flags" -f "duration=30" \
    -f "no_ratelimit=true" -f "hw_profile=$hw" >/dev/null
  # 找到刚刚创建的那次 run（按创建时间取最新一条）。
  i=0
  while [ "$i" -lt 30 ]; do
    id="$(gh run list --repo "$REPO" --workflow "$WORKFLOW" --limit 10 \
          --json databaseId,createdAt \
          --jq "map(select(.createdAt >= \"$since\")) | sort_by(.createdAt) | last | .databaseId" 2>/dev/null || true)"
    if [ -n "$id" ] && [ "$id" != "null" ]; then
      printf '%s\n' "$id"
      return 0
    fi
    i=$((i + 1))
    sleep 4
  done
  return 1
}

# wait_run <run_id> → 打印 conclusion
wait_run() {
  id="$1"
  i=0
  while [ "$i" -lt 180 ]; do
    st="$(gh run view "$id" --repo "$REPO" --json status --jq .status 2>/dev/null || echo unknown)"
    if [ "$st" = "completed" ]; then
      gh run view "$id" --repo "$REPO" --json conclusion --jq .conclusion 2>/dev/null || echo unknown
      return 0
    fi
    i=$((i + 1))
    sleep 20
  done
  echo timeout
  return 1
}

# collect_run <run_id> <目标目录>
collect_run() {
  id="$1"
  dir="$2"
  mkdir -p "$dir"
  gh run download "$id" --repo "$REPO" --name "ladder-result-$id" --dir "$dir" >/dev/null 2>&1 || return 1
  [ -f "$dir/ladder.json" ] || return 1
  return 0
}
# ---------------------------------------------------------------------------
# 门槛判定（单一实现：内联 python3，阈值全部来自上面的常量区）
# ---------------------------------------------------------------------------
eval_scenario() {
  python3 - "$1" "$2" "$3" "$4" <<'PY'
import json, os, sys

scen, primary, secondary, cmpfile = sys.argv[1:5]

def env_f(name, default=0.0):
    try:
        return float(os.environ[name])
    except Exception:
        return float(default)

def env_i(name, default=0):
    try:
        return int(float(os.environ[name]))
    except Exception:
        return int(default)

def load(path):
    if not path:
        return None
    try:
        with open(path) as f:
            return json.load(f)
    except Exception:
        return None

def last(doc, key):
    rows = (doc or {}).get(key) or []
    return rows[-1] if rows else None

checks = []
notes = []

def need(cond, name, detail=""):
    checks.append((name, bool(cond), detail))
    return bool(cond)

def blocked(reason):
    print("VERDICT=BLOCKED")
    print("SUMMARY=" + reason)
    print("EVIDENCE=" + json.dumps({"reason": reason}, ensure_ascii=False))
    sys.exit(0)

doc = load(primary)
if doc is None:
    blocked("结果 JSON 缺失或无法解析: %s" % primary)

lv = last(doc, "levels")
pl = last(doc, "pull_levels")

def flv(key, default=0):
    if isinstance(lv, dict) and key in lv:
        try:
            return float(lv[key])
        except Exception:
            return float(default)
    return float(default)

def fpl(key, default=0):
    if isinstance(pl, dict) and key in pl:
        try:
            return float(pl[key])
        except Exception:
            return float(default)
    return float(default)

if scen == "s1":
    if lv is None:
        blocked("S-1: 结果 JSON 无 levels 记录")
    avail = flv("availability_pct"); p95 = flv("p95_ms")
    drop = flv("server_drop"); conns = flv("conns")
    need(conns >= env_i("S1_MIN_CONNS"), "在线连接数 >= %d" % env_i("S1_MIN_CONNS"), "%d" % conns)
    need(avail >= env_f("S1_AVAIL_MIN"), "可用性 >= %g%%" % env_f("S1_AVAIL_MIN"), "%.2f%%" % avail)
    need(p95 <= env_f("S1_P95_MAX_MS"), "p95 <= %gms" % env_f("S1_P95_MAX_MS"), "%.1fms" % p95)
    need(drop <= env_f("S1_DATA_FRAME_DROPS_MAX"), "数据帧丢失 = 0", "%d" % drop)
    need(pl is not None and bool(pl.get("universal_pull")), "普遍拉取模式已启用（帧形态正确）")
    if p95 > env_f("S1_P95_STRICT_MS"):
        notes.append("WARN: p95 %.1fms 高于 plan 6 节的 %gms（门槛常量以验收书为准，仅告警）" % (p95, env_f("S1_P95_STRICT_MS")))

elif scen == "s2":
    if lv is None:
        blocked("S-2: 结果 JSON 无 levels 记录")
    avail = flv("availability_pct"); conns = flv("conns")
    need(conns >= env_i("S2_MIN_CONNS"), "在线连接数 >= %d" % env_i("S2_MIN_CONNS"), "%d" % conns)
    need(avail >= env_f("S2_AVAIL_MIN"), "可用性 >= %g%%" % env_f("S2_AVAIL_MIN"), "%.2f%%" % avail)

elif scen == "s3":
    if lv is None:
        blocked("S-3: 结果 JSON 无 levels 记录")
    avail = flv("availability_pct"); conns = flv("conns")
    need(conns >= env_i("S3_MIN_CONNS"), "在线连接数 >= %d" % env_i("S3_MIN_CONNS"), "%d" % conns)
    need(avail >= env_f("S3_AVAIL_MIN"), "可用性 >= %g%%（不劣于 v1.2.1）" % env_f("S3_AVAIL_MIN"), "%.2f%%" % avail)

elif scen == "s4":
    if lv is None or pl is None:
        blocked("S-4: 结果 JSON 缺少 levels / pull_levels（慢消费者分组证据缺失）")
    avail = flv("availability_pct"); drop = flv("server_drop")
    slow = fpl("slow_injected"); conv_open = fpl("conv_open")
    need(avail >= env_f("S4_OTHERS_AVAIL_MIN"), "其余连接可用性 >= %g%%" % env_f("S4_OTHERS_AVAIL_MIN"), "%.2f%%" % avail)
    need(drop <= env_f("S4_DATA_FRAME_DROPS_MAX"), "数据帧丢失 = 0", "%d" % drop)
    need(slow == env_f("S4_SLOW_INJECTED"), "慢连接注入数 = %d" % env_f("S4_SLOW_INJECTED"), "%d" % slow)
    need(conv_open == 0, "慢连接重连续传无 gap（未收敛水位线 = 0）", "%d" % conv_open)

elif scen == "s5":
    if "peak_qps" not in doc:
        blocked("S-5: 结果 JSON 不是 restart-storm 形状")
    peak = float(doc.get("peak_qps") or 0)
    errs = int(doc.get("error_frames") or 0)
    req = int(doc.get("requested_conns") or 0)
    rec = int(doc.get("reconnect_ok") or 0)
    need(peak <= env_f("S5_RESUME_QPS_MAX"), "续传峰值 QPS <= %g" % env_f("S5_RESUME_QPS_MAX"), "%.1f" % peak)
    need(errs <= env_i("S5_5XX_MAX"), "无 5xx / 错误帧", "%d" % errs)
    need(req > 0 and rec == req, "全部连接重连成功（cursor 不回退）", "%d/%d" % (rec, req))

elif scen == "s6":
    sdoc = load(secondary)
    if sdoc is None:
        blocked("S-6: 缺少 static 对照组结果 JSON")
    srow = last(sdoc, "pull_levels")
    arow = last(doc, "pull_levels")
    if srow is None or arow is None:
        blocked("S-6: 两侧结果缺少 pull_levels（自适应证据缺失）")
    sp95 = float(srow.get("convergence_p95_ms") or 0)
    ap95 = float(arow.get("convergence_p95_ms") or 0)
    sav = float(srow.get("availability_pct") or 0)
    aav = float(arow.get("availability_pct") or 0)
    if sp95 <= 0:
        blocked("S-6: static 对照组 p95 为 0，无法计算抖动比")
    ratio = ap95 / sp95
    need(ratio <= env_f("S6_JITTER_MAX_RATIO"), "自适应抖动 <= %gx" % env_f("S6_JITTER_MAX_RATIO"), "%.3fx (static %.1fms -> adaptive %.1fms)" % (ratio, sp95, ap95))
    notes.append("可用性 static %.2f%% -> adaptive %.2f%%（spec 4.1 节 S6-3 伴生证据）" % (sav, aav))

elif scen == "s7":
    text = ""
    if cmpfile and os.path.exists(cmpfile):
        try:
            with open(cmpfile, errors="replace") as f:
                text = f.read()
        except Exception:
            text = ""
    if "IDENTICAL" in text:
        need(True, "flag-off 运行与 v1.2.1 基线逐字节一致", "IDENTICAL")
    elif "DIFF" in text:
        need(False, "flag-off 运行与 v1.2.1 基线逐字节一致", "DIFF")
    else:
        blocked("S-7: 缺少 v1.2.1 基线比对证据（BASELINE_MISSING / 未产出）")
    if pl is not None:
        wm = fpl("watermark_per_sec")
        need(wm == 0, "回滚态不发水位线帧（kind 14 = 0）", "%.2f/s" % wm)

failed = [c for c in checks if not c[1]]
verdict = "FAIL" if failed else "PASS"
summary = "; ".join("%s=%s" % (c[0], c[2]) for c in checks)
if notes:
    summary = summary + " | " + " | ".join(notes)
print("VERDICT=" + verdict)
print("SUMMARY=" + summary)
print("EVIDENCE=" + json.dumps({"checks": [[c[0], c[1], c[2]] for c in checks], "notes": notes}, ensure_ascii=False))
PY
}
# ---------------------------------------------------------------------------
# 写回 docs/lts-1.4-plan.md §9 验收记录表
# ---------------------------------------------------------------------------
PLAN="$ROOT/docs/lts-1.4-plan.md"

sanitize() {
  printf '%s' "$1" | tr '|' '/' | tr '\n' ' '
}

# update_row <表格键> <hw_profile> <运行记录> <结论>
# §E：结论列按档合并为 "2c2g: …；4c16g: …"，两档互不覆盖；运行记录列同样按档合并。
update_row() {
  key="$1"; hw="$2"; rec="$3"; verdict="$4"
  [ "$DO_TABLE" = "1" ] || return 0
  [ -f "$PLAN" ] || { echo "plan doc not found: $PLAN" >&2; return 1; }
  tmp="$RESULTS_DIR/plan.tmp"
  awk -v key="$key" -v hw="$hw" -v rec="$rec" -v verdict="$verdict" '
    BEGIN { FS = "|" }
    function merge(old, v,   parts, i, n, out, found, h) {
      if (old == "") return hw ": " v
      n = split(old, parts, "；")
      out = ""; found = 0
      for (i = 1; i <= n; i++) {
        h = parts[i]
        if (substr(h, 1, length(hw) + 2) == hw ": ") {
          out = out (out == "" ? "" : "；") hw ": " v; found = 1
        } else if (h != "") {
          out = out (out == "" ? "" : "；") h
        }
      }
      if (!found) out = out "；" hw ": " v
      return out
    }
    function mergeRec(old, v,   parts, i, n, out, found, h) {
      if (old == "") return hw " [" v "]"
      n = split(old, parts, " | ")
      out = ""; found = 0
      for (i = 1; i <= n; i++) {
        h = parts[i]
        if (substr(h, 1, length(hw) + 2) == hw " [") {
          out = out (out == "" ? "" : " | ") hw " [" v "]"; found = 1
        } else if (h != "") {
          out = out (out == "" ? "" : " | ") h
        }
      }
      if (!found) out = out " | " hw " [" v "]"
      return out
    }
    {
      if (index($0, key) > 0 && substr($0, 1, 1) == "|" && NF >= 6) {
        if (index($0, "转生产判定") > 0) {
          printf "|%s|%s|%s|%s|%s|\n", $2, $3, $4, rec, verdict
        } else {
          printf "|%s|%s|%s|%s|%s|\n", $2, $3, $4, mergeRec($5, rec), merge($6, verdict)
        }
        found = 1
        next
      }
      print
    }
    END { if (!found) exit 3 }
  ' "$PLAN" > "$tmp" || { echo "row not found in §9 table: $key" >&2; return 1; }
  mv "$tmp" "$PLAN"
  return 0
}

# ---------------------------------------------------------------------------
# 主流程（§E：外层按 hw_profile 2c2g / 4c16g 两档，内层 S-1~S-7）
# ---------------------------------------------------------------------------
PASS_N=0
FAIL_N=0
BLOCK_N=0
RUNIDS=""
PROFILE_VERDICTS=""   # 逐档汇总，写「转生产判定」行

echo "== OpenDiscord 1.4 验收编排 (§E 双规格：$HW_PROFILES × S-1~S-7) =="
echo "repo=$REPO workflow=$WORKFLOW results=$RESULTS_DIR"

for HW in $HW_PROFILES; do
  HW_PASS=0; HW_FAIL=0; HW_BLOCK=0; HW_RUNIDS=""
  echo
  echo "==== hw_profile=$HW（$(hw_gate_note "$HW")） ===="

  for sc in $SCENARIOS; do
    primary="$RESULTS_DIR/$HW/$sc/ladder.json"
    secondary=""
    cmpf=""
    case "$sc" in
      s6)
        primary="$RESULTS_DIR/$HW/s6-adaptive/ladder.json"
        secondary="$RESULTS_DIR/$HW/s6-static/ladder.json" ;;
      s7) cmpf="$RESULTS_DIR/$HW/s7/stress.log" ;;
    esac

    if [ -n "$FROM_DIR" ]; then
      primary="$FROM_DIR/$HW/$sc.json"
      case "$sc" in
        s6)
          primary="$FROM_DIR/$HW/s6-adaptive.json"
          secondary="$FROM_DIR/$HW/s6-static.json" ;;
        s7) cmpf="$FROM_DIR/$HW/s7-cmp.txt" ;;
      esac
    elif [ "$DRY_RUN" = "1" ]; then
      echo "-- DRY-RUN [$HW] $sc（不发起 dispatch）"
      dispatch_plan "$sc" > "$RESULTS_DIR/plan-$HW-$sc.txt"
      while IFS="$(printf '\t')" read -r wfsc flags; do
        [ -n "$wfsc" ] || continue
        echo "   gh workflow run $WORKFLOW --repo $REPO -f scenario=$wfsc -f feature_flags=$flags -f duration=30 -f no_ratelimit=true -f hw_profile=$HW"
      done < "$RESULTS_DIR/plan-$HW-$sc.txt"
      echo "   门槛：$(scenario_gate "$sc")"
      continue
    else
      dispatch_plan "$sc" > "$RESULTS_DIR/plan-$HW-$sc.txt"
      while IFS="$(printf '\t')" read -r wfsc flags; do
        [ -n "$wfsc" ] || continue
        echo "-- [$HW] $sc: dispatch $wfsc (flags=$flags)"
        if ! id="$(dispatch_run "$wfsc" "$flags" "$HW")"; then
          echo "   dispatch 失败：$wfsc" >&2
          continue
        fi
        echo "   run_id=$id，等待完成…"
        wait_run "$id" > "$RESULTS_DIR/run-$id.conclusion" 2>/dev/null || true
        HW_RUNIDS="$HW_RUNIDS $id"
        collect_run "$id" "$RESULTS_DIR/$HW/$wfsc" || echo "   收集 artifact 失败：$id" >&2
      done < "$RESULTS_DIR/plan-$HW-$sc.txt"
      case "$sc" in
        s6)
          primary="$RESULTS_DIR/$HW/s6-adaptive/ladder.json"
          secondary="$RESULTS_DIR/$HW/s6-static/ladder.json" ;;
        s7) cmpf="$RESULTS_DIR/$HW/s7/stress.log" ;;
      esac
    fi

    out="$(eval_scenario "$sc" "$primary" "$secondary" "$cmpf" 2>&1)" || out=""
    if ! printf '%s\n' "$out" | grep -q '^VERDICT='; then
      out="VERDICT=BLOCKED
SUMMARY=判定脚本执行异常：$(printf '%s' "$out" | tr '\n' ' ')
EVIDENCE={}"
    fi
    verdict="$(printf '%s\n' "$out" | sed -n 's/^VERDICT=//p')"
    summary="$(printf '%s\n' "$out" | sed -n 's/^SUMMARY=//p')"
    evidence="$(printf '%s\n' "$out" | sed -n 's/^EVIDENCE=//p')"
    printf '%s\n' "$evidence" > "$RESULTS_DIR/$HW/$sc.evidence.json" 2>/dev/null || {
      mkdir -p "$RESULTS_DIR/$HW"; printf '%s\n' "$evidence" > "$RESULTS_DIR/$HW/$sc.evidence.json"; }
    printf 'VERDICT=%s\nSUMMARY=%s\n' "$verdict" "$summary" > "$RESULTS_DIR/$HW/$sc.verdict.txt"

    case "$verdict" in
      PASS) PASS_N=$((PASS_N + 1)); HW_PASS=$((HW_PASS + 1)) ;;
      FAIL) FAIL_N=$((FAIL_N + 1)); HW_FAIL=$((HW_FAIL + 1)) ;;
      *)    BLOCK_N=$((BLOCK_N + 1)); HW_BLOCK=$((HW_BLOCK + 1)) ;;
    esac

    echo "== [$HW] $sc  $(scenario_gate "$sc")"
    echo "   判定：$verdict"
    echo "   证据：$summary"

    update_row "| S-$(printf '%s' "$sc" | sed 's/^s//') |" "$HW" \
      "$(sanitize "$(printf 'run_id:%s artifact:%s' "$(echo $HW_RUNIDS | tr ' ' ',')" "$RESULTS_DIR/$HW/$sc")")" \
      "$(sanitize "$verdict：$summary")" || true
  done

  if [ "$HW_FAIL" -eq 0 ] && [ "$HW_BLOCK" -eq 0 ] && [ "$HW_PASS" -gt 0 ]; then
    HW_OVERALL="PASS"
  elif [ "$HW_FAIL" -gt 0 ]; then
    HW_OVERALL="FAIL($HW_FAIL)"
  else
    HW_OVERALL="BLOCKED($HW_BLOCK)"
  fi
  PROFILE_VERDICTS="$PROFILE_VERDICTS $HW=$HW_OVERALL"
  RUNIDS="$RUNIDS $HW_RUNIDS"
done

# ---- 总体判定 + 转生产判定行（§E：两档全部 PASS 才允许改 flag 默认值）----
if printf '%s' "$PROFILE_VERDICTS" | grep -qv 'PASS'; then
  if printf '%s' "$PROFILE_VERDICTS" | grep -q 'FAIL'; then
    overall="FAIL：$PROFILE_VERDICTS —— 有未达门槛项，保持中间态（flag 默认值不变），开 issue 后重跑"
  else
    overall="BLOCKED：$PROFILE_VERDICTS —— 有证据缺失，保持中间态（flag 默认值不变），补齐证据后重跑"
  fi
else
  overall="PASS：$PROFILE_VERDICTS —— 两档 S-1~S-7 全部达标，允许把 4 个 flag 默认值改 true（universal_pull_enabled / adaptive_control_enabled / v3_bus_enabled / v3_bus_percent），改 flag 属最后单独提交"
fi

update_row "转生产判定" "$HW_PROFILES" \
  "$(sanitize "acceptance.sh：hw_profile=$HW_PROFILES；run_id=$(echo $RUNIDS | tr ' ' ',')；证据目录 $RESULTS_DIR")" \
  "$(sanitize "$overall")" || true

echo
echo "== 汇总：PASS=$PASS_N FAIL=$FAIL_N BLOCKED=$BLOCK_N（$PROFILE_VERDICTS）"
echo "== $overall"
echo "== 结果目录：$RESULTS_DIR"

if [ "$FAIL_N" -gt 0 ]; then
  exit 1
fi
if [ "$BLOCK_N" -gt 0 ]; then
  exit 2
fi
exit 0
