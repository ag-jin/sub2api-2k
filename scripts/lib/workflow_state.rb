# encoding: UTF-8
# workflow_state.rb — 开发工作流状态解析与校验（单一真相）
#
# 为什么单独成库：渲染器与校验器必须用同一套规则判定。
# 若两处各写一份，就会重演本仓库已验证过的"文档与实现漂移"问题。
#
# 调用方式（必须用文件形式；ruby 内联 -e 遇中文必崩，已实测）：
#   ruby scripts/lib/workflow_state.rb --check   <state.json> [<base-dir>]
#   ruby scripts/lib/workflow_state.rb --summary <state.json>
#   ruby scripts/lib/workflow_state.rb --render  <state.json>

require 'json'

STAGES = %w[intake design-draft prototype design-final tasks implement review].freeze
STAGE_LABELS = {
  'intake' => '信息采集', 'design-draft' => '设计初稿', 'prototype' => '原型验证',
  'design-final' => '设计终稿', 'tasks' => '任务', 'implement' => '实现', 'review' => '审查'
}.freeze
OK_STATUS = %w[pending active done blocked skipped].freeze
OK_LINE_STATE = %w[active paused deferred done].freeze
OK_MERGE_STATE = %w[merged not-merged abandoned].freeze
OK_VERDICT = %w[PASS PARTIAL FAIL].freeze
# 证据最小字节数：排除空/占位文件，但不误拒合法的短结论（可用 WORKFLOW_EVIDENCE_MIN 覆盖）
EVIDENCE_MIN_BYTES = (ENV['WORKFLOW_EVIDENCE_MIN'] || '4').to_i
STAGE_MARK = { 'pending' => '.', 'active' => '>', 'done' => 'x', 'blocked' => '!', 'skipped' => '-' }.freeze

# 校验：返回违规数组（空 = 通过）
def validate(doc, base_dir)
  errs = []
  lines = doc['lines']
  unless lines.is_a?(Array)
    return ['MISSING_LINES 顶层缺少 lines 数组']
  end

  # 顶层 stages 必须是数组且非空
  top_stages = doc['stages']
  if !top_stages.is_a?(Array) || top_stages.empty?
    errs << 'MISSING_TOP_STAGES 顶层缺少非空 stages 数组'
  elsif top_stages != STAGES
    # 截断/乱序/伪造会让渲染器只输出部分阶段，击穿「全阶段必显示」不变量
    errs << "TOP_STAGES_MISMATCH 期望 #{STAGES.join(',')} 实际 #{top_stages.join(',')}"
  end

  # coverage：可选，来源清单的逐项处置（零静默遗漏）
  cov = doc['coverage']
  if !cov.nil? && !cov.is_a?(Array)
    errs << "COVERAGE_NOT_ARRAY_#{cov.class}"
  elsif cov.is_a?(Array)
    ids = lines.map { |l| l['id'] }
    cov.each_with_index do |c, i|
      unless c.is_a?(Hash)
        errs << "COVERAGE_ENTRY_NOT_OBJECT_#{i}"
        next
      end
      item = c['item'].to_s
      errs << "COVERAGE_ENTRY_WITHOUT_ITEM_#{i}" if item.strip.empty?
      disp = c['disposition']
      unless %w[adopted adapted rejected deferred].include?(disp)
        errs << "COVERAGE_BAD_DISPOSITION_#{item}_#{disp.inspect}"
      end
      if c['rationale'].nil? || c['rationale'].to_s.strip.empty?
        errs << "COVERAGE_WITHOUT_RATIONALE_#{item}"
      end
      if c['verifier'].nil? || c['verifier'].to_s.strip.empty?
        errs << "COVERAGE_WITHOUT_VERIFIER_#{item}"
      end
      ow = c['owner']
      if ow.nil? || ow.to_s.strip.empty?
        errs << "COVERAGE_WITHOUT_OWNER_#{item}"
      elsif !ids.include?(ow)
        errs << "COVERAGE_OWNER_NOT_A_LINE_#{item}_#{ow}"
      end
    end
  end

  # scope_exclude：可选，数组
  se = doc['scope_exclude']
  if !se.nil? && !se.is_a?(Array)
    errs << "SCOPE_EXCLUDE_NOT_ARRAY_#{se.class}"
  end

  # stage_labels 若提供，必须覆盖全部阶段（部分覆盖会导致看板中英混杂）
  sl = doc['stage_labels']
  if sl.is_a?(Hash)
    miss = STAGES - sl.keys
    errs << "STAGE_LABELS_INCOMPLETE #{miss.join(',')}" unless miss.empty?
  elsif !sl.nil?
    errs << "STAGE_LABELS_NOT_HASH_#{sl.class}"
  end

  # 重复 id（找不到 id 的线也算违规）
  seen_ids = {}
  lines.each do |l|
    lid = l['id']
    if lid.nil? || lid.to_s.strip.empty?
      errs << 'LINE_WITHOUT_ID 存在无 id 的线'
      next
    end
    if seen_ids.key?(lid)
      errs << "DUPLICATE_LINE_ID_#{lid}"
    end
    seen_ids[lid] = true
  end

  lines.each do |l|
    id = l['id'].to_s
    st_map = l['stages']
    unless st_map.is_a?(Hash)
      errs << "LINE_#{id}_MISSING_STAGES 缺少 stages 映射"
      next
    end

    missing = STAGES - st_map.keys
    extra   = st_map.keys - STAGES
    errs << "LINE_#{id}_MISSING_STAGES #{missing.join(',')}" unless missing.empty?
    errs << "LINE_#{id}_EXTRA_STAGES #{extra.join(',')}"   unless extra.empty?

    # 先归一化：坏值记录一次后替换成空 Hash，避免后续所有分支各自崩溃
    # （三模型审查共同指出：不归一化则同一坏数据在不同代码路径表现不一致）
    bad_stages = []
    st_map = st_map.each_with_object({}) do |(k, v), acc|
      if v.is_a?(Hash)
        acc[k] = v
      else
        bad_stages << [k, v.class]
        acc[k] = {}
      end
    end
    bad_stages.each { |sname, klass| errs << "LINE_#{id}_BAD_STAGE_VALUE_#{sname}_#{klass}" }

    st_map.each do |sname, v|
      stat = v['status']
      errs << "LINE_#{id}_BAD_STATUS_#{sname}_#{stat.inspect}" unless OK_STATUS.include?(stat)
      # skipped 必须说明理由（否则可用来跳过任何阶段而不留痕）
      if stat == 'skipped' && (v['skip_reason'].nil? || v['skip_reason'].to_s.strip.empty?)
        errs << "LINE_#{id}_SKIPPED_WITHOUT_REASON_#{sname}"
      end
      # 非 done 状态一律不得带 verdict（未完成何来结论）
      # 依据：A1 核心防线若只覆盖 done，blocked/skipped 会成为无约束地带（实测 rc=0 绕过）
      if stat != 'done' && !v['verdict'].nil?
        errs << "LINE_#{id}_VERDICT_ON_UNFINISHED_#{sname}_#{stat}_#{v['verdict']}"
      end
      # 非 done 状态也不得带非空 uncovered（漏出"承认有未覆盖项"的自相矛盾态）
      if stat != 'done' && v['uncovered'].is_a?(Array) && !v['uncovered'].empty?
        errs << "LINE_#{id}_UNCOVERED_ON_UNFINISHED_#{sname}_#{stat}"
      end
      next unless stat == 'done'

      vd = v['verdict']
      if vd.nil?
        errs << "LINE_#{id}_DONE_WITHOUT_VERDICT_#{sname}"
      elsif !OK_VERDICT.include?(vd)
        errs << "LINE_#{id}_BAD_VERDICT_#{sname}_#{vd}"
      end

      unc = v['uncovered']
      if !unc.nil? && !unc.is_a?(Array)
        # 字符串/对象会被 `!unc.empty?` 误判，从而绕过 A1 双向防线
        errs << "LINE_#{id}_UNCOVERED_NOT_ARRAY_#{sname}_#{unc.class}"
        unc = []
      end
      unc = [] if unc.nil?
      if unc.is_a?(Array) && unc.any? { |u| u.nil? || u.to_s.strip.empty? }
        errs << "LINE_#{id}_UNCOVERED_BLANK_ENTRY_#{sname}"
      end
      # A1 核心防线：结论范围不得大于验证范围
      if vd == 'PASS' && !unc.empty?
        errs << "LINE_#{id}_PASS_WITH_UNCOVERED_#{sname} 结论为 PASS 但列出未覆盖项：#{unc.join('; ')}"
      end
      if vd == 'PARTIAL' && unc.empty?
        errs << "LINE_#{id}_PARTIAL_WITHOUT_UNCOVERED_#{sname} 结论为 PARTIAL 但未列出未覆盖项"
      end
      # FAIL 却标 done = 把失败当完成（此前漏检）
      if vd == 'FAIL'
        errs << "LINE_#{id}_FAIL_MARKED_DONE_#{sname} 结论为 FAIL 却标记 done"
      end

      # artifact：可选字段，但填了就必须存在。
      # 允许**目录**：真实项目的阶段产物常是多文件目录（跨项目试跑实测：
      # tasks 阶段产物是 review-run/（4 个任务）、intake 问题集是 issues/（3 个 issue））
      art = v['artifact']
      if !art.nil? && !art.to_s.strip.empty?
        af = resolve_path(art.to_s, base_dir)
        errs << "LINE_#{id}_ARTIFACT_MISSING_#{sname}_#{art}" unless af && File.exist?(af)
      end
      ev = v['evidence']
      if !ev.is_a?(Array) || ev.empty?
        errs << "LINE_#{id}_DONE_WITHOUT_EVIDENCE_#{sname}"
      else
        ev.each do |p|
          ep = p.to_s
          if ep.strip.empty?
            errs << "LINE_#{id}_EVIDENCE_EMPTY_#{sname}"
            next
          end
          rf = resolve_path(ep, base_dir)
          if rf.nil?
            errs << "LINE_#{id}_EVIDENCE_ESCAPES_BASE_#{sname}_#{ep}"
          elsif !File.file?(rf)
            errs << "LINE_#{id}_EVIDENCE_MISSING_#{sname}_#{ep}"
          else
            # 最小实质内容：0 字节/纯空白文件不构成证据
            # 依据：实测 0 字节证据 rc=0 通过 → "完成"可被形式化伪造
            # 边界：只查"非空且达到最小字节"，**不判断内容质量**
            sz = File.size(rf)
            if sz < EVIDENCE_MIN_BYTES
              errs << "LINE_#{id}_EVIDENCE_TOO_SMALL_#{sname}_#{ep}_#{sz}B"
            elsif File.read(rf, 4096).to_s.strip.empty?
              errs << "LINE_#{id}_EVIDENCE_BLANK_#{sname}_#{ep}"
            end
          end
        end
      end

      # questions（若有）路径必须存在；允许目录（问题集常是多文件目录）
      q = v['questions']
      if !q.nil? && !q.to_s.strip.empty?
        qf = resolve_path(q.to_s, base_dir)
        errs << "LINE_#{id}_QUESTIONS_MISSING_#{sname}_#{q}" unless qf && File.exist?(qf)
      end
    end

    # 跳序：某阶段 done 而任一前序未 done
    STAGES.each_with_index do |sname, i|
      v = st_map[sname]
      next unless v.is_a?(Hash) && v['status'] == 'done'
      prev = STAGES[0...i]
      # skipped 是"合法跳过"，应视为已满足前序（否则 skip 一旦不在末位就毒化全部下游）
      bad = prev.reject { |p| %w[done skipped].include?((st_map[p] || {})['status']) }
      errs << "LINE_#{id}_OUT_OF_ORDER_#{sname}_blocked_by_#{bad.join('+')}" unless bad.empty?
    end

    # 字段卫生：标记串会污染注入定位；控制字符会污染 AGENTS.md 可读性
    # 实测：title 含 0x01、next_action 含 0x07 时，控制字符进入用户 AGENTS.md 且无报警
    %w[title next_action hold_reason].each do |k|
      val = l[k].to_s
      if val.include?('WORKFLOW-STATE:')
        errs << "LINE_#{id}_FIELD_CONTAINS_MARKER_#{k}"
      end
      bad = val.chars.select { |c| c.ord < 0x20 && c != "\t" && c != "\n" }
      unless bad.empty?
        codes = bad.uniq.map { |c| format('0x%02x', c.ord) }.join(',')
        errs << "LINE_#{id}_FIELD_CONTAINS_CONTROL_CHAR_#{k}_#{codes}"
      end
    end
    # 阶段名与标签同样不得含控制字符
    st_map.each_key do |sname|
      if sname.to_s.chars.any? { |c| c.ord < 0x20 }
        errs << "LINE_#{id}_STAGE_NAME_CONTROL_CHAR"
      end
    end

    # write_scope：可选，声明本条线允许改动的路径（前缀匹配）
    wsz = l['write_scope']
    if !wsz.nil? && !wsz.is_a?(Array)
      errs << "LINE_#{id}_WRITE_SCOPE_NOT_ARRAY_#{wsz.class}"
    elsif wsz.is_a?(Array) && wsz.any? { |x| x.nil? || x.to_s.strip.empty? }
      errs << "LINE_#{id}_WRITE_SCOPE_BLANK_ENTRY"
    end

    # priority 必须是整数（字符串 'high'.to_i => 0 会静默抢走 goal 归属）
    pr = l['priority']
    if !pr.is_a?(Integer)
      errs << "LINE_#{id}_BAD_PRIORITY_#{pr.class}_#{pr.inspect}"
    elsif pr < 0
      errs << "LINE_#{id}_NEGATIVE_PRIORITY_#{pr}"
    end

    # 线级状态
    lstate = l['state']
    errs << "LINE_#{id}_BAD_LINE_STATE_#{lstate}" unless OK_LINE_STATE.include?(lstate)
    # active=进行中、done=已完成，都不需要搁置原因；paused/deferred 才需要
    if %w[paused deferred].include?(lstate) && (l['hold_reason'].nil? || l['hold_reason'].to_s.strip.empty?)
      errs << "LINE_#{id}_HOLD_WITHOUT_REASON"
    end

    ms = l['merge_state']
    if ms.nil?
      errs << "LINE_#{id}_MISSING_MERGE_STATE"
    elsif !OK_MERGE_STATE.include?(ms)
      errs << "LINE_#{id}_BAD_MERGE_STATE_#{ms}"
    elsif ms == 'not-merged' && !%w[active done].include?(lstate) &&
          (l['hold_reason'].nil? || l['hold_reason'].to_s.strip.empty?)
      # active 线正在推进，本来就没合并 —— 正常，不要求 reason
      # 非 active 且未合并 = 搁置了却没说明 —— 这才违规
      errs << "LINE_#{id}_NOT_MERGED_WITHOUT_REASON"
    end

    cs = l['current_stage']
    if cs && (st_map[cs] || {})['status'] != 'active'
      errs << "LINE_#{id}_CURRENT_STAGE_NOT_ACTIVE_#{cs}"
    end

    # 线级语义交叉矛盾（字段间布尔关系，属结构校验射程）
    stat_list = STAGES.map { |s| (st_map[s] || {})['status'] }
    if lstate == 'done'
      not_done = STAGES.each_with_index.reject { |_, i| %w[done skipped].include?(stat_list[i]) }
      unless not_done.empty?
        errs << "LINE_#{id}_DONE_BUT_STAGES_#{not_done.map { |s, _| s }.join('+')}"
      end
      errs << "LINE_#{id}_DONE_WITH_CURRENT_STAGE" if cs
    end
    if lstate == 'active' && stat_list.all? { |x| x == 'done' }
      errs << "LINE_#{id}_ACTIVE_BUT_ALL_STAGES_DONE"
    end
    if ms == 'abandoned' && lstate == 'active'
      errs << "LINE_#{id}_ABANDONED_BUT_ACTIVE"
    end
    # blocked 阶段需理由（与 skipped 对称）
    STAGES.each do |sname|
      v = st_map[sname]
      next unless v.is_a?(Hash) && v['status'] == 'blocked'
      if v['block_reason'].nil? && v['note'].nil? && (l['hold_reason'].nil? || l['hold_reason'].to_s.strip.empty?)
        errs << "LINE_#{id}_BLOCKED_WITHOUT_REASON_#{sname}"
      end
    end
  end

  # recommendation（若有）必须指向真实存在的线
  rec = doc['recommendation']
  if rec.is_a?(Hash) && rec['line']
    unless lines.map { |l| l['id'] }.include?(rec['line'])
      errs << "RECOMMENDATION_POINTS_TO_MISSING_LINE_#{rec['line']}"
    end
  end

  # goal 校验：目标必须指向真实存在的线，且该线必须 active
  g = doc['goal']
  if !g.nil? && !g.is_a?(Hash)
    errs << "GOAL_NOT_HASH_#{g.class}"
  elsif g.is_a?(Hash)
    if g['objective'].nil? || g['objective'].to_s.strip.empty?
      errs << 'GOAL_WITHOUT_OBJECTIVE'
    end
    if g['line'].nil? || g['line'].to_s.strip.empty?
      errs << 'GOAL_WITHOUT_LINE'
    end
    # goal.stage 必须等于目标线的 current_stage（否则目标与实际位置不符）
    if g['line'] && g['stage']
      tgt = lines.find { |l| l['id'] == g['line'] }
      if tgt && tgt['current_stage'] != g['stage']
        errs << "GOAL_STAGE_MISMATCH_宣告_#{g['stage']}_实际_#{tgt['current_stage']}"
      end
    end
  end
  if g.is_a?(Hash) && g['line']
    ids = lines.map { |l| l['id'] }
    unless ids.include?(g['line'])
      errs << "GOAL_POINTS_TO_MISSING_LINE_#{g['line']}"
    else
      tgt = lines.find { |l| l['id'] == g['line'] }
      if tgt['state'] != 'active'
        errs << "GOAL_POINTS_TO_INACTIVE_LINE_#{g['line']}_#{tgt['state']}"
      end
      # 目标线应是最高优先度的 active 线（否则 = 目标漂移）
      # 必须与 derive_goal 用同一排序键，否则并列 priority 时自相矛盾
      top = lines.select { |l| l['state'] == 'active' }.sort_by { |l| [l['priority'].to_i, l['id'].to_s] }.first
      if top && top['id'] != g['line']
        errs << "GOAL_DRIFT_期望_#{top['id']}_实际_#{g['line']}"
      end
    end
  end
  errs
end

# 路径解析：绝对路径直接用；相对路径解析后必须仍在 base 内（防 ../ 逃逸）
def resolve_path(p, base_dir)
  return nil if p.nil? || p.to_s.strip.empty?
  return File.expand_path(p) if p.start_with?('/')
  return File.expand_path(p) if base_dir.nil?
  base = File.expand_path(base_dir)
  full = File.expand_path(File.join(base, p))
  return nil unless full == base || full.start_with?(base + File::SEPARATOR)
  full
end

# 归一化：把坏值类型收敛成规范形态，供**所有**消费者使用。
# 依据：三模型审查指出"判定逻辑散落、没有先归一化"，实测 render/summary
# 对 stage 坏值抛 TypeError（rb:442/rb:386），且渲染出半截看板。
def normalize_doc(doc)
  return doc unless doc.is_a?(Hash)
  (doc['lines'] || []).each do |l|
    next unless l.is_a?(Hash)
    st = l['stages']
    next unless st.is_a?(Hash)
    st.each_key do |k|
      st[k] = {} unless st[k].is_a?(Hash)
    end
  end
  doc
end

# 并发安全写入：flock 串行化 + 临时文件原子替换
# 依据：实测并发 8 次 --add-line 丢失 3 条线（读-改-写竞争导致 lost update）
def with_lock(path)
  lock = path + '.lock'
  File.open(lock, File::RDWR | File::CREAT, 0o644) do |f|
    f.flock(File::LOCK_EX)
    begin
      yield
    ensure
      f.flock(File::LOCK_UN)
    end
  end
rescue Errno::ENOENT, Errno::EACCES => e
  warn "LOCK_UNAVAILABLE\t#{e.class}\t无法加锁，拒绝写入（防止数据丢失）"
  exit 1
end

def write_state(path, doc)
  tmp = "#{path}.tmp.#{Process.pid}"
  File.write(tmp, JSON.pretty_generate(doc) + "\n")
  File.rename(tmp, path)   # 原子替换，避免读者看到半截文件
end

def load_state(path)
  normalize_doc(JSON.parse(File.read(path, encoding: 'UTF-8')))
rescue Errno::ENOENT
  warn "MISSING_STATE_FILE #{path}"
  exit 1
rescue JSON::ParserError => e
  warn "STATE_JSON_INVALID #{e.message}"
  exit 1
end

def label(doc, s)
  (doc['stage_labels'] || {})[s] || STAGE_LABELS[s] || s
end

# 摘要：给 AGENTS.md 状态块用。
# 双重上界：条数（limit）+ 单字段字符数（FIELD_MAX）
# 依据：真实数据试跑时 3 条线的长文本使状态块膨胀到 497 字节，
#       长文本场景达 998 字节 —— 只限条数不够，必须同时限字段长度。
# 按**字节**截断（中文每字符 3 字节；按字符计会低估 3 倍，实测 998 字节仍未收敛）
# 控制字符兜底过滤：校验器会拒绝，但注入路径不应依赖校验被调用
def sanitize_text(str)
  str.to_s.each_char.reject { |c| c.ord < 0x20 && c != "\t" && c != "\n" }.join
end

def trunc(str, max_bytes = 48)
  t = sanitize_text(str).gsub(/\s+/, ' ').strip
  return t if t.bytesize <= max_bytes
  # 逐字符累加，保证不切断多字节字符
  out = +''
  t.each_char do |c|
    break if out.bytesize + c.bytesize + '…'.bytesize > max_bytes
    out << c
  end
  out + '…'
end

# 摘要：给 AGENTS.md 状态块用。
# 三重上界：条数 + 单字段字节 + **整块字节硬上限**
# 依据：真实数据试跑时 3 条线长文本使状态块达 998 字节；按字符截断对中文无效
#       （中文 3 字节/字符），故改为按字节截断 + 整块收敛。
BLOCK_MAX = 400

def summary(doc, limit = 3)
  labels = doc['stage_labels'] || STAGE_LABELS
  stages = doc['stages'] || STAGES
  ls = doc['lines'] || []
  # 排序键必须含 id 作为次序键：并列 priority 时 sort_by 结果不确定
  # （实测首次返回 A、第二次返回 B），会导致 goal 归属漂移并触发 GOAL_DRIFT
  act  = ls.select { |l| l['state'] == 'active' }.sort_by { |l| [l['priority'].to_i, l['id'].to_s] }
  hold = ls.reject { |l| l['state'] == 'active' }.sort_by { |l| [l['priority'].to_i, l['id'].to_s] }

  # 逐级收敛：先减线数，再减字段宽度，直到整块 ≤ BLOCK_MAX
  [[3, 36, 30], [2, 28, 24], [1, 22, 18]].each do |lim, tw, nw|
    out = build_summary(doc, labels, stages, act, hold, lim, tw, nw)
    txt = out.join("\n")
    return txt if txt.bytesize <= BLOCK_MAX
  end
  # 最后兜底：只留标题 + 计数 + 指引
  ["工作流状态（生成，勿手改）: 共 #{ls.size} 条线（active #{act.size} / 搁置 #{hold.size}）",
   "详见 docs/workflow/WORKFLOW.md"].join("\n")
end

def build_summary(doc, labels, stages, act, hold, limit, title_w, next_w)
  out = []
  out << "工作流状态（生成，勿手改）: 共 #{(doc['lines'] || []).size} 条线（active #{act.size} / 搁置 #{hold.size}）"
  g = doc['goal']
  if g && g['objective']
    out << "本轮目标: #{trunc(g['objective'], 60)}"
  end
  act.first(limit).each do |l|
    done = stages.count { |s| ((l['stages'] || {})[s] || {})['status'] == 'done' }
    cs = l['current_stage']
    cur = cs ? (labels[cs] || cs) : '—'
    out << "- P#{l['priority']} #{trunc(l['title'], title_w)}: #{done}/#{stages.size} 阶段，当前=#{trunc(cur, 12)}，下一步=#{trunc(l['next_action'] || '—', next_w)}"
  end
  out << "- …另有 #{act.size - limit} 条 active（见 WORKFLOW.md）" if act.size > limit
  hold.first(limit).each { |l| out << "- [#{l['state']}] #{trunc(l['title'], title_w)}: #{trunc(l['hold_reason'], next_w)}" }
  out << "- …另有 #{hold.size - limit} 条搁置（见 WORKFLOW.md）" if hold.size > limit
  out << '详见 docs/workflow/WORKFLOW.md'
  out
end

# 自动推导 goal：从状态推出「当前应该聚焦什么」
# 依据：多线并行时目标易漂移；把「本轮目标」机械地从最高优先度 active 线推导出来，
#       并使 goal 与线的对应关系可被校验（goal 指向不存在的线 = 漂移信号）。
def derive_goal(doc)
  stages = doc['stages'] || STAGES
  labels = doc['stage_labels'] || STAGE_LABELS
  ls = doc['lines'] || []
  # (priority, id) 稳定序：并列时以 id 决出确定次序（见 summary 处说明）
  act = ls.select { |l| l['state'] == 'active' }.sort_by { |l| [l['priority'].to_i, l['id'].to_s] }
  return nil if act.empty?
  l = act.first
  cs = l['current_stage']
  stage_name = cs ? (labels[cs] || cs) : '—'
  done = stages.count { |s| ((l['stages'] || {})[s] || {})['status'] == 'done' }
  {
    'objective' => "推进【#{l['title']}】到「#{stage_name}」阶段完成"                    "（当前 #{done}/#{stages.size} 阶段）；下一步：#{l['next_action'] || '待定'}",
    'line' => l['id'],
    'stage' => cs,
    'derived_at' => doc['updated_at'],
    'hold_note' => act.size > 1 ? "另有 #{act.size - 1} 条 active 线在排队" : nil
  }.compact
end

# 写入范围对比：声明的 scope vs git 实际改动
# 依据：DESIGN-FINAL 第 30/207 行声明了"任务的写入范围"，但此前无任何机械检查 ——
# 这是最强的防偏移信号（"我说要做 X，实际改了 Y"），且 git 已提供判据。
def check_scope(doc, changed_files)
  lines = doc['lines'] || []
  out = []
  lines.each do |l|
    # done 线的改动同样是本工作流所为 —— 实测排除 done 会导致已声明路径被误判越界
    next unless %w[active paused done].include?(l['state'])
    scope = l['write_scope']
    if scope.nil? || !scope.is_a?(Array) || scope.empty?
      out << { 'line' => l['id'], 'code' => 'SCOPE_UNDECLARED', 'detail' => '未声明 write_scope' }
      next
    end
    sc = scope.map { |x| x.to_s }
    hit = changed_files.select { |f| sc.any? { |s| f == s || f.start_with?(s.chomp('/') + '/') } }
    # 1) 声明范围内有实际改动 → 正常
    # 2) 声明了但一个都没动 → 可能漂移/漏做（提示，不一定是错）
    out << { 'line' => l['id'], 'code' => 'SCOPE_UNUSED', 'detail' => "声明 #{sc.size} 项但无实际改动" } if hit.empty?
  end
  out
end

# 未被任何 active 线声明、却被实际改动的文件 = 越界
def scope_violations(doc, changed_files)
  lines = (doc['lines'] || []).select { |l| %w[active paused done].include?(l['state']) }
  declared = lines.flat_map { |l| (l['write_scope'] || []).map { |x| x.to_s } }
  return [] if declared.empty?
  # 顶层 scope_exclude：与本工作流无关、已知存在的改动（如用户未提交文件）
  # 依据：实测把用户原有 13 个未提交改动误判为越界 —— 判据必须能排除"非我所为"
  excl = (doc['scope_exclude'] || []).map { |x| x.to_s }
  files = changed_files.reject do |f|
    excl.any? { |e| f == e || f.start_with?(e.chomp('/') + '/') }
  end
  files.reject do |f|
    declared.any? { |s| f == s || f.start_with?(s.chomp('/') + '/') }
  end
end

# 渲染：全阶段必显示（不变量）
def render(doc)
  labels = doc['stage_labels'] || STAGE_LABELS
  stages = doc['stages'] || STAGES
  ls = (doc['lines'] || []).sort_by { |l| [l['priority'].to_i, l['id'].to_s] }
  o = []
  o << "# #{doc['project']} 工作流状态（生成文件，勿手改）"
  o << ""
  o << "更新时间: #{doc['updated_at']}"
  o << ""
  o << "阶段: " + stages.each_with_index.map { |s, i| "#{i + 1}.#{labels[s] || s}" }.join(' → ')
  o << ""
  g = doc['goal']
  if g.is_a?(Hash) && g['objective']
    o << "本轮目标: #{g['objective']}"
    o << ""
  end
  o << "| 优先 | 线 | 状态 | 合并 | " + stages.map { |s| labels[s] || s }.join(' | ') + " | 当前 | 下一步 |"
  o << "|---|---|---|---" + "|---" * stages.size + "|---|---|"
  ls.each do |l|
    cells = stages.map do |s|
      v = (l['stages'] || {})[s]
      v.nil? ? '?' : (STAGE_MARK[v['status']] || '?')
    end
    st = l['hold_reason'] && l['state'] != 'active' ? "#{l['state']}(#{l['hold_reason']})" : l['state']
    cs = l['current_stage']
    cur = cs ? (labels[cs] || cs) : '—'
    o << "| #{l['priority']} | #{l['title']} | #{st} | #{l['merge_state']} | " + cells.join(' | ') +
         " | #{cur} | #{l['next_action'] || '—'} |"
  end
  o << ""
  o << "图例: x=done >=active .=pending !=blocked -=skipped ?=缺失(违规)"
  o.join("\n")
end


# 注入：只重写标记块内，块外逐字节保留；内容未变返回 2（幂等）
BEGIN_MARK = '<!-- WORKFLOW-STATE:BEGIN -->'
END_MARK   = '<!-- WORKFLOW-STATE:END -->'

def inject_block(doc, block)
  # 防注入：块内容含标记串会导致 index 命中假标记 → 每次注入无界膨胀（实测 +106/次）
  if block.include?(BEGIN_MARK) || block.include?(END_MARK)
    warn "BLOCK_CONTAINS_MARKER 摘要块含标记串，拒绝注入（会造成无界膨胀）"
    return 1
  end
  text = File.read(doc, encoding: 'UTF-8')
  b = text.index(BEGIN_MARK)
  e = text.index(END_MARK)
  if b.nil? || e.nil?
    warn "MISSING_MARKER\t#{doc}\t需要 #{BEGIN_MARK} 与 #{END_MARK}"
    return 1
  end
  if e < b
    warn "MARKER_ORDER\t#{doc}"
    return 1
  end
  head = text[0...(b + BEGIN_MARK.length)]
  tail = text[e..]
  new = head + "\n" + block.chomp + "\n" + tail
  return 2 if new == text
  File.write(doc, new)
  0
end

if __FILE__ == $PROGRAM_NAME
  mode = ARGV[0]
  path = ARGV[1]
  # base 必须在 lambda 之前定义：lambda 闭包只捕获定义时已存在的局部变量
  base = ARGV[2] if ARGV[2] && !ARGV[2].to_s.start_with?('--')

  main_dispatch = lambda do
  case mode
  when '--check'
    doc = load_state(path)
    errs = validate(doc, base || File.dirname(File.expand_path(path)))
    if errs.empty?
      puts "OK\tlines=#{(doc['lines'] || []).size}\tstages=#{(doc['stages'] || STAGES).size}"
      exit 0
    else
      errs.each { |e| warn e }
      warn "FAILED\t#{errs.size}"
      exit 1
    end
  when '--mutate'
    # 供验收套件造违规 fixture（不修改原文件）
    mode, src, dst = ARGV[1], ARGV[2], ARGV[3]
    d = JSON.parse(File.read(src, encoding: 'UTF-8'))
    l0 = d['lines'][0]
    st = l0['stages']
    case mode
    when 'remove_stage'          then st.delete('review')
    when 'out_of_order'          then st['implement'] = { 'status' => 'done', 'artifact' => 'x', 'verdict' => 'PASS', 'evidence' => ['a.md'], 'uncovered' => [] }
    when 'a1_contradiction'      then st['design-draft']['verdict'] = 'PASS'; st['design-draft']['uncovered'] = ['未覆盖项']
    when 'partial_no_unc'        then st['design-draft']['uncovered'] = []
    when 'done_no_verdict'       then st['intake']['verdict'] = nil
    when 'done_no_evidence'      then st['intake']['evidence'] = []
    when 'hold_no_reason'        then l0['state'] = 'paused'; l0['hold_reason'] = nil
    when 'missing_merge_state'   then l0.delete('merge_state')
    when 'active_not_merged'     then l0['merge_state'] = 'not-merged'; l0['hold_reason'] = nil
    when 'long_text'
      l0['title'] = '很长的线名称' * 8
      l0['next_action'] = '这是一个非常长的下一步动作描述' * 8
    when 'many_long'
      d['lines'] = (1..40).map { |i| l = Marshal.load(Marshal.dump(l0)); l['id'] = "ml#{i}"; l['priority'] = i; l['title'] = ('很长的线名称' * 8) + i.to_s; l['next_action'] = '非常长的下一步描述' * 8; l }
    when 'with_goal'             then d['goal'] = nil
    when 'goal_drift'
      l1 = Marshal.load(Marshal.dump(l0)); l1['id'] = 'other'; l1['priority'] = 1; l1['title'] = '更高优先度线'; l0['priority'] = 5
      d['lines'] = [l1, l0]
      d['goal'] = { 'objective' => 'x', 'line' => l0['id'], 'stage' => l0['current_stage'] }
    when 'goal_missing_line'     then d['goal'] = { 'objective' => 'x', 'line' => 'ghost', 'stage' => 'intake' }
    when 'stage_null'            then st['review'] = nil
    when 'stage_array'           then st['review'] = []
    when 'stage_string'          then st['review'] = 'pending'
    when 'dup_id'                then d['lines'] << Marshal.load(Marshal.dump(l0))
    when 'verdict_fail_done'     then st['intake']['verdict'] = 'FAIL'
    when 'no_stages_top'         then d.delete('stages')
    when 'priority_str'          then l0['priority'] = 'high'
    when 'priority_neg'          then l0['priority'] = -5
    when 'skipped_no_reason'     then st['design-final'] = { 'status' => 'skipped' }
    when 'verdict_on_unfinished' then st['tasks'] = { 'status' => 'pending', 'verdict' => 'PASS', 'evidence' => ['a.md'], 'uncovered' => [] }
    when 'rec_missing'           then d['recommendation'] = { 'line' => 'ghost', 'reason' => 'x' }
    when 'legal_skipped'         then st['design-final'] = { 'status' => 'skipped', 'skip_reason' => '本项目不适用' }
    when 'marker_inject'         then l0['title'] = '<!-- WORKFLOW-STATE:END -->'
    when 'top_stages_truncated'  then d['stages'] = ['intake', 'design-draft']
    when 'done_no_hold'
      # done 是终态：必须所有阶段都 done/skipped 才合法（否则命中 DONE_BUT_STAGES）
      l0['state'] = 'done'; l0['current_stage'] = nil; l0['hold_reason'] = nil
      %w[intake design-draft prototype design-final tasks implement review].each do |k|
        st[k] = { 'status' => 'done', 'artifact' => 'a.md', 'verdict' => 'PASS', 'evidence' => ['a.md'], 'uncovered' => [] }
      end
    when 'uncovered_string'      then st['design-draft']['uncovered'] = '未覆盖X'
    when 'uncovered_blank'       then st['design-draft']['uncovered'] = ['   ']
    when 'evidence_escape'       then st['intake']['evidence'] = ['../../etc/hosts']
    when 'evidence_dir'          then st['intake']['evidence'] = ['.']
    when 'artifact_ghost'        then st['intake']['artifact'] = 'does-not-exist.md'
    when 'marker_in_field'       then l0['next_action'] = '见 <!-- WORKFLOW-STATE:BEGIN --> 块'
    when 'blocked_with_verdict'  then st['prototype'] = { 'status' => 'blocked', 'verdict' => 'PASS', 'evidence' => ['a.md'], 'uncovered' => [] }; l0['current_stage'] = nil
    when 'skipped_with_uncovered' then st['design-final'] = { 'status' => 'skipped', 'skip_reason' => 'r', 'uncovered' => ['x'] }
    when 'skipped_then_done'
      st['intake'] = { 'status' => 'skipped', 'skip_reason' => '无需访谈' }
      %w[design-draft prototype design-final tasks implement review].each do |k|
        st[k] = { 'status' => 'done', 'artifact' => 'a.md', 'verdict' => 'PASS', 'evidence' => ['a.md'], 'uncovered' => [] }
      end
      l0['current_stage'] = nil; l0['state'] = 'done'
    when 'done_but_stages_pending'
      l0['state'] = 'done'; l0['current_stage'] = nil
      st.each_key { |k| st[k] = { 'status' => 'pending' } }
    when 'abandoned_but_active'  then l0['merge_state'] = 'abandoned'; l0['state'] = 'active'
    when 'goal_no_line'          then d['goal'] = { 'objective' => 'x' }
    when 'goal_stage_mismatch'   then d['goal'] = { 'objective' => 'x', 'line' => l0['id'], 'stage' => 'review' }
    when 'labels_partial'        then d['stage_labels'] = { 'intake' => '信息采集' }
    when 'artifact_dir_ok'       then st['intake']['artifact'] = 'artifacts_dir'
    when 'questions_dir_ok'      then st['design-draft']['questions'] = 'questions_dir'
    when 'evidence_is_dir'       then st['intake']['evidence'] = ['artifacts_dir']
    when 'fresh_line'
      l = { 'id' => 'main', 'title' => '新线', 'priority' => 1, 'state' => 'active',
            'hold_reason' => nil, 'merge_state' => 'merged', 'current_stage' => 'intake',
            'next_action' => '开始',
            'stages' => Hash[STAGES.map { |x| [x, { 'status' => x == 'intake' ? 'active' : 'pending' }] }] }
      d['lines'] = [l]; d.delete('goal')
    when 'tie_priorities'
      # 两条 active 线 priority 相同 —— 曾因 sort_by 不稳定导致 goal 漂移
      l2 = Marshal.load(Marshal.dump(l0))
      l2['id'] = 'zzz-tie'; l2['title'] = '并列线'
      l0['priority'] = 1; l2['priority'] = 1
      d['lines'] = [l0, l2]; d['goal'] = nil
    when 'freshness_reset' then d['coverage'] = nil
    when 'reopenable'
      # 造一条 intake/design-draft/prototype 已 done 的线，供 --reopen 测试
      %w[intake design-draft prototype].each do |k|
        st[k] = { 'status' => 'done', 'artifact' => 'a.md', 'verdict' => 'PASS', 'evidence' => ['a.md'], 'uncovered' => [] }
      end
      st['design-final'] = { 'status' => 'active' }
      l0['current_stage'] = 'design-final'
    when 'control_char'
      l0['title'] = "主线\u0001控制"
      l0['next_action'] = "下一步\u0007" 
    when 'paused_not_merged'     then l0['state'] = 'paused'; l0['merge_state'] = 'not-merged'; l0['hold_reason'] = nil
    when 'zero_lines'            then d['lines'] = []
    when 'many_active'
      d['lines'] = (1..40).map { |i| l = Marshal.load(Marshal.dump(l0)); l['id'] = "a#{i}"; l['priority'] = i; l['title'] = "线#{i}"; l }
    when 'many_hold'
      d['lines'] = (1..40).map { |i| l = Marshal.load(Marshal.dump(l0)); l['id'] = "h#{i}"; l['priority'] = i; l['title'] = "搁置线#{i}"; l['state'] = 'paused'; l['hold_reason'] = "原因#{i}"; l }
    else
      warn "UNKNOWN_MUTATE_MODE #{mode}"; exit 2
    end
    File.write(dst, JSON.pretty_generate(d))
    exit 0
  when '--inject'
    block = File.read(ARGV[2], encoding: 'UTF-8')
    rc = inject_block(path, block)
    puts(rc == 2 ? 'UNCHANGED' : (rc.zero? ? 'UPDATED' : ''))
    exit rc
  when '--goal'
    doc = load_state(path)
    g = derive_goal(doc)
    if g.nil?
      puts 'NO_ACTIVE_LINE'
      exit 0
    end
    require 'json'
    puts JSON.pretty_generate(g)
  when '--set-goal'
    # 自动设置 goal 并写回状态文件（幂等：内容相同不写）
    doc = load_state(path)
    # base 已在 lambda 外定义（ARGV[2]），此处不再重复定义以免覆盖
    # 写入门：违规状态不得写入（否则"先校验后使用"被绕过，实测对违规状态会照写）
    unless ENV['WORKFLOW_SKIP_VALIDATE'] == '1'
      verrs = validate(doc, base || File.dirname(File.expand_path(path)))
      unless verrs.empty?
        warn "STATE_INVALID_REFUSE_WRITE\t#{verrs.size} 项违规，拒绝写入 goal"
        verrs.first(3).each { |e| warn "  #{e}" }
        exit 1
      end
    end
    g = derive_goal(doc)
    if g.nil?
      warn 'NO_ACTIVE_LINE 无可推进的 active 线，未写入 goal'
      exit 1
    end
    if doc['goal'] == g
      puts 'GOAL_UNCHANGED'
      exit 2
    end
    doc['goal'] = g
    write_state(path, doc)
    puts "GOAL_SET\t#{g['line']}\t#{g['stage']}"
    exit 0
  when '--advance'
    # 推进一个阶段：把某条线的某阶段标记为完成并携带三件套
    # 用法: --advance <state.json> <line-id> <stage> <verdict> <artifact> <evidence,逗号分隔> [<uncovered,分号分隔>]
    # 设计原则：降低摩擦，但**不得绕过三件套**——写入后立即校验，不合法则回滚
    doc = load_state(path)
    lid, stage, verdict = ARGV[2], ARGV[3], ARGV[4]
    # base 统一用环境变量（与 --hold/--resume/--skip-stage 一致）
    # 位置参数下 ARGV[8] 极易因省略可选参数而错位；实测冷启动时被静默忽略
    adv_base = ENV['WORKFLOW_BASE']
    adv_base = File.dirname(File.expand_path(path)) if adv_base.nil? || adv_base.to_s.strip.empty?
    artifact = ARGV[5]
    evidence = (ARGV[6] || '').split(',').reject { |x| x.strip.empty? }
    uncovered = (ARGV[7] || '').split(';').reject { |x| x.strip.empty? }
    # 中文经 shell 参数进来会是 ASCII-8BIT，写 JSON 时崩
    # （原型阶段已记录约束 C2；不归一化则用户无法用中文写未覆盖项）
    utf8 = lambda do |x|
      next x if x.nil?
      x.to_s.dup.force_encoding('UTF-8')
    end
    artifact = utf8.call(artifact)
    evidence = evidence.map { |x| utf8.call(x) }
    uncovered = uncovered.map { |x| utf8.call(x) }
    abort_usage = lambda do
      warn 'usage: --advance <state.json> <line-id> <stage> <PASS|PARTIAL|FAIL> <artifact> <ev1,ev2> [<unc1;unc2>]'
      exit 2
    end
    abort_usage.call if [lid, stage, verdict].any? { |x| x.nil? || x.empty? }
    unless OK_VERDICT.include?(verdict)
      warn "BAD_VERDICT_ARG\t#{verdict}\t允许: #{OK_VERDICT.join('|')}"
      exit 2
    end
    line = (doc['lines'] || []).find { |l| l['id'] == lid }
    unless line
      warn "NO_SUCH_LINE\t#{lid}"
      exit 1
    end
    unless STAGES.include?(stage)
      warn "NO_SUCH_STAGE\t#{stage}"
      exit 1
    end
    # 门禁：前置阶段必须都 done/skipped（不可跳阶段）
    idx = STAGES.index(stage)
    blocked_by = STAGES[0...idx].reject { |x| %w[done skipped].include?((line['stages'][x] || {})['status']) }
    unless blocked_by.empty?
      warn "OUT_OF_ORDER\t#{stage}\t前置未完成: #{blocked_by.join(',')}"
      exit 1
    end
    before = Marshal.load(Marshal.dump(line['stages'][stage]))
    line['stages'][stage] = {
      'status' => 'done', 'artifact' => artifact, 'verdict' => verdict,
      'evidence' => evidence, 'uncovered' => uncovered
    }
    doc['updated_at'] = Time.now.strftime('%Y-%m-%dT%H:%M:%S%z')
    # 自动收尾（可审计，不是弱化约束）：完成最后阶段时，线的指针与状态必须同步收敛
    notes = []
    all_done = STAGES.all? { |x| %w[done skipped].include?((line['stages'][x] || {})['status']) }
    if all_done
      if line['current_stage']
        line['current_stage'] = nil
        notes << 'current_stage_cleared'
      end
      if line['state'] == 'active'
        line['state'] = 'done'
        notes << 'line_state_done'
      end
    else
      # 未完成：把 current_stage 指向下一个未完成阶段（保持指向 active 阶段的自洽）
      nxt = STAGES.find { |x| !%w[done skipped].include?((line['stages'][x] || {})['status']) }
      if nxt
        line['stages'][nxt]['status'] = 'active' if (line['stages'][nxt] || {})['status'] == 'pending'
        line['current_stage'] = nxt if line['state'] == 'active'
        notes << "current_stage_advanced_to_#{nxt}"
      end
    end
    # goal 重新推导：线完成/切换后，旧 goal 会指向非 active 线
    # （实测：完成最后阶段后 GOAL_POINTS_TO_INACTIVE_LINE + GOAL_STAGE_MISMATCH）
    g = derive_goal(doc)
    if g.nil?
      if doc['goal']
        doc.delete('goal')
        notes << 'goal_cleared'
      end
      # 无线可推进时显式提示（否则用户面对空 goal 不知为何）
      act = (doc['lines'] || []).count { |l| l['state'] == 'active' }
      if act.zero?
        notes << "no_active_line_remaining(#{ (doc['lines'] || []).count { |l| l['state'] != 'done' } }条未完成线均为搁置态)"
      end
    else
      doc['goal'] = g
      notes << 'goal_rederived'
    end
    # 写后必校验：不合法则回滚（防止用便捷命令绕过三件套）
    verrs = validate(doc, adv_base)
    unless verrs.empty?
      line['stages'][stage] = before
      warn "ADVANCE_REJECTED\t#{verrs.size} 项违规，已回滚"
      verrs.first(5).each { |e| warn "  #{e}" }
      exit 1
    end
    write_state(path, doc)
    puts "ADVANCED\t#{lid}\t#{stage}\t#{verdict}\t#{notes.join(',')}"
    exit 0
  when '--add-line'
    # 新建一条线：7 阶段全 pending。用途：多开发方向并行（用户明确需求）
    lid, title = ARGV[2], ARGV[3]
    if lid.nil? || lid.to_s.strip.empty? || title.nil? || title.to_s.strip.empty?
      warn 'usage: --add-line <state.json> <line-id> <title> [priority] [<base-dir>]'
      exit 2
    end
    prio = (ARGV[4] || '99').to_i
    add_base = ENV['WORKFLOW_BASE'] || File.dirname(File.expand_path(path))
    doc = load_state(path)
    if (doc['lines'] || []).any? { |l| l['id'] == lid }
      warn "LINE_ALREADY_EXISTS\t#{lid}"
      exit 1
    end
    doc['lines'] ||= []
    doc['lines'] << {
      'id' => lid, 'title' => title.dup.force_encoding('UTF-8'), 'priority' => prio,
      'state' => 'active', 'hold_reason' => nil, 'merge_state' => 'not-merged',
      'current_stage' => 'intake', 'next_action' => '开始信息采集',
      'stages' => Hash[STAGES.map { |x| [x, { 'status' => x == 'intake' ? 'active' : 'pending' }] }]
    }
    doc['updated_at'] = Time.now.strftime('%Y-%m-%dT%H:%M:%S%z')
    g = derive_goal(doc)
    doc['goal'] = g if g
    verrs = validate(doc, add_base)
    unless verrs.empty?
      warn "ADD_LINE_REJECTED\t#{verrs.size} 项违规，未写入"
      verrs.first(5).each { |e| warn "  #{e}" }
      exit 1
    end
    write_state(path, doc)
    puts "LINE_ADDED\t#{lid}\t#{prio}"
    exit 0
  when '--hold'
    # 搁置/推迟一条线：必须给原因（校验器强制 HOLD_WITHOUT_REASON）
    lid, kind, reason = ARGV[2], ARGV[3], ARGV[4]
    unless %w[paused deferred].include?(kind)
      warn "BAD_HOLD_KIND\t#{kind}\t允许: paused|deferred"
      exit 2
    end
    if reason.nil? || reason.to_s.strip.empty?
      warn 'MISSING_HOLD_REASON\t搁置必须说明原因'
      exit 2
    end
    # base 用环境变量而非位置参数：位置参数下省略 reason 会让 base 顶上，静默误用
    hold_base = ENV['WORKFLOW_BASE'] || File.dirname(File.expand_path(path))
    doc = load_state(path)
    line = (doc['lines'] || []).find { |l| l['id'] == lid }
    unless line
      warn "NO_SUCH_LINE\t#{lid}"
      exit 1
    end
    before = Marshal.load(Marshal.dump(line))
    line['state'] = kind
    line['hold_reason'] = reason.dup.force_encoding('UTF-8')
    # 搁置时不再指向 active 阶段（否则 CURRENT_STAGE_NOT_ACTIVE）
    line['current_stage'] = nil
    doc['updated_at'] = Time.now.strftime('%Y-%m-%dT%H:%M:%S%z')
    g = derive_goal(doc)
    g.nil? ? doc.delete('goal') : doc['goal'] = g
    verrs = validate(doc, hold_base)
    unless verrs.empty?
      (doc['lines'] || [])[(doc['lines'] || []).index { |l| l['id'] == lid }] = before
      warn "HOLD_REJECTED\t#{verrs.size} 项违规，已回滚"
      verrs.first(5).each { |e| warn "  #{e}" }
      exit 1
    end
    write_state(path, doc)
    puts "LINE_HELD\t#{lid}\t#{kind}\t#{line['hold_reason']}"
    exit 0
  when '--resume'
    # 恢复一条线：state→active，current_stage 指向第一个未完成阶段
    lid = ARGV[2]
    resume_base = ENV['WORKFLOW_BASE'] || File.dirname(File.expand_path(path))
    doc = load_state(path)
    line = (doc['lines'] || []).find { |l| l['id'] == lid }
    unless line
      warn "NO_SUCH_LINE\t#{lid}"
      exit 1
    end
    before = Marshal.load(Marshal.dump(line))
    nxt = STAGES.find { |x| !%w[done skipped].include?((line['stages'][x] || {})['status']) }
    if nxt.nil?
      warn "NOTHING_TO_RESUME\t#{lid}\t所有阶段已完成"
      exit 1
    end
    line['state'] = 'active'
    line['hold_reason'] = nil
    line['stages'][nxt]['status'] = 'active' if (line['stages'][nxt] || {})['status'] == 'pending'
    line['current_stage'] = nxt
    doc['updated_at'] = Time.now.strftime('%Y-%m-%dT%H:%M:%S%z')
    g = derive_goal(doc)
    g.nil? ? doc.delete('goal') : doc['goal'] = g
    verrs = validate(doc, resume_base)
    unless verrs.empty?
      (doc['lines'] || [])[(doc['lines'] || []).index { |l| l['id'] == lid }] = before
      warn "RESUME_REJECTED\t#{verrs.size} 项违规，已回滚"
      verrs.first(5).each { |e| warn "  #{e}" }
      exit 1
    end
    write_state(path, doc)
    puts "LINE_RESUMED\t#{lid}\t#{nxt}"
    exit 0
  when '--reopen'
    # 重新打开已完成/跳过的阶段：真实开发中"完成"被重开是常态
    # （审查发现问题、需求变更、后续迭代）。缺此能力会导致状态失真：
    # 实测 dev-workflow 线标 done 后又迭代 6 次，状态却仍说"已完成"。
    # 用法: --reopen <state.json> <line-id> <stage> <reason>
    lid, stage, reason = ARGV[2], ARGV[3], ARGV[4]
    unless STAGES.include?(stage)
      warn "NO_SUCH_STAGE\t#{stage}"
      exit 1
    end
    if reason.nil? || reason.to_s.strip.empty?
      warn 'MISSING_REOPEN_REASON\t重开阶段必须说明原因'
      exit 2
    end
    re_base = ENV['WORKFLOW_BASE'] || File.dirname(File.expand_path(path))
    doc = load_state(path)
    line = (doc['lines'] || []).find { |l| l['id'] == lid }
    unless line
      warn "NO_SUCH_LINE\t#{lid}"
      exit 1
    end
    cur = (line['stages'][stage] || {})['status']
    unless %w[done skipped].include?(cur)
      warn "NOT_REOPENABLE\t#{lid}\t#{stage}\t当前状态=#{cur.inspect}（仅 done/skipped 可重开）"
      exit 1
    end
    before = Marshal.load(Marshal.dump(line))
    # 重开该阶段，并把其后的阶段退回 pending（后续工作需重新经过）
    line['stages'][stage] = { 'status' => 'active', 'reopened_reason' => reason.dup.force_encoding('UTF-8') }
    idx = STAGES.index(stage)
    STAGES[(idx + 1)..-1].each do |later|
      line['stages'][later] = { 'status' => 'pending' } if %w[done skipped].include?((line['stages'][later] || {})['status'])
    end
    line['current_stage'] = stage
    line['state'] = 'active'
    line['hold_reason'] = nil
    doc['updated_at'] = Time.now.strftime('%Y-%m-%dT%H:%M:%S%z')
    g = derive_goal(doc)
    g.nil? ? doc.delete('goal') : doc['goal'] = g
    verrs = validate(doc, re_base)
    unless verrs.empty?
      (doc['lines'] || [])[(doc['lines'] || []).index { |l| l['id'] == lid }] = before
      warn "REOPEN_REJECTED\t#{verrs.size} 项违规，已回滚"
      verrs.first(5).each { |e| warn "  #{e}" }
      exit 1
    end
    File.write(path, JSON.pretty_generate(doc) + "\n")
    puts "REOPENED\t#{lid}\t#{stage}\t#{reason}"
    exit 0
  when '--skip-stage'
    # 合法跳过某阶段：必须给原因（校验器强制 SKIPPED_WITHOUT_REASON）
    lid, stage, reason = ARGV[2], ARGV[3], ARGV[4]
    unless STAGES.include?(stage)
      warn "NO_SUCH_STAGE\t#{stage}"
      exit 1
    end
    if reason.nil? || reason.to_s.strip.empty?
      warn 'MISSING_SKIP_REASON\t跳过阶段必须说明原因'
      exit 2
    end
    skip_base = ENV['WORKFLOW_BASE'] || File.dirname(File.expand_path(path))
    doc = load_state(path)
    line = (doc['lines'] || []).find { |l| l['id'] == lid }
    unless line
      warn "NO_SUCH_LINE\t#{lid}"
      exit 1
    end
    before = Marshal.load(Marshal.dump(line))
    line['stages'][stage] = { 'status' => 'skipped', 'skip_reason' => reason.dup.force_encoding('UTF-8') }
    doc['updated_at'] = Time.now.strftime('%Y-%m-%dT%H:%M:%S%z')
    # 若跳过的正是当前阶段，指针前移
    if line['current_stage'] == stage
      nxt = STAGES.find { |x| !%w[done skipped].include?((line['stages'][x] || {})['status']) }
      line['current_stage'] = nxt
      line['stages'][nxt]['status'] = 'active' if nxt && (line['stages'][nxt] || {})['status'] == 'pending'
    end
    g = derive_goal(doc)
    g.nil? ? doc.delete('goal') : doc['goal'] = g
    verrs = validate(doc, skip_base)
    unless verrs.empty?
      (doc['lines'] || [])[(doc['lines'] || []).index { |l| l['id'] == lid }] = before
      warn "SKIP_REJECTED\t#{verrs.size} 项违规，已回滚"
      verrs.first(5).each { |e| warn "  #{e}" }
      exit 1
    end
    write_state(path, doc)
    puts "STAGE_SKIPPED\t#{lid}\t#{stage}"
    exit 0
  when '--scope-check'
    # 用法: --scope-check <state.json> <git-base> [<repo-dir>]
    base_rev = ARGV[2] || 'HEAD'
    repo = ARGV[3] || Dir.pwd
    doc = load_state(path)
    # 若线里记了 baseline，优先用它（避免把历史无关改动算作本次越界）
    bl = (doc['lines'] || []).map { |l| l['baseline'] }.compact.first
    base_rev = bl if bl && (ARGV[2].nil? || ARGV[2] == 'HEAD')
    # 基线必须可解析，否则范围检查静默失效（fail-open）
    `git -C #{repo} rev-parse --verify --quiet #{base_rev} 2>/dev/null`
    if $?.exitstatus != 0
      warn "SCOPE_BASE_INVALID\t#{base_rev}\t不是有效的 git 修订，无法比较"
      exit 1
    end
    # 已跟踪的改动：git diff 能给出
    tracked = `git -C #{repo} diff --name-only #{base_rev} 2>/dev/null`.split("\n")
    # 未跟踪文件：git diff 不含 untracked（曾漏检"新建的越界文件"=假阴性），
    # 但直接全收又会把仓库中早已存在的未跟踪文件算成越界（实测 142 个误报）。
    # 判据：未跟踪文件的 mtime 晚于基线提交时间 ⇒ 本次新增。
    base_ts = `git -C #{repo} log -1 --format=%ct #{base_rev} 2>/dev/null`.strip
    untracked_all = `git -C #{repo} ls-files --others --exclude-standard 2>/dev/null`.split("\n").reject(&:empty?)
    untracked_new = if base_ts.empty?
      []   # 无法判定时间 ⇒ 不把未跟踪算作越界（避免误报淹没信号）
    else
      untracked_all.select do |f|
        full = File.join(repo, f)
        File.exist?(full) && File.mtime(full).to_i > base_ts.to_i
      end
    end
    changed = (tracked + untracked_new).reject(&:empty?).uniq
    if changed.empty?
      puts "SCOPE_OK\t无实际改动（base=#{base_rev}）"
      exit 0
    end
    viol = scope_violations(doc, changed)
    notes = check_scope(doc, changed)
    if viol.empty?
      puts "SCOPE_OK\t改动 #{changed.size} 个文件，均在声明范围内（base=#{base_rev}）"
    else
      warn "SCOPE_VIOLATION\t#{viol.size} 个文件超出所有 active 线的声明范围："
      viol.each { |f| warn "  #{f}" }
    end
    notes.each { |n| puts "SCOPE_NOTE\t#{n['line']}\t#{n['code']}\t#{n['detail']}" }
    exit(viol.empty? ? 0 : 1)
  when '--coverage-check'
    # 零静默遗漏：来源清单每一项在 state.coverage 里必须有明确去向
    # 依据用户原始痛点："只吸收了能使用，其他功能一律都没有"（少做且无痕）
    # 与仓库先例对齐：docs/superpowers/specs/2026-07-15-claude-closure-*.md 的 zero_loss_rule
    inv = ARGV[2]
    if inv.nil? || !File.exist?(inv)
      warn "MISSING_INVENTORY\t#{inv}"
      exit 1
    end
    items = File.readlines(inv, encoding: 'UTF-8')
                .map { |l| l.strip }
                .reject { |l| l.empty? || l.start_with?('#') }
    if items.empty?
      warn "EMPTY_INVENTORY\t#{inv}\t清单为空，无法判定遗漏"
      exit 1
    end
    doc = load_state(path)
    cov = doc['coverage'] || []
    mapped = cov.select { |c| c.is_a?(Hash) }.map { |c| c['item'].to_s }
    unmapped = items.reject { |it| mapped.include?(it) }
    dups = items.select { |it| items.count(it) > 1 }.uniq
    if unmapped.empty?
      puts "COVERAGE_OK\t#{items.size} 项全部有明确去向（覆盖记录 #{cov.size} 条）"
      unless dups.empty?
        puts "COVERAGE_NOTE\t清单含重复项: #{dups.join(', ')}"
      end
      exit 0
    end
    warn "COVERAGE_UNMAPPED\t#{unmapped.size}/#{items.size} 项无去向（被静默遗漏）："
    unmapped.first(20).each { |u| warn "  #{u}" }
    warn "  提示：未采纳的项也必须显式记 disposition=rejected + rationale"
    exit 1
  when '--summary' then puts summary(load_state(path))
  when '--render'  then puts render(load_state(path))
  else
    warn "usage: workflow_state.rb --check|--summary|--render <state.json> [<base-dir>]"
    exit 2
  end
  end   # main_dispatch

  WRITE_MODES = %w[--advance --add-line --hold --resume --skip-stage --set-goal --reopen].freeze
  if WRITE_MODES.include?(mode) && path && File.exist?(path)
    lockf = path + '.lock'
    File.open(lockf, File::RDWR | File::CREAT, 0o644) do |lf|
      lf.flock(File::LOCK_EX)
      main_dispatch.call
    end
  else
    main_dispatch.call
  end
end
