import { describe, expect, it } from 'vitest'

import type { AlertEvent } from '@/api/admin/ops'
import {
  ZHIPU_SIGN_EFFECTIVE_RATE_METRIC,
  ZHIPU_SIGN_FAIL_WINDOW_METRIC,
  describeZhipuSignAlert,
  extractSuggestedAction,
  isZhipuSignAlert,
  maskAccountId,
} from '../zhipuSignAlertPresentation'

/**
 * 事件 fixture 按票 25 后端的真实形状构造（见
 * backend/internal/service/ops_alert_evaluator_service.go 的
 * buildOpsAlertDescription / opsAlertEventDimensions，以及 ops_alert_models.go 的
 * 内置指标注册项文案），不是按实现推导出来的。
 */
function makeEvent(overrides: Partial<AlertEvent> = {}): AlertEvent {
  return {
    id: 1,
    rule_id: 42,
    severity: 'P1',
    status: 'firing',
    title: '智谱签名失效（L1 实时窗口）',
    description:
      'zhipu_sign_fail_window > 0.00 (current 2.00) over last 5m (platform=zhipu)；建议动作：检查 X-Client-Version 是否为 0.16.9；若上游持续拒绝签名，先关停全局签名开关（gateway.zhipu.sign_v4_enabled）',
    metric_value: 2,
    threshold_value: 0,
    dimensions: {
      platform: 'zhipu',
      zhipu_sign_accounts: [1024, 77],
      zhipu_sign_window_bucket: 5_879_040,
    },
    fired_at: '2026-10-06T01:00:00Z',
    resolved_at: null,
    email_sent: true,
    created_at: '2026-10-06T01:00:00Z',
    ...overrides,
  }
}

const L2_EVENT: AlertEvent = {
  id: 2,
  rule_id: 43,
  severity: 'P1',
  status: 'resolved',
  title: '智谱签名有效系数偏离（L2 费率对账）',
  description:
    'zhipu_sign_effective_rate > 0.70 (current 0.71) over last 360m (platform=zhipu)；建议动作：检查 X-Client-Version 是否为 0.16.9',
  metric_value: 0.71,
  threshold_value: 0.7,
  dimensions: { platform: 'zhipu' },
  fired_at: '2026-10-06T00:00:00Z',
  resolved_at: '2026-10-06T06:00:00Z',
  email_sent: true,
  created_at: '2026-10-06T00:00:00Z',
}

describe('isZhipuSignAlert', () => {
  it('identifies events by the platform dimension only', () => {
    expect(isZhipuSignAlert(makeEvent())).toBe(true)
    expect(isZhipuSignAlert(makeEvent({ dimensions: { platform: 'ZHIPU' } }))).toBe(true)
    expect(isZhipuSignAlert(makeEvent({ dimensions: { platform: 'openai' } }))).toBe(false)
    expect(isZhipuSignAlert(makeEvent({ dimensions: {} }))).toBe(false)
    expect(isZhipuSignAlert(makeEvent({ dimensions: undefined }))).toBe(false)
    expect(isZhipuSignAlert(null)).toBe(false)
  })
})

describe('describeZhipuSignAlert', () => {
  it('maps the L1 window-count event with masked accounts and the suggested action', () => {
    const presentation = describeZhipuSignAlert(makeEvent())
    expect(presentation).not.toBeNull()
    expect(presentation?.kind).toBe('fail_window')
    expect(presentation?.metricType).toBe(ZHIPU_SIGN_FAIL_WINDOW_METRIC)
    expect(presentation?.value).toBe(2)
    expect(presentation?.threshold).toBe(0)
    expect(presentation?.windowBucket).toBe(5_879_040)
    expect(presentation?.accounts).toEqual([
      { id: 77, masked: '**' },
      { id: 1024, masked: '10**' },
    ])
    // 建议动作取自后端描述（#25 内置注册项文案），且不含任何凭据字样
    expect(presentation?.suggestedAction).toContain('X-Client-Version')
    expect(presentation?.suggestedAction).toContain('gateway.zhipu.sign_v4_enabled')
    for (const forbidden of ['api_key', 'Authorization', 'Bearer', 'sk-']) {
      expect(presentation?.suggestedAction).not.toContain(forbidden)
    }
  })

  it('maps the L2 effective-rate event (no account dimension, resolved status intact)', () => {
    const presentation = describeZhipuSignAlert(L2_EVENT)
    expect(presentation?.kind).toBe('effective_rate')
    expect(presentation?.metricType).toBe(ZHIPU_SIGN_EFFECTIVE_RATE_METRIC)
    expect(presentation?.value).toBeCloseTo(0.71, 10)
    expect(presentation?.threshold).toBeCloseTo(0.7, 10)
    expect(presentation?.accounts).toEqual([])
    expect(presentation?.windowBucket).toBeNull()
    expect(presentation?.suggestedAction).toContain('X-Client-Version')
  })

  it('ignores malformed dimension payloads instead of fabricating accounts', () => {
    const cases: Array<[unknown, Array<{ id: number; masked: string }>]> = [
      [undefined, []],
      [null, []],
      ['oops', []],
      [[0, -3, 'x', null, Number.NaN], []],
      // 重复 id 去重、升序
      [[1024, 1024], [{ id: 1024, masked: '10**' }]],
    ]

    for (const [raw, expectedAccounts] of cases) {
      const presentation = describeZhipuSignAlert(
        makeEvent({ dimensions: { platform: 'zhipu', zhipu_sign_accounts: raw } })
      )
      expect(presentation?.kind).toBe('fail_window')
      expect(`${JSON.stringify(raw)}: ${JSON.stringify(presentation?.accounts)}`).toBe(
        `${JSON.stringify(raw)}: ${JSON.stringify(expectedAccounts)}`
      )
    }
  })

  it('keeps a zhipu event without a recognised metric name out of the sign presentation', () => {
    const other = makeEvent({
      description: 'group_available_ratio < 0.50 (current 0.20) over last 5m (platform=zhipu)',
      dimensions: { platform: 'zhipu' },
    })
    expect(describeZhipuSignAlert(other)).toBeNull()

    // 非智谱平台即使是签名指标也不接管渲染
    expect(
      describeZhipuSignAlert(makeEvent({ dimensions: { platform: 'openai' } }))
    ).toBeNull()
    expect(describeZhipuSignAlert(null)).toBeNull()
  })

  it('tolerates missing metric values without emitting NaN', () => {
    const presentation = describeZhipuSignAlert(
      makeEvent({ metric_value: undefined, threshold_value: undefined })
    )
    expect(presentation?.value).toBeNull()
    expect(presentation?.threshold).toBeNull()
    expect(Number.isNaN(presentation?.value as number)).toBe(false)
  })
})

describe('maskAccountId', () => {
  it('keeps only the leading digits visible', () => {
    expect(maskAccountId(12345)).toBe('12***')
    expect(maskAccountId(1024)).toBe('10**')
    expect(maskAccountId(77)).toBe('**')
    expect(maskAccountId(7)).toBe('*')
    // 脱敏标识不得泄露完整 id
    for (const id of [12345, 1024, 99]) {
      expect(maskAccountId(id)).not.toBe(String(id))
    }
  })
})

describe('extractSuggestedAction', () => {
  it('reads the action text appended by the backend event builder', () => {
    expect(
      extractSuggestedAction('zhipu_sign_fail_window > 0.00 (current 2.00)；建议动作：检查 X-Client-Version')
    ).toBe('检查 X-Client-Version')
    expect(extractSuggestedAction('只有描述，没有建议动作')).toBe('')
    expect(extractSuggestedAction(undefined)).toBe('')
    expect(extractSuggestedAction('')).toBe('')
  })
})
