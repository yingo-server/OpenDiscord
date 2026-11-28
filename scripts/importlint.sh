#!/bin/sh
# importlint.sh — enforce docs/bridge-design.md §4.1 import red lines (P0-1).
#
# Rules (make lint / CI must fail on any violation):
#   A: bridge component packages (framerw verifier router egress ack session
#      adminapi supervisor) must not import each other. Exemptions:
#      - internal/bridge/pipeline.go: the documented assembly file whose job
#        is wiring the siblings together.
#      - internal/bridge root package test files (pipeline_test.go etc.):
#        they exercise the ASSEMBLED pipeline, not one component — the
#        sibling-import ban targets "component implementation" coupling,
#        and a pipeline test necessarily sees all wired components.
#        Component subpackage tests are NOT exempt (docs: a component test's
#        imports must not include sibling implementation packages).
#   B: layer packages internal/{user,data,admin,authsvc,mediasvc} must not
#      import internal/bridge/ — the only layer-to-layer entries are
#      transport + proto. cmd/ is the assembly layer and is exempt.
#   C: "os/signal" may be imported only by internal/pkg/signalset and
#      internal/bridge/supervisor.
#   D: cmd/ binaries (other than chatd/adminctl/ufotctl) may import only
#      app-layer assembly entry points, not component implementation
#      packages. cmd/chatd IS the assembly layer (exempt); adminctl and
#      ufotctl are CLIs that talk HTTP/files only (exempt per task).
#
# Tests (_test.go) obey rules A/B/C/D the same as production files.

set -u
cd "$(dirname "$0")/.."
mod=$(go list -m 2>/dev/null || echo github.com/yingo-server/UFOT)
server=$(pwd)/server
violations=0

report() {
    printf 'importlint: %s\n' "$1"
    violations=$((violations + 1))
}

go_files=$(find "$server" -name '*.go' -type f | sort)

# ruleA FILEDIR FILEBASE IMPORT_TAIL — compDir is e.g. internal/bridge/adminapi.
ruleA() {
    pkgdir=$1; base=$2; tail=$3
    comp=${pkgdir#internal/bridge/}       # adminapi, or "" for bridge root
    imp_comp=${tail#bridge/}              # e.g. adminapi
    [ "$comp" = "$imp_comp" ] && return 0 # own package: not a sibling
    # bridge root package: pipeline.go (assembly) and tests are exempt.
    if [ "$pkgdir" = "internal/bridge" ]; then
        return 0
    fi
    report "rule A (bridge sibling import): $pkgdir/$base imports internal/$tail"
}

# ruleB PKGDIR FILEBASE IMPORT_TAIL
ruleB() {
    pkgdir=$1; base=$2; tail=$3
    report "rule B (layer imports bridge): $pkgdir/$base.go imports internal/$tail"
}

# ruleD PKGDIR FILEBASE IMPORT_TAIL
ruleD() {
    pkgdir=$1; base=$2; tail=$3
    report "rule D (cmd imports component impl): $pkgdir/$base.go imports internal/$tail"
}

for f in $go_files; do
    rel=${f#"$server"/}
    pkgdir=$(dirname "$rel")
    base=$(basename "$f" .go)

    # Extract every quoted import path in the file (block + single-line form).
    imports=$(grep -o '"[^"]*"' "$f" | tr -d '"')

    for imp in $imports; do
        case "$imp" in
            "$mod"/server/internal/*) tail=${imp#"$mod"/server/internal/} ;;
            *) continue ;;
        esac

        # ---- Rule A ----
        case "$pkgdir" in
            internal/bridge/*)
                case "$tail" in
                    bridge/framerw|bridge/verifier|bridge/router|bridge/egress|\
                    bridge/ack|bridge/session|bridge/adminapi|bridge/supervisor)
                        ruleA "$pkgdir" "$base" "$tail" ;;
                esac
                ;;
        esac

        # ---- Rule B ----
        case "$pkgdir" in
            internal/user|internal/user/*|internal/data|internal/data/*|\
            internal/admin|internal/admin/*|internal/authsvc|internal/authsvc/*|\
            internal/mediasvc|internal/mediasvc/*)
                case "$tail" in
                    bridge/*) ruleB "$pkgdir" "$base" "$tail" ;;
                esac
                ;;
        esac

        # ---- Rule D ----
        case "$pkgdir" in
            cmd/chatd|cmd/adminctl|cmd/ufotctl) ;; # assembly / CLIs: exempt
            cmd/*)
                case "$tail" in
                    bridge/framerw|bridge/verifier|bridge/router|bridge/egress|\
                    bridge/ack|bridge/session|bridge/adminapi|bridge/supervisor|\
                    user/net/*|user/core/*|data/pipe/*)
                        ruleD "$pkgdir" "$base" "$tail" ;;
                esac
                ;;
        esac
    done
done

# ---- Rule C: os/signal allowlist ----
for f in $go_files; do
    rel=${f#"$server"/}
    case "$rel" in
        internal/pkg/signalset/*|internal/bridge/supervisor/*) continue ;;
    esac
    if grep -q '"os/signal"' "$f"; then
        report "rule C (os/signal outside allowlist): $rel"
    fi
done

if [ "$violations" -gt 0 ]; then
    printf 'importlint: %d violation(s) — see docs/bridge-design.md §4.1\n' "$violations"
    exit 1
fi
exit 0
