#!/usr/bin/env bash
# UFOT deployment scripts — shared output helpers.
# Sourced by install.sh / release.sh / backup.sh. Safe to source twice.
#
# Design:
#   - One TTY decision at source time: colors only when stdout is a terminal
#     and NO_COLOR is unset. CI/journald get plain, greppable lines.
#   - Three output levels (log/warn/die) + optional ok, with stable prefixes.
#   - step N/M gives long install/release flows a visible rhythm.
#   - No Unicode glyphs in plain mode (✓/✗ break log viewers); ASCII in TTY
#     only. Every function writes to the documented stream (warn/die -> stderr).

# Guard: re-sourcing must not double-define or reset an in-progress step count.
if [ -n "${UFOT_OUTPUT_LIB_LOADED:-}" ]; then
  return 0 2>/dev/null || true
fi
UFOT_OUTPUT_LIB_LOADED=1

UFOT_COLOR=0
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  UFOT_COLOR=1
fi

if [ "$UFOT_COLOR" -eq 1 ]; then
  _c_reset=$'\033[0m'; _c_bold=$'\033[1m'
  _c_blue=$'\033[34m'; _c_yellow=$'\033[33m'; _c_red=$'\033[31m'; _c_green=$'\033[32m'
  _p_log="${_c_bold}${_c_blue}==>${_c_reset}"
  _p_warn="${_c_bold}${_c_yellow}[warn]${_c_reset}"
  _p_die="${_c_bold}${_c_red}[error]${_c_reset}"
  _p_ok="${_c_green}ok${_c_reset}"
else
  _c_reset=''; _c_bold=''; _c_blue=''; _c_yellow=''; _c_red=''; _c_green=''
  _p_log='==>'; _p_warn='[warn]'; _p_die='[error]'; _p_ok='ok'
fi

log()  { printf '%s %s\n' "$_p_log" "$*"; }
warn() { printf '%s %s\n' "$_p_warn" "$*" >&2; }
die()  { printf '%s %s\n' "$_p_die" "$*" >&2; exit 1; }
ok()   { printf '%s %s\n' "$_p_ok" "$*"; }

# Optional step rhythm: step "3/9" "Generating keys" or step 3 9 "msg".
step() {
  if [ "$#" -ge 2 ]; then
    case "$2" in
      */*) printf '%s [%s] %s\n' "$_p_log" "$1" "${*:3}" ;;
      *)   printf '%s [%s/%s] %s\n' "$_p_log" "$1" "$2" "${*:3}" ;;
    esac
  else
    printf '%s %s\n' "$_p_log" "$*"
  fi
}
