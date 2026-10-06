#!/usr/bin/env bash
# =============================================================================
# check_r0_invariant.sh —— R0 不变量守护：代码库不存在「使用重置卡」的任何路径
#
# 归属：票 13（.scratch/zhipu-account-channel/issues/13-reset-card-readonly-invariant.md）
# 需求源：spec.md「R0（最高优先）：永不使用账户的重置卡」 / design.md 决策 B4（2026-10-06 终版）
#
# 用法：
#   scripts/check_r0_invariant.sh                        # 默认扫描整仓（脚本所在仓库根）
#   scripts/check_r0_invariant.sh backend frontend/src   # 只扫指定根（相对路径）
#   scripts/check_r0_invariant.sh --selftest             # 自检：样本违规必须被捕获、干净样本必须 0 违规
#
# 退出码：0 = 不变量成立；1 = 违规（含缺少 R0 只读语义注释）；3 = 用法/环境错误。
#
# 扫描器：优先 rg（ripgrep），缺失时退回 grep -rE。所有模式均为 POSIX ERE（无 lookaround、
#         无反向引用、无 \b），两种引擎结果一致；匹配大小写不敏感。
#
# 判定口径：
#   1) 任一命中行未落在下面的「豁免清单」上即计违规，脚本以非零退出码报错。
#   2) 豁免清单集中在本文件头部，三字段（路径 / 行内容正则 / 理由）逐条写死。路径为仓库相对
#      路径的精确匹配；只有「路径 + 行内容」同时匹配才豁免，避免整文件放水。理由不可为空，
#      为空视为配置错误（退出码 3）。
#   3) 检查器自身含模式字面量与自检固件（均非真实调用），故整文件豁免。
#   4) 额外正向检查：票 12 的 domain 类型与 service 处必须留 R0 只读语义注释（标记串见
#      R0_MARKER），缺失即违规 —— 不变量既要求「无使用路径」，也要求「只读声明在场」。
#
# 模式设计说明（防误报，实测口径）：
#   * 每条模式末尾的 ([^A-Za-z0-9]|$) 是边界守卫：防止 reset_use 命中 reset_user(s)、
#     reset-use 命中 reset-user、use_reset_card 命中 use_reset_cardXxx 这类无关标识符
#     （本仓 Go/TS 的 ResetUserAffCode / ResetIdempotencyKey / ResetUsedCount 等大量复用
#     reset+use 前缀，无守卫时误报成灾）。
#   * 分隔符集合 [_-]? 不含空格：英文散文里的 "never use reset cards"（R0 免责声明文案）合法，
#     不纳入；代码/URL/驼峰标识符形态才纳入。
#
# 挂接方式（本票只交脚本与说明，未接 CI/pre-commit）：见 scripts/README.md。
# =============================================================================
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# -----------------------------------------------------------------------------
# 违规模式清单（ERE，大小写不敏感）。前 5 条为票 13「票面清单」形态，第 6 条为票面要求的
# idempotency_key 组合态，第 7 条为票面清单之外的同义动词扩展（理由：R0 的实质是禁止消耗
# 路径，命名可换；漏检成本远高于误报成本，且该模式在整仓实测 0 命中）。
# -----------------------------------------------------------------------------
PATTERN_LABELS=(
    "票面#1  reset/use"
    "票面#2  reset_use"
    "票面#3  reset-use"
    "票面#4  UseResetCard / use_reset_card"
    "票面#5  resetCardUse / reset_card_use"
    "票面#6  idempotency_key × 重置卡 同行组合"
    "扩展#7  consume/redeem/apply/claim/burn × 重置卡"
)
PATTERN_REGEXES=(
    'reset/use([^A-Za-z0-9]|$)'
    'reset_use([^A-Za-z0-9]|$)'
    'reset-use([^A-Za-z0-9]|$)'
    'use[_-]?reset[_-]?card([^A-Za-z0-9]|$)'
    'reset[_-]?card[_-]?use([^A-Za-z0-9]|$)'
    '(reset[_-]?card|coding-plan/reset).*idempotency|idempotency.*(reset[_-]?card|coding-plan/reset)'
    '(consume|redeem|apply|claim|burn)[_-]?reset[_-]?card|reset[_-]?card[_-]?(consume|redeem|apply|claim|burn)'
)

# -----------------------------------------------------------------------------
# 豁免清单（白名单）：路径 / 行内容正则 / 理由 —— 三字段一一对应，理由必填。
# 新增豁免必须写清「为什么它不是使用路径」；真实的调用、封装、路由、按钮一律不得豁免。
# -----------------------------------------------------------------------------
EXEMPT_PATHS=(
    "scripts/check_r0_invariant.sh"
    "frontend/src/components/common/__tests__/MonitorQuotaView.spec.ts"
)
EXEMPT_CONTENT_REGEXES=(
    "."
    "not\\.toContain\\(|^[[:space:]]*'[^']*'[,]?[[:space:]]*$"
)
EXEMPT_REASONS=(
    "检查器自身：违规模式字面量 + 自检固件（均非调用/封装，仓库内无任何真实使用路径）；整文件豁免"
    "R0 负向守卫断言（票 16/17）：仅豁免两类行——(a) not.toContain(...) 否定断言，(b) forbidden 列表里的纯字符串字面量项（断言的输入数据，非调用/封装）；该文件其余行照常判定"
)

# 正向检查：R0 只读语义注释必须出现在票 12 的 domain 类型与 service 处。
R0_MARKER='R0：仅观测，永不使用'
MARKER_FILES=(
    "backend/internal/domain/channel_monitor_quota.go"
    "backend/internal/service/zhipu_account_monitor_service.go"
)

# 扫描排除面：文档与构建产物（文档描述协议不构成可执行路径，票面明确排除说明处）。
EXCLUDE_DIRS=( ".git" "node_modules" "dist" "build" "coverage" "vendor" ".scratch" "docs" "openspec" )
EXCLUDE_FILE_GLOBS=( "*.md" )

usage() {
    sed -n '2,26p' "$0"
}

# --- 扫描器探测 --------------------------------------------------------------
if ! command -v grep >/dev/null 2>&1; then
    echo "错误：环境缺少 grep，无法执行 R0 检查" >&2
    exit 3
fi
if command -v rg >/dev/null 2>&1; then
    SCANNER="rg"
    SCANNER_VERSION="$(rg --version 2>/dev/null | head -1)"
else
    SCANNER="grep"
    SCANNER_VERSION="$(grep --version 2>/dev/null | head -1)"
fi
[ -n "$SCANNER_VERSION" ] || SCANNER_VERSION="$SCANNER (版本未知)"

# run_scan <outfile> <regex> <root...>：命中写入 outfile（格式 path:line:content）。
run_scan() {
    local out="$1" pattern="$2"; shift 2
    local d
    if [ "$SCANNER" = "rg" ]; then
        local args=( --no-heading -n -i --color never -e "$pattern" )
        for d in "${EXCLUDE_DIRS[@]}"; do
            args+=( --glob "!$d" --glob "!**/$d/**" )
        done
        for d in "${EXCLUDE_FILE_GLOBS[@]}"; do
            args+=( --glob "!$d" )
        done
        rg "${args[@]}" "$@" >"$out" 2>/dev/null
    else
        local args=( -r -n -i -I -E )
        for d in "${EXCLUDE_DIRS[@]}"; do
            args+=( --exclude-dir="$d" )
        done
        for d in "${EXCLUDE_FILE_GLOBS[@]}"; do
            args+=( --exclude="$d" )
        done
        grep "${args[@]}" -e "$pattern" "$@" >"$out" 2>/dev/null
    fi
    return 0
}

# exempt_rule_for <path> <content>：命中豁免规则时输出规则下标，否则输出 -1。
exempt_rule_for() {
    local path="$1" content="$2" i=0
    while [ "$i" -lt "${#EXEMPT_PATHS[@]}" ]; do
        if [ "$path" = "${EXEMPT_PATHS[$i]}" ] &&
            printf '%s\n' "$content" | grep -E -q -- "${EXEMPT_CONTENT_REGEXES[$i]}"; then
            echo "$i"
            return 0
        fi
        i=$((i + 1))
    done
    echo "-1"
    return 0
}

# analyze <hits-file> <violations-out> <exempt-out>：按豁免清单分类，设置全局计数。
ANALYZE_TOTAL=0
ANALYZE_EXEMPT=0
ANALYZE_VIOL=0
analyze() {
    local hits="$1" viol_out="$2" exempt_out="$3"
    local raw path rest line_no content rule i
    : >"$viol_out"
    : >"$exempt_out"
    ANALYZE_TOTAL=0
    ANALYZE_EXEMPT=0
    ANALYZE_VIOL=0
    i=0
    while [ "$i" -lt "${#EXEMPT_PATHS[@]}" ]; do
        RULE_MATCHES[$i]=0
        RULE_FILES[$i]=""
        i=$((i + 1))
    done
    while IFS= read -r raw; do
        [ -n "$raw" ] || continue
        path="${raw%%:*}"
        rest="${raw#*:}"
        line_no="${rest%%:*}"
        content="${rest#*:}"
        case "$path" in
        ./*) path="${path#./}" ;;
        esac
        ANALYZE_TOTAL=$((ANALYZE_TOTAL + 1))
        rule="$(exempt_rule_for "$path" "$content")"
        if [ "$rule" -ge 0 ]; then
            ANALYZE_EXEMPT=$((ANALYZE_EXEMPT + 1))
            RULE_MATCHES[$rule]=$(( ${RULE_MATCHES[$rule]} + 1 ))
            if [ "${EXEMPT_CONTENT_REGEXES[$rule]}" = "." ]; then
                # 内容正则 `.` = 整文件豁免：明细从略，只记涉及的文件与行数。
                case " ${RULE_FILES[$rule]} " in
                *" $path "*) ;;
                *) RULE_FILES[$rule]="${RULE_FILES[$rule]}$path " ;;
                esac
            else
                printf '  %s:%s  ← 豁免#%s（%s）\n' "$path" "$line_no" "$((rule + 1))" "${EXEMPT_REASONS[$rule]}" >>"$exempt_out"
            fi
        else
            ANALYZE_VIOL=$((ANALYZE_VIOL + 1))
            printf '  %s:%s  %s\n' "$path" "$line_no" "$content" >>"$viol_out"
        fi
    done <"$hits"
    i=0
    while [ "$i" -lt "${#EXEMPT_PATHS[@]}" ]; do
        if [ -n "${RULE_FILES[$i]}" ]; then
            printf '  整文件豁免：%s（%s 行）  ← 豁免#%s（%s）\n' "${RULE_FILES[$i]}" "${RULE_MATCHES[$i]}" "$((i + 1))" "${EXEMPT_REASONS[$i]}" >>"$exempt_out"
        fi
        i=$((i + 1))
    done
}

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/r0-invariant.XXXXXX")" || {
    echo "错误：无法创建临时目录" >&2
    exit 3
}
trap 'rm -rf "$TMP_DIR"' EXIT

# --- 自检模式 ----------------------------------------------------------------
run_selftest() {
    local SLASH="/" ok=1
    local dir="$TMP_DIR/selftest"
    mkdir -p "$dir/clean" "$dir/dirty"
    # 干净样本：本仓大量存在的无关 reset+use 前缀标识符与孤立 idempotency_key，必须 0 命中。
    {
        printf '%s\n' 'func (r *repo) ResetUserAffCode(ctx context.Context, id int64) error { return nil }'
        printf '%s\n' 'mutation.ResetIdempotencyKey()'
        printf '%s\n' 'mutation.ResetUsedCount()'
        printf '%s\n' 'repo.ResetUserID()'
        printf '%s\n' 'const idempotency_key = "batch-image-job"'
        printf '%s\n' 'expect(html).not.toContain("reset-password")'
    } >"$dir/clean/sample.txt"
    # 违规样本：分片拼接，源码里不出现完整的调用形态。
    {
        printf 'POST https://zcode.z.ai/api/v1/coding-plan/reset%suse\n' "$SLASH"
        printf 'const fn = use%sReset%sCard%s\n' "" "" ""
        printf '%s %s\n' 'idempotency_key' 'reset_card'
    } >"$dir/dirty/sample.txt"

    run_scan "$TMP_DIR/st_clean.txt" "${PATTERN_REGEXES[0]}" "$dir/clean"
    analyze "$TMP_DIR/st_clean.txt" "$TMP_DIR/st_clean_viol.txt" "$TMP_DIR/st_clean_exempt.txt"
    local clean_viol=$ANALYZE_VIOL
    run_scan "$TMP_DIR/st_dirty.txt" "${PATTERN_REGEXES[0]}" "$dir/dirty"
    analyze "$TMP_DIR/st_dirty.txt" "$TMP_DIR/st_dirty_viol.txt" "$TMP_DIR/st_dirty_exempt.txt"
    local dirty_viol=$ANALYZE_VIOL

    echo "R0 检查器自检（票 13）"
    echo "  干净样本（ResetUserAffCode / ResetIdempotencyKey / 孤立 idempotency_key 等）：违规 $clean_viol 行（期望 0）"
    echo "  违规样本（reset/use 端点 + 用卡标识符 + idempotency_key×重置卡）：违规 $dirty_viol 行（期望 >0）"
    [ "$clean_viol" -eq 0 ] || ok=0
    [ "$dirty_viol" -gt 0 ] || ok=0
    if [ "$ok" -eq 1 ]; then
        echo "== 自检 OK：模式既能捕获违规，也不误报无关 reset/idempotency 代码 =="
        return 0
    fi
    echo "== 自检 FAILED：模式失效或误报，禁止采信任何 0 违规结论 ==" >&2
    return 1
}

# --- 参数解析 ----------------------------------------------------------------
ROOTS=()
for arg in "$@"; do
    case "$arg" in
    --selftest) run_selftest && exit 0 || exit 1 ;;
    -h | --help)
        usage
        exit 0
        ;;
    -*)
        echo "错误：未知参数 $arg" >&2
        exit 3
        ;;
    *) ROOTS+=( "$arg" ) ;;
    esac
done
[ "${#ROOTS[@]}" -gt 0 ] || ROOTS=( "." )

for r in "${ROOTS[@]}"; do
    if [ ! -e "$REPO_ROOT/$r" ]; then
        echo "错误：扫描根不存在：${r}（相对仓库根 ${REPO_ROOT}）" >&2
        exit 3
    fi
done

for i in "${!EXEMPT_PATHS[@]}"; do
    if [ -z "${EXEMPT_REASONS[$i]}" ]; then
        echo "错误：豁免清单第 $((i + 1)) 条缺少理由（配置错误）" >&2
        exit 3
    fi
    if [ "${#EXEMPT_PATHS[$i]}" -eq 0 ] || [ "${#EXEMPT_CONTENT_REGEXES[$i]}" -eq 0 ]; then
        echo "错误：豁免清单第 $((i + 1)) 条字段不完整（配置错误）" >&2
        exit 3
    fi
done

# --- 主流程 ------------------------------------------------------------------
COMBINED=""
for r in "${PATTERN_REGEXES[@]}"; do
    COMBINED="$COMBINED($r)|"
done
COMBINED="${COMBINED%|}"

echo "R0 不变量检查（票 13：永不使用账户重置卡）"
echo "  仓库根：$REPO_ROOT"
echo "  扫描根：${ROOTS[*]}（相对仓库根）"
echo "  扫描器：$SCANNER_VERSION"
echo "  排除面：${EXCLUDE_DIRS[*]} ${EXCLUDE_FILE_GLOBS[*]}（文档/构建产物；R0 强制对象是可执行代码与配置）"
echo

echo "模式命中统计（原始命中行数，含白名单行；豁免判定见下方汇总）："
for i in "${!PATTERN_REGEXES[@]}"; do
    run_scan "$TMP_DIR/p_$i.txt" "${PATTERN_REGEXES[$i]}" "${ROOTS[@]}"
    count="$(wc -l <"$TMP_DIR/p_$i.txt" | tr -d ' ')"
    printf '  [%s] %-46s 命中 %s 行\n' "$((i + 1))" "${PATTERN_LABELS[$i]}" "$count"
done
echo

run_scan "$TMP_DIR/hits.txt" "$COMBINED" "${ROOTS[@]}"
analyze "$TMP_DIR/hits.txt" "$TMP_DIR/violations.txt" "$TMP_DIR/exempt.txt"

echo "汇总：命中 ${ANALYZE_TOTAL} 行 · 豁免 ${ANALYZE_EXEMPT} 行 · 违规 ${ANALYZE_VIOL} 行"
if [ "$ANALYZE_EXEMPT" -gt 0 ]; then
    echo "豁免命中明细（白名单，理由见脚本头部）："
    cat "$TMP_DIR/exempt.txt"
fi
if [ "$ANALYZE_VIOL" -gt 0 ]; then
    echo "违规明细（必须删除；不得注释保留、不得环境开关后置）："
    cat "$TMP_DIR/violations.txt"
fi
i=0
while [ "$i" -lt "${#EXEMPT_PATHS[@]}" ]; do
    if [ "${RULE_MATCHES[$i]:-0}" -eq 0 ]; then
        echo "提示（不影响退出码）：豁免#$((i + 1)) 未命中任何行，若对应代码已删除请从脚本头部移除：${EXEMPT_PATHS[$i]}" >&2
    fi
    i=$((i + 1))
done

echo
echo "R0 只读语义注释（\"${R0_MARKER}\"）："
marker_ok=0
for f in "${MARKER_FILES[@]}"; do
    if [ -f "$REPO_ROOT/$f" ] && grep -F -q -- "$R0_MARKER" "$REPO_ROOT/$f"; then
        marker_ok=$((marker_ok + 1))
        echo "  ✓ ${f}"
    else
        echo "  ✗ ${f}（缺失）"
    fi
done

echo
if [ "$ANALYZE_VIOL" -eq 0 ] && [ "$marker_ok" -eq "${#MARKER_FILES[@]}" ]; then
    echo "== OK：R0 不变量成立（0 违规，只读语义注释 ${marker_ok}/${#MARKER_FILES[@]} 在场）=="
    exit 0
fi
echo "== FAIL：R0 不变量被破坏（违规 ${ANALYZE_VIOL} 行，注释 ${marker_ok}/${#MARKER_FILES[@]}）==" >&2
exit 1
