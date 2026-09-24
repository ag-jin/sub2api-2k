#!/bin/zsh
# render-workflow.sh — 把 workflow-state.json 渲染成人读板，并可选注入 AGENTS.md 状态块
#
# 用法:
#   scripts/render-workflow.sh --state <state.json> --out <WORKFLOW.md>
#   scripts/render-workflow.sh --state <state.json> --agents-md <AGENTS.md>   # 注入状态块
#
# 不变量:
#   - 输出恒含全部 7 阶段 × 全部线，未到达显示 pending（不靠人记得写全）
#   - 零线时仍输出阶段图例与空表
#   - 缺依赖 → MISSING_DEPENDENCY_<cmd>，非零退出（绝不 fail-open）
set -u
SCRIPT_DIR=${0:A:h}
LIB="${WORKFLOW_LIB:-$SCRIPT_DIR/lib/workflow_state.rb}"

STATE=""; OUT=""; AGENTS_MD=""; SET_GOAL=0; STATE_DIR=""; BASE=""
while (( $# > 0 )); do
  case "$1" in
    --state)     STATE="$2"; shift 2 ;;
    --out)       OUT="$2"; shift 2 ;;
    --agents-md) AGENTS_MD="$2"; shift 2 ;;
    --set-goal)  SET_GOAL=1; shift ;;
    --base)      BASE="$2"; shift 2 ;;
    *) print -u2 -- "UNKNOWN_ARG\t$1"; exit 2 ;;
  esac
done
[[ -n "$STATE" ]] || { print -u2 -- "MISSING_ARG\t--state"; exit 2 }

command -v ruby >/dev/null 2>&1 || { print -u2 -- "MISSING_DEPENDENCY_ruby"; exit 1 }
[[ -f "$LIB" ]] || { print -u2 -- "MISSING_LIB\t$LIB"; exit 1 }
[[ -f "$STATE" ]] || { print -u2 -- "MISSING_STATE_FILE\t$STATE"; exit 1 }
STATE_DIR="${STATE:A:h}"
# --base 未给时回退 state 所在目录：state 可能在项目侧而证据在仓库内，
# 此时必须由调用者显式指定基准，否则写入门会把合法状态误判为违规
[[ -n "$BASE" ]] || BASE="$STATE_DIR"

# 使用前必校验：渲染/注入是"消费"违规状态的路径（实测对 A1 矛盾状态仍 rc=0 并注入成功）
# 与 --set-goal 同级的校验门；可用 WORKFLOW_SKIP_VALIDATE=1 显式跳过（需自担风险）
if [[ "${WORKFLOW_SKIP_VALIDATE:-0}" != "1" ]]; then
  verr=$(ruby "$LIB" --check "$STATE" "$BASE" 2>&1); vrc=$?
  if (( vrc != 0 )); then
    print -u2 -- "STATE_INVALID_REFUSE_RENDER	校验未通过，拒绝渲染/注入"
    print -u2 -- "$verr" | head -5
    exit 1
  fi
fi

# 自动设置 goal（--set-goal）：从最高优先度 active 线推导，幂等（未变 rc=2 不视为错误）
if (( SET_GOAL )); then
  ruby "$LIB" --set-goal "$STATE" "$BASE"
  rc=$?
  case $rc in
    0) print -- "GOAL	set" ;;
    2) print -- "GOAL	unchanged" ;;
    *) print -u2 -- "GOAL_SET_FAILED	rc=$rc"; exit 1 ;;
  esac
fi

if [[ -n "$OUT" ]]; then
  ruby "$LIB" --render "$STATE" > "$OUT" || exit 1
  print -- "RENDERED\t$OUT\t$(wc -c < "$OUT" | tr -d ' ')字节"
fi

if [[ -n "$AGENTS_MD" ]]; then
  [[ -f "$AGENTS_MD" ]] || { print -u2 -- "MISSING_AGENTS_MD\t$AGENTS_MD"; exit 1 }
  BLOCK_FILE=$(mktemp) || { print -u2 -- "MKTEMP_FAILED"; exit 1 }
  trap 'rm -f "$BLOCK_FILE"' EXIT
  ruby "$LIB" --summary "$STATE" > "$BLOCK_FILE" || exit 1
  # 注入逻辑在库文件里实现（内联 ruby 遇中文会崩，已实测）
  ruby "$LIB" --inject "$AGENTS_MD" "$BLOCK_FILE"
  rc=$?
  case $rc in
    0) print -- "AGENTS_STATE_BLOCK\tupdated" ;;
    2) print -- "AGENTS_STATE_BLOCK\tunchanged" ;;
    *) exit 1 ;;
  esac
fi

[[ -n "$OUT" || -n "$AGENTS_MD" ]] || ruby "$LIB" --render "$STATE"
