#!/bin/zsh
# check-workflow.sh — 开发工作流状态校验（机械拒绝 12 类违规）
#
# 用法:
#   scripts/check-workflow.sh --state <state.json> [--base <dir>]
#
# 退出码: 0=通过  1=违规或依赖缺失  2=参数错误
# 依赖:   zsh, ruby（缺失报 MISSING_DEPENDENCY_*，非零退出，绝不 fail-open）
set -u
SCRIPT_DIR=${0:A:h}
LIB="${WORKFLOW_LIB:-$SCRIPT_DIR/lib/workflow_state.rb}"

STATE=""; BASE=""
while (( $# > 0 )); do
  case "$1" in
    --state) STATE="$2"; shift 2 ;;
    --base)  BASE="$2";  shift 2 ;;
    *) print -u2 -- "UNKNOWN_ARG\t$1"; exit 2 ;;
  esac
done
[[ -n "$STATE" ]] || { print -u2 -- "MISSING_ARG\t--state"; exit 2 }

command -v ruby >/dev/null 2>&1 || { print -u2 -- "MISSING_DEPENDENCY_ruby"; exit 1 }
[[ -f "$LIB" ]] || { print -u2 -- "MISSING_LIB\t$LIB"; exit 1 }
[[ -f "$STATE" ]] || { print -u2 -- "MISSING_STATE_FILE\t$STATE"; exit 1 }

[[ -n "$BASE" ]] || BASE="${STATE:A:h}"
ruby "$LIB" --check "$STATE" "$BASE"
