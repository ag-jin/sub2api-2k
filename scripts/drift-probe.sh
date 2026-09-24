#!/bin/zsh
# drift-probe.sh — 范围漂移探针（只读）
#
# 目的: 检出「文档声明的范围」与「代码实际实现」的矛盾。
# 依据: docs/exec-plans/active/2026-09-23-dev-workflow/DRIFT-DETECTION-EXPERIMENT.md
#
# 用法:
#   scripts/drift-probe.sh --design <design.md> --code <dir> --platform <name> [--map <map.tsv>]
#
# map.tsv 格式（每行: 项目名<TAB>关键词正则）——从 design.md 的「非目标」段自动提取，
# 也可用 --map 显式提供。提取不到任何项时报错，不静默通过。
#
# 精确判据（从实验修正而来）:
#   1. 排除 *_test.go —— 测试里的"拒绝 X"是守约证据，不是实现
#   2. 排除拒绝语义    —— 命中行含 reject/拒绝/not/不含 ⇒ 计入"疑似守约"
#   3. verdict 必须综合 impl 与 guard —— guard 高时不判"疑似已实现"（防假阳性）
set -u
DESIGN=""; CODE=""; PLATFORM=""; MAP=""
while (( $# > 0 )); do
  case "$1" in
    --design)   DESIGN="$2";   shift 2 ;;
    --code)     CODE="$2";     shift 2 ;;
    --platform) PLATFORM="$2"; shift 2 ;;
    --map)      MAP="$2";      shift 2 ;;
    *) print -u2 -- "UNKNOWN_ARG\t$1"; exit 2 ;;
  esac
done
[[ -n "$DESIGN" ]] || { print -u2 -- "MISSING_ARG\t--design"; exit 2 }
[[ -n "$CODE" ]]   || { print -u2 -- "MISSING_ARG\t--code"; exit 2 }
[[ -n "$PLATFORM" ]] || PLATFORM="${DRIFT_PLATFORM:-}"
[[ -n "$PLATFORM" ]] || { print -u2 -- "MISSING_ARG\t--platform"; exit 2 }
[[ -f "$DESIGN" ]] || { print -u2 -- "MISSING_DESIGN\t$DESIGN"; exit 1 }
[[ -d "$CODE" ]]   || { print -u2 -- "MISSING_CODE_ROOT\t$CODE"; exit 1 }
command -v grep >/dev/null 2>&1 || { print -u2 -- "MISSING_DEPENDENCY_grep"; exit 1 }
[[ -s "$DESIGN" ]] || { print -u2 -- "EMPTY_DESIGN\t$DESIGN\t设计文件为空，无法提取对照物"; exit 1 }

TMPMAP=""
if [[ -z "$MAP" ]]; then
  # 从 design.md 的「非目标」段落提取关键词（不再硬编码）
  TMPMAP=$(mktemp) || { print -u2 -- "MKTEMP_FAILED"; exit 1 }
  trap 'rm -f "$TMPMAP"' EXIT
  ruby - "$DESIGN" "$TMPMAP" <<'RB'
# encoding: UTF-8
text = File.read(ARGV[0], encoding: 'UTF-8')
out = ARGV[1]
# 取「非目标」小节内容；支持 markdown 标题与行内两种写法
sec = text[/^#+.*非目标.*?$(.*?)(?=^#+\s|\z)/m, 1]
sec ||= text[/非目标[^\n]*\n(.*?)(?=\n#|\z)/m, 1]
abort('NO_NONGOAL_SECTION') if sec.nil? || sec.strip.empty?
items = sec.split(/[、,，\n]/).map { |x| x.gsub(/^[-*\d.\s]+/, '').strip }.reject { |x| x.empty? || x.length > 40 }
abort('NO_NONGOAL_ITEMS') if items.empty?
rows = items.map do |it|
  # 从中文项里抽取英文/标识符片段作为代码关键词（如 /v1/responses、OAuth、429、desensitize）
  kws = it.scan(%r{/?[A-Za-z][A-Za-z0-9_./-]{1,}}).reject { |w| w.length < 3 }
  if kws.empty?
    # 纯中文项：抽连续中文片段（≥2 字）作为低置信关键词。
    # 依据：实测纯中文非目标项被标为"未能抽取"，实际覆盖率仅 25%；
    # 中文项在中文项目里很常见，不应直接跳过。
    zh = it.scan(/[\u4e00-\u9fa5]{2,}/)
    kw = zh.empty? ? nil : zh.join('|')
  else
    kw = kws.join('|')
  end
  "#{it}\t#{kw || '__NEEDS_KEYWORD__'}"
end
File.write(out, rows.join("\n") + "\n")
RB
  rc=$?
  if (( rc != 0 )); then
    print -u2 -- "CANNOT_EXTRACT_NONGOALS\t$DESIGN\t未找到可用的「非目标」段，请用 --map 显式提供"
    exit 1
  fi
  MAP="$TMPMAP"
fi
[[ -f "$MAP" ]] || { print -u2 -- "MISSING_MAP\t$MAP"; exit 1 }
[[ -s "$MAP" ]] || { print -u2 -- "EMPTY_MAP\t$MAP\t对照表为空，拒绝给出结论"; exit 1 }

# 校验 map 正则合法性（非法正则会导致静默"守约"）
if ! ruby -e 'File.readlines(ARGV[0], encoding: "UTF-8").each { |l| n, k = l.chomp.split("\t", 2); next if n.nil? || k.nil?; Regexp.new(k) }' "$MAP" 2>/dev/null; then
  print -u2 -- "INVALID_MAP_REGEX\t$MAP"; exit 1
fi

print -- "DRIFT_PROBE\tplatform=$PLATFORM"
print -- "DESIGN\t$DESIGN"
print -- "CODE\t$CODE"
print -- "MAP\t$MAP"
print -- ""
printf '%-24s %-8s %-8s %s\n' "非目标项(来自 design)" "实现" "疑似守约" "判读"
need_kw=0
while IFS=$'\t' read -r name kw; do
  [[ -z "$name" ]] && continue
  if [[ "$kw" == "__NEEDS_KEYWORD__" ]]; then
    printf '%-24s %-8s %-8s %s\n' "${name:0:22}" "-" "-" "⚠ 未能抽取关键词，需 --map 显式提供"
    need_kw=$(( need_kw + 1 ))
    continue
  fi
  # 平台归属按**文件路径**判定，不要求每行都含平台名。
  # 依据：原判据要求命中行含平台名 → 平台专属文件内的实现被漏检
  # （实测 oauth.go 里写"多租户隔离实现"却报"守约"，假阴性）。
  # 判据：文件路径含平台名（大小写不敏感）⇒ 该文件属于此平台。
  hits=$(grep -rn -E "$kw" "$CODE" --include='*.go' 2>/dev/null | grep -i "$PLATFORM")
  all=$(print -- "$hits" | grep -c . )
  tests=$(print -- "$hits" | grep -c '_test\.go')
  impl=$(( all - tests ))
  guard=$(print -- "$hits" | grep -ciE 'reject|拒绝|not |不含|unsupported')
  # verdict 必须综合 impl 与 guard（旧版 guard 算了不用 = 死代码，导致假阳性）
  if (( impl > 0 && guard > 0 )); then
    verdict="需人工复核：疑实现/或守约（两者均命中）"
  elif (( impl > 0 )); then
    verdict="★需复核：疑似已实现"
  else
    verdict="守约：未见实现"
  fi
  printf '%-24s %-8s %-8s %s\n' "${name:0:22}" "$impl" "$guard" "$verdict"
done < "$MAP"
print -- ""
(( need_kw == 0 )) || print -- "⚠ 有 ${need_kw} 项未能抽取关键词 —— 这些项未被真正检查（不是『守约』）"
print -- ""
print -- "注意：命中仅为『需人工复核』信号，不构成『已实现』结论（关键词命中 ≠ 功能等价）。"
print -- ""
print -- "边界（实测量化，不得省略）："
print -- "  1. 只能检『多做了什么』，检不出『少做了什么』（后者用 --coverage-check）"
print -- "  2. 判『守约』≠『确实没做』：若实现使用了同义词，本工具检不出。"
print -- "     实测：文档写 desensitize、实现文件叫 prompt_sanitize.go → 误判守约。"
print -- "  3. 平台归属按文件路径判定；路径不含平台名时可能漏检。"
print -- "  4. 关键词由非目标原文抽取（英文片段优先，纯中文项取中文片段），"
print -- "     抽取不到时显式报『未能抽取』，需 --map 显式提供。"
