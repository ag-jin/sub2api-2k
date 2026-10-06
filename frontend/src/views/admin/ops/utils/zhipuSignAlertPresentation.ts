/**
 * 智谱签名告警事件 → 既有 ops 告警行的字段映射（ui-panels §6.2 / §9 的
 * `ZhipuSignAlertPresentation`）。
 *
 * 事件来自票 25 的内置规则（`zhipu_sign_fail_window` / `zhipu_sign_effective_rate`），
 * 事件 DTO 只有 title/description/metric_value/threshold_value/dimensions：
 *   - `dimensions.platform === 'zhipu'` 是「智谱签名相关」的识别口径（票 30 验收项 2）；
 *   - 指标类型名出现在后端事件描述开头（票 25 的 `buildOpsAlertDescription`，
 *     metric_type 是跨票冻结常量），据此区分 L1 / L2；
 *   - L1 事件在 `dimensions` 里带 `zhipu_sign_accounts`（账号 id 数组，最多 20 个）
 *     与 `zhipu_sign_window_bucket`（5 分钟桶）；账号在本面板按脱敏标识展示。
 *
 * 这里不新增通道、不改后端：只是把已有字段整理成告警行需要的展示结构。
 */

import type { AlertEvent } from '@/api/admin/ops'

/** 票 25 冻结的指标类型名（跨票契约，不得改名）。 */
export const ZHIPU_SIGN_FAIL_WINDOW_METRIC = 'zhipu_sign_fail_window'
export const ZHIPU_SIGN_EFFECTIVE_RATE_METRIC = 'zhipu_sign_effective_rate'

/** L1 = 实时失效窗口计数；L2 = 费率对账有效系数。 */
export type ZhipuSignAlertKind = 'fail_window' | 'effective_rate'

export interface ZhipuSignAlertAccount {
  /** 完整账号 id：只作为 `title` 的可访问完整值，正文按脱敏标识展示。 */
  id: number
  /** 脱敏标识（如 `12***`），用于正文。 */
  masked: string
}

export interface ZhipuSignAlertPresentation {
  kind: ZhipuSignAlertKind
  /** 指标类型名（文案里也字面出现，便于对回 #25 的规则）。 */
  metricType: string
  /** L1 窗口计数 / L2 当前有效系数（metric_value；缺失为 null）。 */
  value: number | null
  /** 阈值（threshold_value；缺失为 null）。 */
  threshold: number | null
  /** 5 分钟窗口的桶标识（L1 事件维度，缺失为 null）。 */
  windowBucket: number | null
  /** 涉及账号（脱敏标识；维度里没有时为 []）。 */
  accounts: ZhipuSignAlertAccount[]
  /** 后端描述里附带的建议动作文案（#25 内置注册项；缺失为空串）。 */
  suggestedAction: string
}

function readNumber(value: unknown): number | null {
  return typeof value === 'number' && Number.isFinite(value) ? value : null
}

function readDimension(event: AlertEvent | null | undefined, key: string): unknown {
  return event?.dimensions?.[key]
}

/** 该事件是否属于智谱签名（维度平台 = zhipu）。 */
export function isZhipuSignAlert(event: AlertEvent | null | undefined): boolean {
  return String(readDimension(event, 'platform') ?? '').trim().toLowerCase() === 'zhipu'
}

/**
 * 账号 id 的脱敏标识：保留前两位，其余以 `*` 代替（`12345` → `12***`）。
 * 两位以内的 id 全遮（`7` → `**`）。完整 id 只放 `title`，供需要精确排查时读取。
 */
export function maskAccountId(id: number): string {
  const text = String(id)
  if (text.length <= 2) return '*'.repeat(text.length)
  return `${text.slice(0, 2)}${'*'.repeat(text.length - 2)}`
}

/** 从后端事件描述里取出「建议动作：…」的后半段（无则为空串）。 */
export function extractSuggestedAction(description: string | undefined): string {
  const text = typeof description === 'string' ? description.trim() : ''
  if (!text) return ''
  const match = /建议动作[：:]\s*([\s\S]+)$/.exec(text)
  return match ? match[1].trim() : ''
}

function readAccountIds(event: AlertEvent | null | undefined): number[] {
  const raw = readDimension(event, 'zhipu_sign_accounts')
  if (!Array.isArray(raw)) return []
  const ids: number[] = []
  for (const item of raw) {
    const id = readNumber(item)
    if (id === null || id <= 0) continue
    if (ids.includes(id)) continue
    ids.push(id)
  }
  return ids.sort((a, b) => a - b)
}

/**
 * 把智谱签名事件映射成展示结构；无法归类（平台不是 zhipu，或描述里既无 L1 也无 L2
 * 指标名）时返回 null —— 调用方保持既有渲染，不臆造本地化标题。
 */
export function describeZhipuSignAlert(
  event: AlertEvent | null | undefined
): ZhipuSignAlertPresentation | null {
  if (!isZhipuSignAlert(event)) return null

  const description = typeof event?.description === 'string' ? event.description : ''
  const bucket = readNumber(readDimension(event, 'zhipu_sign_window_bucket'))
  const hasAccountDimension = readAccountIds(event).length > 0

  let kind: ZhipuSignAlertKind | null = null
  if (description.includes(ZHIPU_SIGN_FAIL_WINDOW_METRIC) || bucket !== null || hasAccountDimension) {
    kind = 'fail_window'
  } else if (description.includes(ZHIPU_SIGN_EFFECTIVE_RATE_METRIC)) {
    kind = 'effective_rate'
  }
  if (!kind) return null

  return {
    kind,
    metricType:
      kind === 'fail_window' ? ZHIPU_SIGN_FAIL_WINDOW_METRIC : ZHIPU_SIGN_EFFECTIVE_RATE_METRIC,
    value: readNumber(event?.metric_value),
    threshold: readNumber(event?.threshold_value),
    windowBucket: bucket,
    accounts: readAccountIds(event).map((id) => ({ id, masked: maskAccountId(id) })),
    suggestedAction: extractSuggestedAction(event?.description),
  }
}
