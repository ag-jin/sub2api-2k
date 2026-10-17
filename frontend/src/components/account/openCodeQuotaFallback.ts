import { resolveOpenCodeAccountMode } from './credentialsBuilder'
import type { OpenCodeQuotaFallbackStatus, OpenCodeQuotaFallbackTriggerWindow } from '@/types'

/**
 * OpenCode 额度保护（可选功能；冻结契约见 .scratch/opencode-free-quota/contract.md）：
 *
 * - credentials.quota_fallback = { enabled, threshold_percent(默认98), model_mapping }
 *   key = 正常最终上游模型 ID（model_mapping 的输出/实际 target），value = 管理员手动选择的替代目标 ID。
 * - 只读状态 DTO 由 backend 派生下发（account.opencode_quota_fallback），前端只做展示。
 * - 仅 opencode / opencode_go 的 apikey 账号（Go 模式）适用；Zen 无 5h 窗口，不展示、不启用。
 */

export const OPENCODE_QUOTA_FALLBACK_CREDENTIAL_KEY = 'quota_fallback'
export const DEFAULT_OPENCODE_QUOTA_FALLBACK_THRESHOLD = 98

export interface OpenCodeQuotaFallbackConfig {
  enabled: boolean
  threshold_percent: number
  model_mapping: Record<string, string>
}

export interface OpenCodeQuotaFallbackCandidate {
  id: string
  label?: string
  capability: 'available' | 'unavailable' | 'unknown'
}

export type OpenCodeQuotaFallbackValidationError = 'thresholdInvalid' | 'mappingInvalid'

export function createOpenCodeQuotaFallbackConfig(): OpenCodeQuotaFallbackConfig {
  return {
    enabled: false,
    threshold_percent: DEFAULT_OPENCODE_QUOTA_FALLBACK_THRESHOLD,
    model_mapping: {}
  }
}

function defaultThreshold(): number {
  return DEFAULT_OPENCODE_QUOTA_FALLBACK_THRESHOLD
}

/**
 * 宽松读存储值（脏数据 → 安全缺省，等价关闭）：非对象 → null（账号无该字段）。
 * enabled 仅接受布尔 true；threshold 仅接受有限数字（保留原值供编辑回显与校验），
 * 其余情况取默认 98；model_mapping 仅保留 string→string 且 key 非空（trim）。
 */
export function parseOpenCodeQuotaFallbackConfig(raw: unknown): OpenCodeQuotaFallbackConfig | null {
  if (raw === null || raw === undefined || typeof raw !== 'object' || Array.isArray(raw)) return null
  const source = raw as Record<string, unknown>
  const threshold = typeof source.threshold_percent === 'number' && Number.isFinite(source.threshold_percent)
    ? source.threshold_percent
    : defaultThreshold()
  return {
    enabled: source.enabled === true,
    threshold_percent: threshold,
    model_mapping: parseModelMapping(source.model_mapping)
  }
}

function parseModelMapping(raw: unknown): Record<string, string> {
  const mapping: Record<string, string> = {}
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return mapping
  for (const [rawKey, rawValue] of Object.entries(raw as Record<string, unknown>)) {
    if (typeof rawValue !== 'string') continue
    const key = rawKey.trim()
    if (!key) continue
    mapping[key] = rawValue.trim()
  }
  return mapping
}

/**
 * 功能资格：apikey 且 Go 模式；opencode（legacy，Go 端点）与 opencode_go 同规则，
 * Zen 不因 legacy 豁免（Zen 无 5h 窗口，不展示、不启用、不提交 enabled 策略）。
 * 缺省 mode 按 go（resolveOpenCodeAccountMode 语义）。
 */
export function isOpenCodeQuotaFallbackEligible(
  platform: string,
  type: string,
  mode?: string | null
): boolean {
  if (type !== 'apikey') return false
  if (platform !== 'opencode' && platform !== 'opencode_go') return false
  return resolveOpenCodeAccountMode(mode ?? undefined) === 'go'
}

/**
 * 启用态校验（仅 enabled=true 生效）：
 * - threshold 必须为整数 1..100；
 * - 每条映射的 key/value 必须非空且 key != value（含空占位行在内，任一条不完整/自映射即拒绝，
 *   不静默丢弃坏行）；
 * - 拒绝链式与循环（任一 value 不得同时是另一条的 key，如 A->B + B->C 或 A->B + B->A），
 *   否则 effective 映射会出现多跳/自环歧义。
 */
export function validateOpenCodeQuotaFallbackConfig(
  config: OpenCodeQuotaFallbackConfig
): OpenCodeQuotaFallbackValidationError | null {
  if (!config.enabled) return null
  const threshold = config.threshold_percent
  if (!Number.isInteger(threshold) || threshold < 1 || threshold > 100) return 'thresholdInvalid'
  const entries: Array<[string, string]> = []
  for (const [rawKey, rawValue] of Object.entries(config.model_mapping ?? {})) {
    const key = typeof rawKey === 'string' ? rawKey.trim() : ''
    const value = typeof rawValue === 'string' ? rawValue.trim() : ''
    if (!key || !value || key === value) return 'mappingInvalid'
    entries.push([key, value])
  }
  if (entries.length === 0) return 'mappingInvalid'
  const sources = new Set(entries.map(([key]) => key))
  for (const [, value] of entries) {
    if (sources.has(value)) return 'mappingInvalid'
  }
  return null
}

/**
 * 规范化为持久形态：
 * - completeOnly=true（启用保存）：仅保留 trim 后 key/value 非空且 key != value 的行；
 * - completeOnly=false（关闭保存）：保留 key 非空的行（value 可为空，避免丢失编辑中内容）；
 * - model_mapping 为空的条目被丢弃；threshold 非有限数字时回落默认 98。
 */
export function normalizeOpenCodeQuotaFallbackConfig(
  config: OpenCodeQuotaFallbackConfig,
  opts?: { completeOnly?: boolean }
): OpenCodeQuotaFallbackConfig {
  const completeOnly = opts?.completeOnly === true
  const mapping: Record<string, string> = {}
  for (const [rawKey, rawValue] of Object.entries(config.model_mapping ?? {})) {
    const key = typeof rawKey === 'string' ? rawKey.trim() : ''
    const value = typeof rawValue === 'string' ? rawValue.trim() : ''
    if (!key) continue
    if (completeOnly && (!value || key === value)) continue
    mapping[key] = value
  }
  return {
    enabled: config.enabled === true,
    threshold_percent:
      typeof config.threshold_percent === 'number' && Number.isFinite(config.threshold_percent)
        ? config.threshold_percent
        : defaultThreshold(),
    model_mapping: mapping
  }
}

/**
 * 写入 credentials（就地修改）：
 * - enabled && eligible → 写启用形态（规范化为完整行）；
 * - 否则 → 关闭形态：仅当已存在存量字段或表单已有映射内容时写 enabled:false（保留配置，不删除，
 *   legacy 无字段且无内容时完全不写，避免污染）。
 * eligible=false（Zen/非 OpenCode）强制 enabled:false，防止启用策略泄漏。
 */
export function applyOpenCodeQuotaFallbackConfig(
  credentials: Record<string, unknown>,
  config: OpenCodeQuotaFallbackConfig,
  opts?: { eligible?: boolean }
): void {
  const eligible = opts?.eligible !== false
  const enabled = config.enabled === true && eligible
  const normalized = normalizeOpenCodeQuotaFallbackConfig(
    { ...config, enabled },
    { completeOnly: enabled }
  )
  const hasExisting = credentials[OPENCODE_QUOTA_FALLBACK_CREDENTIAL_KEY] !== undefined
  const hasContent = Object.keys(normalized.model_mapping).length > 0
  if (enabled) {
    credentials[OPENCODE_QUOTA_FALLBACK_CREDENTIAL_KEY] = normalized
    return
  }
  if (hasExisting || hasContent) {
    credentials[OPENCODE_QUOTA_FALLBACK_CREDENTIAL_KEY] = normalized
  }
}

/**
 * 正常 model_mapping 的“真正 targets”去重列表（= 正常最终上游模型 ID），
 * 作为替代映射的 source 候选（modelOptions）。
 */
export function openCodeQuotaFallbackSourceOptions(
  modelMapping: Record<string, string> | null | undefined
): string[] {
  if (!modelMapping || typeof modelMapping !== 'object') return []
  const seen = new Set<string>()
  for (const rawValue of Object.values(modelMapping)) {
    if (typeof rawValue !== 'string') continue
    const value = rawValue.trim()
    if (value) seen.add(value)
  }
  return [...seen]
}

/**
 * 上游目录同步结果 → 候选。
 * 公共 /v1/models 只给出 id/object/created/owned_by：只有 id 不能宣称 capability=available，
 * 因此一律标记 unknown（label 仅在同步结果带 display_name 元数据时使用）。
 * capability=unavailable 仅保留给明确的协议不兼容/标准端点证据，前端不猜测。
 */
export function openCodeQuotaFallbackCandidatesFromSync(
  result: { models?: string[]; metadata?: Record<string, { display_name?: string }> } | null | undefined
): OpenCodeQuotaFallbackCandidate[] {
  if (!result || !Array.isArray(result.models)) return []
  const candidates: OpenCodeQuotaFallbackCandidate[] = []
  const seen = new Set<string>()
  for (const model of result.models) {
    const id = typeof model === 'string' ? model.trim() : ''
    if (!id || seen.has(id)) continue
    seen.add(id)
    const label = result.metadata?.[id]?.display_name?.trim()
    candidates.push(label ? { id, label, capability: 'unknown' } : { id, capability: 'unknown' })
  }
  return candidates
}

/**
 * 合并上一轮候选：本轮目录仍有的以本轮为准；上轮有、本轮缺失的保留原样
 * （未出现在本次目录不代表协议不支持，也不能因此禁止管理员手输目标）。
 */
export function mergeOpenCodeQuotaFallbackCandidates(
  previous: OpenCodeQuotaFallbackCandidate[],
  fetched: OpenCodeQuotaFallbackCandidate[]
): OpenCodeQuotaFallbackCandidate[] {
  const previousById = new Map(previous.map(candidate => [candidate.id, candidate]))
  const merged: OpenCodeQuotaFallbackCandidate[] = fetched.map(candidate => {
    const label = candidate.label ?? previousById.get(candidate.id)?.label
    return label ? { ...candidate, label } : { ...candidate }
  })
  const fetchedIds = new Set(fetched.map(candidate => candidate.id))
  for (const candidate of previous) {
    if (fetchedIds.has(candidate.id)) continue
    merged.push({ ...candidate })
  }
  return merged
}

const STATUS_STATES = new Set(['disabled', 'normal', 'fallback_active', 'unknown'])
const WINDOW_NAMES = new Set(['5h', 'weekly', 'monthly'])

function normalizeTriggeringWindows(raw: unknown): OpenCodeQuotaFallbackTriggerWindow[] {
  if (!Array.isArray(raw)) return []
  const windows: OpenCodeQuotaFallbackTriggerWindow[] = []
  for (const item of raw) {
    if (!item || typeof item !== 'object' || Array.isArray(item)) continue
    const source = item as Record<string, unknown>
    if (typeof source.window !== 'string' || !WINDOW_NAMES.has(source.window)) continue
    const entry: OpenCodeQuotaFallbackTriggerWindow = {
      window: source.window as OpenCodeQuotaFallbackTriggerWindow['window']
    }
    if (typeof source.used_percent === 'number' && Number.isFinite(source.used_percent)) {
      entry.used_percent = source.used_percent
    }
    if (typeof source.resets_at === 'string' && source.resets_at) entry.resets_at = source.resets_at
    windows.push(entry)
  }
  return windows
}

/**
 * 只读状态 DTO → 展示用形态，严格按冻结字段读取（不做别名兼容）。
 * 非对象 → null（不渲染）；未知/缺失 state 一律回落 'unknown'，绝不伪报 normal。
 */
export function normalizeOpenCodeQuotaFallbackStatus(raw: unknown): OpenCodeQuotaFallbackStatus | null {
  if (raw === null || raw === undefined || typeof raw !== 'object' || Array.isArray(raw)) return null
  const source = raw as Record<string, unknown>
  const state = typeof source.state === 'string' && STATUS_STATES.has(source.state)
    ? (source.state as OpenCodeQuotaFallbackStatus['state'])
    : 'unknown'
  const threshold = typeof source.threshold_percent === 'number' &&
    Number.isInteger(source.threshold_percent) &&
    source.threshold_percent >= 1 &&
    source.threshold_percent <= 100
    ? source.threshold_percent
    : defaultThreshold()
  const status: OpenCodeQuotaFallbackStatus = {
    enabled: source.enabled === true,
    state,
    threshold_percent: threshold,
    triggering_windows: normalizeTriggeringWindows(source.triggering_windows)
  }
  if (typeof source.snapshot_at === 'string' && source.snapshot_at) {
    status.snapshot_at = source.snapshot_at
  }
  if (typeof source.reason === 'string') status.reason = source.reason
  return status
}
