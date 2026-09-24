#!/bin/zsh
# check-all-workflow.sh — 开发工作流的统一检查入口
#
# 目的：把「完整检查」变成默认动作，而不是散落的可选项。
# 依据：scope-check 曾只被验收套件调用、无任何门禁使用 —— 机制存在但不生效。
#
# 用法:
#   scripts/check-all-workflow.sh --state <state.json> --base <repo-root> \
#       [--git-base <rev>] [--inventory <list.txt>] [--design/--code/--platform ...]
#
# 依次执行：
#   1. 结构校验（--check）                  失败 → rc=1
#   2. 写入范围检查（--scope-check）        失败 → rc=1
#   3. 覆盖清单（--coverage-check）         失败 → rc=1
#   4. 漂移探针（--design/--code）          无对照物 → SKIP，不改变退出码
#
# 依据：scope-check 与 coverage-check 都曾"只被验收套件调用、无门禁使用"
#       —— 机制存在但不生效。此处集中接入，并由断言防止今后新增检查项漏接。
set -u
SCRIPT_DIR=${0:A:h}
LIB="$SCRIPT_DIR/lib/workflow_state.rb"

STATE=""; BASE=""; GIT_BASE=""; DESIGN=""; CODE=""; PLATFORM=""; INVENTORY=""
while (( $# > 0 )); do
  case "$1" in
    --state)     STATE="$2"; shift 2 ;;
    --base)      BASE="$2"; shift 2 ;;
    --git-base)  GIT_BASE="$2"; shift 2 ;;
    --design)    DESIGN="$2"; shift 2 ;;
    --code)      CODE="$2"; shift 2 ;;
    --platform)  PLATFORM="$2"; shift 2 ;;
    --inventory) INVENTORY="$2"; shift 2 ;;
    *) print -u2 -- "UNKNOWN_ARG\t$1"; exit 2 ;;
  esac
done
[[ -n "$STATE" ]] || { print -u2 -- "MISSING_ARG\t--state"; exit 2 }
command -v ruby >/dev/null 2>&1 || { print -u2 -- "MISSING_DEPENDENCY_ruby"; exit 1 }
[[ -f "$LIB" ]] || { print -u2 -- "MISSING_LIB\t$LIB"; exit 1 }
[[ -f "$STATE" ]] || { print -u2 -- "MISSING_STATE_FILE\t$STATE"; exit 1 }
[[ -n "$BASE" ]] || BASE="${STATE:A:h}"

rc_total=0
n_pass=0; n_fail=0; n_skip=0

print -- "== 1/4 结构校验 =="
out=$(ruby "$LIB" --check "$STATE" "$BASE" 2>&1); rc=$?
if (( rc == 0 )); then print -- "  PASS\t$out"; n_pass=$((n_pass+1))
else print -- "  FAIL\t结构校验未通过"; print -- "$out" | head -8 | sed 's/^/    /'; n_fail=$((n_fail+1)); rc_total=1; fi

print -- "== 2/4 写入范围检查 =="
if [[ -n "$GIT_BASE" ]]; then
  out=$(ruby "$LIB" --scope-check "$STATE" "$GIT_BASE" "$BASE" 2>&1); rc=$?
  if (( rc == 0 )); then print -- "  PASS\t$(print -- "$out" | head -1)"; n_pass=$((n_pass+1))
  else print -- "  FAIL\t范围越界"; print -- "$out" | head -8 | sed 's/^/    /'; n_fail=$((n_fail+1)); rc_total=1; fi
else
  print -- "  SKIP\t未提供 --git-base，跳过范围检查"; n_skip=$((n_skip+1))
fi

print -- "== 3/4 覆盖清单零静默遗漏 =="
if [[ -n "$INVENTORY" ]]; then
  out=$(ruby "$LIB" --coverage-check "$STATE" "$INVENTORY" 2>&1); rc=$?
  if (( rc == 0 )); then print -- "  PASS\t$(print -- "$out" | head -1)"; n_pass=$((n_pass+1))
  else print -- "  FAIL\t覆盖清单未逐项处置"; print -- "$out" | head -8 | sed 's/^/    /'; n_fail=$((n_fail+1)); rc_total=1; fi
else
  print -- "  SKIP\t未提供 --inventory，跳过覆盖检查"; n_skip=$((n_skip+1))
fi

print -- "== 4/4 漂移探针 =="
if [[ -n "$DESIGN" && -n "$CODE" && -n "$PLATFORM" ]]; then
  out=$("$SCRIPT_DIR/drift-probe.sh" --design "$DESIGN" --code "$CODE" --platform "$PLATFORM" 2>&1); rc=$?
  if (( rc == 0 )); then print -- "  PASS\t漂移探针已执行（人工复核命中项）"; n_pass=$((n_pass+1))
  else print -- "  FAIL\t漂移探针执行失败"; print -- "$out" | head -5 | sed 's/^/    /'; n_fail=$((n_fail+1)); rc_total=1; fi
else
  print -- "  SKIP\t未提供 --design/--code/--platform，跳过漂移探针"; n_skip=$((n_skip+1))
fi

print -- ""
print -- "CHECK_ALL\tPASS=$n_pass FAIL=$n_fail SKIP=$n_skip"
exit $rc_total
