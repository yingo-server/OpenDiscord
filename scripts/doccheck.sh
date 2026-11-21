#!/usr/bin/env bash
# doccheck.sh — verifies docs/{api-reference,cli-reference,ws-protocol}.md against source.
# Fails (exit 1) if any documented endpoint / subcommand / kind cannot be found in code,
# or if the "⚠️ 未实现" coverage is below the known gap count.
#
# Usage: bash scripts/doccheck.sh   (run from repo root)
set -u
cd "$(dirname "$0")/.."

API=docs/api-reference.md
CLI=docs/cli-reference.md
WS=docs/ws-protocol.md

fail=0
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# ---------------------------------------------------------------- routes
{
  # admin REST (authoritative Routes() table)
  grep -oE 'Method: http\.Method[A-Za-z]+, Pattern: "[^"]+"' server/internal/admin/handler/api.go \
    | sed -E 's/Method: http\.Method([A-Za-z]+), Pattern: "(.*)"/\1 \2/' \
    | awk '{m=toupper($1); $1=""; sub(/^ /,""); print m" "$0}'
  # user auth
  grep -oE '"POST /api/auth/[^"]+"' server/internal/authsvc/authsvc.go | tr -d '"' | grep -v '\.\.\.'
  # media
  grep -oE '"(GET|POST) /api/media/[^"]+"' server/internal/mediasvc/mediasvc.go | tr -d '"'
  # hand-registered / upgraded
  echo "GET /healthz"
  echo "GET /ws"
  echo "POST /admin/inject"
} | sort -u > "$tmp/routes.txt"

# documented endpoints: table rows whose 1st cell is an HTTP method
awk -F'|' '/^\|[[:space:]]*(GET|POST|PUT|DELETE|PATCH)[[:space:]]*\|/{
  m=$2; p=$3; gsub(/^[[:space:]]+|[[:space:]]+$/,"",m); gsub(/^[[:space:]]+|[[:space:]]+$/,"",p);
  print m" "p
}' "$API" | sort -u > "$tmp/doceps.txt"

n_ep=$(wc -l < "$tmp/doceps.txt")
ep_missing=$(comm -23 "$tmp/doceps.txt" "$tmp/routes.txt")
echo "== [1] API endpoints =="
echo "   documented: $n_ep, matched: $((n_ep - $(printf '%s' "$ep_missing" | grep -c . )))"
if [ -n "$ep_missing" ]; then
  echo "   MISSING from source:"; printf '     - %s\n' $ep_missing; fail=1
fi

# ---------------------------------------------------------------- CLI subcommands
{ grep -oE 'case "[a-z]+"' server/cmd/adminctl/main.go
  grep -oE 'case "[a-z]+"' server/cmd/ufotctl/main.go
} | sed -E 's/case "([a-z]+)"/\1/' | sort -u > "$tmp/cmds.txt"

awk '/^#### /{print $2}' "$CLI" | sort -u > "$tmp/doccmds.txt"
cli_missing=$(comm -23 "$tmp/doccmds.txt" "$tmp/cmds.txt")
n_cli=$(wc -l < "$tmp/doccmds.txt")
echo "== [2] CLI subcommands =="
echo "   documented: $n_cli, matched: $((n_cli - $(printf '%s' "$cli_missing" | grep -c .)))"
if [ -n "$cli_missing" ]; then
  echo "   MISSING from source case list:"; printf '     - %s\n' $cli_missing; fail=1
fi

# ---------------------------------------------------------------- kinds
# documented kind rows: | NUM | NAME | ...
awk -F'|' '/^\|[[:space:]]*[0-9]+[[:space:]]*\|[[:space:]]*[A-Z][A-Z0-9_]*[[:space:]]*\|/{
  n=$2; nm=$3; gsub(/[[:space:]]/,"",n); gsub(/[[:space:]]/,"",nm); print n" "nm
}' "$WS" | sort -u > "$tmp/kinds.txt"

k_bad=""
while read -r num name; do
  [ -z "$num" ] && continue
  # constant value present (Kind = NUM, guard against prefix like 1 vs 101)
  if ! grep -Eq "=[[:space:]]*$num([^0-9]|$)" server/internal/proto/kinds/kinds.go; then
    k_bad="$k_bad\n     - value $num ($name) not a constant in kinds.go"; continue
  fi
  # name registered
  if ! grep -q "\"$name\"" server/internal/proto/kinds/kinds.go; then
    k_bad="$k_bad\n     - name $name not registered in kinds.go"
  fi
done < "$tmp/kinds.txt"
n_k=$(wc -l < "$tmp/kinds.txt")
echo "== [3] WS kinds =="
echo "   documented: $n_k"
if [ -n "$k_bad" ]; then echo "   PROBLEMS:"; printf "%b\n" "$k_bad"; fail=1; fi

# ---------------------------------------------------------------- gap coverage
count=$(grep -oh '⚠️ 未实现' "$API" "$CLI" "$WS" | wc -l)
REQUIRED=20
echo "== [4] '⚠️ 未实现' coverage =="
echo "   occurrences: $count (required >= $REQUIRED)"
if [ "$count" -lt "$REQUIRED" ]; then echo "   TOO FEW gap markers"; fail=1; fi

echo
if [ "$fail" -eq 0 ]; then echo "RESULT: ALL CHECKS PASSED"; else echo "RESULT: FAILED"; fi
exit "$fail"
