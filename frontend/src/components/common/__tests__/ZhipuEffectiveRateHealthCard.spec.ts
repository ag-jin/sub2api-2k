import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import type { MonitorQuotaSnapshot } from '@/api/admin/channelMonitor'
import enDashboard from '@/i18n/locales/en/dashboard'
import zhDashboard from '@/i18n/locales/zh/dashboard'
import ZhipuEffectiveRateHealthCard from '../ZhipuEffectiveRateHealthCard.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    // 带插值的调用把参数一并透出（`key|{"factor":"0.50"}`），便于断言数值确实传给了 i18n。
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key}|${JSON.stringify(params)}` : key,
      te: () => true,
    }),
  }
})

function makeSnapshot(overrides: Partial<MonitorQuotaSnapshot> = {}): MonitorQuotaSnapshot {
  return {
    source: 'cn_quota',
    success: true,
    fetched_at: '2026-10-06T00:00:00Z',
    ...overrides,
  }
}

describe('ZhipuEffectiveRateHealthCard', () => {
  it('renders nothing for legacy snapshots without any sign field', () => {
    const variants: Array<Partial<MonitorQuotaSnapshot>> = [
      {},
      { tiers: [{ window: '5h', used_percent: 10 }] },
      // 只有 false 的显式布尔位（omitempty 下不会出现，但要防御）：不算有对账数据
      { sign_reconcile_stale: false, sign_reconcile_deviation: false },
    ]

    for (const [index, overrides] of variants.entries()) {
      const wrapper = mount(ZhipuEffectiveRateHealthCard, {
        props: { snapshot: makeSnapshot(overrides) },
      })
      expect(`${index}: ${wrapper.find('[data-testid="zhipu-sign-health-card"]').exists()}`).toBe(
        `${index}: false`
      )
      expect(`${index}: ${wrapper.text()}`).toBe(`${index}: `)
      wrapper.unmount()
    }
  })

  it('renders nothing without a snapshot', () => {
    const wrapper = mount(ZhipuEffectiveRateHealthCard, { props: { snapshot: null } })
    expect(wrapper.find('[data-testid="zhipu-sign-health-card"]').exists()).toBe(false)
  })

  // 表驱动：状态阈值表（目标 0.67，告警阈值 0.70）来自 design M5 / ui-panels §6.1；
  // 期望值全部是规格里的字面量，不由实现推导。
  const stateCases: Array<[number | undefined, string, string]> = [
    [0.67, 'on_target', '0.67'],
    [0.5, 'on_target', '0.50'],
    [0.68, 'above_target', '0.68'],
    [0.7, 'above_target', '0.70'],
    [0.71, 'alert', '0.71'],
    [1, 'alert', '1.00'],
    [undefined, 'unknown', '-'],
  ]

  it.each(stateCases)('maps rate %s to state %s (value %s)', (rate, expectedState, expectedValue) => {
    const wrapper = mount(ZhipuEffectiveRateHealthCard, {
      props: {
        snapshot: makeSnapshot({
          sign_effective_rate: rate,
          // 让字段存在（rate 缺失时也用对账时间触发面板渲染）
          sign_reconciled_at: '2026-10-06T00:00:00Z',
        }),
      },
    })

    const card = wrapper.get('[data-testid="zhipu-sign-health-card"]')
    expect(card.attributes('data-state')).toBe(expectedState)
    expect(card.get('[data-testid="zhipu-sign-health-value"]').text()).toBe(expectedValue)

    // 状态文字必须存在（色彩不是唯一状态信号）
    const stateKeys: Record<string, string> = {
      on_target: 'onTarget',
      above_target: 'aboveTarget',
      alert: 'alert',
      unknown: 'unknown',
    }
    const stateBadge = card.get('[data-testid="zhipu-sign-health-state"]')
    expect(stateBadge.text()).toBe(
      `monitorCommon.signHealth.states.${stateKeys[expectedState]}`
    )

    // 偏离状态色：健康 emerald / 注意 amber / 告警 red / 未知 gray
    const stateColors: Record<string, string> = {
      on_target: 'emerald',
      above_target: 'amber',
      alert: 'red',
      unknown: 'gray',
    }
    expect(stateBadge.html()).toContain(stateColors[expectedState])

    // 负向断言：任何状态下都不得出现 NaN / Invalid Date，未知态不得用 0.00 冒充
    expect(card.text()).not.toContain('NaN')
    expect(card.text()).not.toContain('Invalid Date')
    if (expectedState === 'unknown') {
      expect(card.text()).not.toContain('0.00')
    }
  })

  it('keeps target and a signed variance next to the value', () => {
    const cases: Array<[number, string]> = [
      [0.68, '+0.01'],
      [0.71, '+0.04'],
      [0.67, '0.00'],
      [0.6, '-0.07'],
    ]

    for (const [rate, expectedVariance] of cases) {
      const wrapper = mount(ZhipuEffectiveRateHealthCard, {
        props: { snapshot: makeSnapshot({ sign_effective_rate: rate }) },
      })
      const card = wrapper.get('[data-testid="zhipu-sign-health-card"]')
      expect(`${rate}: ${card.get('[data-testid="zhipu-sign-health-target"]').text()}`).toBe(
        `${rate}: 0.67`
      )
      expect(`${rate}: ${card.get('[data-testid="zhipu-sign-health-variance"]').text()}`).toBe(
        `${rate}: ${expectedVariance}`
      )
      wrapper.unmount()
    }

    // 无数据：偏差显示占位符，而不是 0.00/-0.67
    const unknown = mount(ZhipuEffectiveRateHealthCard, {
      props: { snapshot: makeSnapshot({ sign_reconciled_at: '2026-10-06T00:00:00Z' }) },
    })
    const unknownCard = unknown.get('[data-testid="zhipu-sign-health-card"]')
    expect(unknownCard.get('[data-testid="zhipu-sign-health-variance"]').text()).toBe('-')
    expect(unknownCard.get('[data-testid="zhipu-sign-health-target"]').text()).toBe('0.67')
  })

  // L2 审计判定优先于前端默认阈值：后端 sign_reconcile_deviation 是按生效阈值算出来的，
  // 自定义阈值（>0.70）时前端不得反向把「未偏离」渲染成告警。
  it('treats the backend deviation flag as authoritative over the default threshold', () => {
    const flagCases: Array<[boolean, number, string]> = [
      [true, 0.67, 'alert'],
      [true, 0.72, 'alert'],
      [false, 0.75, 'above_target'],
      [false, 0.67, 'on_target'],
      [false, 0.7, 'above_target'],
    ]

    for (const [deviation, rate, expectedState] of flagCases) {
      const wrapper = mount(ZhipuEffectiveRateHealthCard, {
        props: {
          snapshot: makeSnapshot({
            sign_effective_rate: rate,
            sign_reconcile_deviation: deviation,
          }),
        },
      })
      const card = wrapper.get('[data-testid="zhipu-sign-health-card"]')
      expect(`${deviation}/${rate}: ${card.attributes('data-state')}`).toBe(
        `${deviation}/${rate}: ${expectedState}`
      )
      wrapper.unmount()
    }
  })

  it('summarises the alert reason without offering any action', () => {
    const viaThreshold = mount(ZhipuEffectiveRateHealthCard, {
      props: { snapshot: makeSnapshot({ sign_effective_rate: 0.71 }) },
    })
    const thresholdReason = viaThreshold.get('[data-testid="zhipu-sign-health-reason"]')
    expect(thresholdReason.text()).toContain('monitorCommon.signHealth.reasonAboveAlertThreshold')
    expect(thresholdReason.text()).toContain('0.70')
    expect(thresholdReason.html()).toContain('text-red-600')

    // 标志位触发的告警点名 sign_reconcile_deviation（不写死 0.70）
    const viaFlag = mount(ZhipuEffectiveRateHealthCard, {
      props: {
        snapshot: makeSnapshot({ sign_effective_rate: 0.67, sign_reconcile_deviation: true }),
      },
    })
    const flagReason = viaFlag.get('[data-testid="zhipu-sign-health-reason"]')
    expect(flagReason.text()).toBe('monitorCommon.signHealth.reasonDeviationFlag')

    // 非告警态没有原因摘要；卡片整体不是操作入口
    for (const wrapper of [viaThreshold, viaFlag]) {
      wrapper.unmount()
    }
    const healthy = mount(ZhipuEffectiveRateHealthCard, {
      props: { snapshot: makeSnapshot({ sign_effective_rate: 0.67 }) },
    })
    const card = healthy.get('[data-testid="zhipu-sign-health-card"]')
    expect(card.find('[data-testid="zhipu-sign-health-reason"]').exists()).toBe(false)
    expect(
      card.findAll('button, a, input, select, textarea, [role="button"], [tabindex]')
    ).toHaveLength(0)
  })

  it('shows the current period factor and the reconciliation time through i18n', () => {
    const wrapper = mount(ZhipuEffectiveRateHealthCard, {
      props: {
        snapshot: makeSnapshot({
          sign_effective_rate: 0.67,
          sign_peak_factor: 0.5,
          sign_reconciled_at: '2026-10-06T00:00:00Z',
        }),
      },
    })

    const card = wrapper.get('[data-testid="zhipu-sign-health-card"]')
    const factor = card.get('[data-testid="zhipu-sign-health-peak-factor"]')
    expect(factor.text()).toBe('monitorCommon.signHealth.peakFactor|{"factor":"0.50"}')
    expect(factor.attributes('title')).toBe('monitorCommon.signHealth.peakFactorTooltip')

    const reconciled = card.get('[data-testid="zhipu-sign-health-reconciled-at"]')
    expect(reconciled.text()).toContain('monitorCommon.signHealth.reconciledAt')
    // 可解析的 ISO 时间已被本地化，原文不再出现在正文里
    expect(reconciled.text()).not.toContain('2026-10-06T00:00:00Z')
    expect(reconciled.attributes('title')).toBe('monitorCommon.signHealth.reconciledAtTooltip')
  })

  it('keeps missing or invalid reconciliation data as placeholders rather than fake dates', () => {
    const missing = mount(ZhipuEffectiveRateHealthCard, {
      props: { snapshot: makeSnapshot({ sign_effective_rate: 0.67 }) },
    })
    expect(
      missing.get('[data-testid="zhipu-sign-health-reconciled-at"]').text()
    ).toBe('monitorCommon.signHealth.reconciledAt|{"time":"-"}')
    expect(
      missing.get('[data-testid="zhipu-sign-health-peak-factor"]').text()
    ).toBe('monitorCommon.signHealth.peakFactor|{"factor":"-"}')
    expect(missing.text()).not.toContain('Invalid Date')

    // 非法日期：原样透出（不伪造日期，也不渲染 Invalid Date）
    const invalid = mount(ZhipuEffectiveRateHealthCard, {
      props: {
        snapshot: makeSnapshot({
          sign_effective_rate: 0.67,
          sign_reconciled_at: 'not-a-date',
        }),
      },
    })
    const reconciled = invalid.get('[data-testid="zhipu-sign-health-reconciled-at"]')
    expect(reconciled.text()).toContain('not-a-date')
    expect(invalid.text()).not.toContain('Invalid Date')
    expect(invalid.text()).not.toContain('NaN')

    // 非数值系数（字段存在但类型不对）→ 未知态，不抛错也不显示 0.00
    const bogus = mount(ZhipuEffectiveRateHealthCard, {
      props: {
        snapshot: makeSnapshot({
          sign_effective_rate: 'oops' as never,
          sign_reconciled_at: '2026-10-06T00:00:00Z',
        }),
      },
    })
    expect(bogus.get('[data-testid="zhipu-sign-health-card"]').attributes('data-state')).toBe(
      'unknown'
    )
    expect(bogus.get('[data-testid="zhipu-sign-health-value"]').text()).toBe('-')
  })

  it('marks stale reconciliations without recolouring the last value as healthy', () => {
    const stale = mount(ZhipuEffectiveRateHealthCard, {
      props: {
        snapshot: makeSnapshot({
          sign_effective_rate: 0.71,
          sign_reconcile_stale: true,
        }),
      },
    })
    const staleCard = stale.get('[data-testid="zhipu-sign-health-card"]')
    const staleNote = staleCard.get('[data-testid="zhipu-sign-health-stale"]')
    expect(staleNote.text()).toBe('monitorCommon.signHealth.stale')
    expect(staleNote.html()).toContain('text-amber')
    // 保留最后成功值与原状态色（陈旧不是「健康」）
    expect(staleCard.get('[data-testid="zhipu-sign-health-value"]').text()).toBe('0.71')
    expect(staleCard.attributes('data-state')).toBe('alert')

    // 陈旧但没有可展示数值：只有未知态，不凭空造陈旧标记
    const staleWithoutRate = mount(ZhipuEffectiveRateHealthCard, {
      props: { snapshot: makeSnapshot({ sign_reconcile_stale: true }) },
    })
    const unknownCard = staleWithoutRate.get('[data-testid="zhipu-sign-health-card"]')
    expect(unknownCard.attributes('data-state')).toBe('unknown')
    expect(unknownCard.find('[data-testid="zhipu-sign-health-stale"]').exists()).toBe(false)
    expect(unknownCard.get('[data-testid="zhipu-sign-health-value"]').text()).toBe('-')

    // 新鲜数据没有陈旧标记
    const fresh = mount(ZhipuEffectiveRateHealthCard, {
      props: { snapshot: makeSnapshot({ sign_effective_rate: 0.67 }) },
    })
    expect(fresh.find('[data-testid="zhipu-sign-health-stale"]').exists()).toBe(false)
  })

  it('reports a read failure while keeping the last value when one exists', () => {
    const withValue = mount(ZhipuEffectiveRateHealthCard, {
      props: {
        snapshot: makeSnapshot({
          success: false,
          error: 'upstream 500',
          sign_effective_rate: 0.68,
        }),
      },
    })
    const failedCard = withValue.get('[data-testid="zhipu-sign-health-card"]')
    expect(failedCard.get('[data-testid="zhipu-sign-health-failed"]').text()).toBe(
      'monitorCommon.signHealth.failed'
    )
    expect(failedCard.get('[data-testid="zhipu-sign-health-failed"]').html()).toContain(
      'text-red-600'
    )
    expect(failedCard.get('[data-testid="zhipu-sign-health-value"]').text()).toBe('0.68')

    // 失败且没有任何签名快照字段（老快照/未启用签名）：不虚构健康面板
    const noValue = mount(ZhipuEffectiveRateHealthCard, {
      props: { snapshot: makeSnapshot({ success: false, error: 'boom' }) },
    })
    expect(noValue.find('[data-testid="zhipu-sign-health-card"]').exists()).toBe(false)

    // 有签名字段但无有效样本：失败态 + 未知值，仍然不虚构 0.00
    const staleOnly = mount(ZhipuEffectiveRateHealthCard, {
      props: { snapshot: makeSnapshot({ success: false, error: 'boom', sign_reconcile_stale: true }) },
    })
    const staleOnlyCard = staleOnly.get('[data-testid="zhipu-sign-health-card"]')
    expect(staleOnlyCard.attributes('data-state')).toBe('unknown')
    expect(staleOnlyCard.get('[data-testid="zhipu-sign-health-value"]').text()).toBe('-')
    expect(staleOnlyCard.get('[data-testid="zhipu-sign-health-failed"]').text()).toBe(
      'monitorCommon.signHealth.failed'
    )
    expect(staleOnlyCard.text()).not.toContain('0.00')
  })
})

// 健康卡文案必须 zh/en 齐备且键集合一致（票 30 验收项 5）。
function flattenKeys(node: unknown, prefix = ''): string[] {
  if (typeof node !== 'object' || node === null || Array.isArray(node)) {
    return prefix ? [prefix] : []
  }
  return Object.entries(node as Record<string, unknown>).flatMap(([key, value]) =>
    flattenKeys(value, prefix ? `${prefix}.${key}` : key)
  )
}

describe('ZhipuEffectiveRateHealthCard i18n keys', () => {
  const requiredKeys = [
    'monitorCommon.signHealth.title',
    'monitorCommon.signHealth.current',
    'monitorCommon.signHealth.target',
    'monitorCommon.signHealth.variance',
    'monitorCommon.signHealth.peakFactor',
    'monitorCommon.signHealth.peakFactorTooltip',
    'monitorCommon.signHealth.reconciledAt',
    'monitorCommon.signHealth.reconciledAtTooltip',
    'monitorCommon.signHealth.effectiveRateTooltip',
    'monitorCommon.signHealth.states.onTarget',
    'monitorCommon.signHealth.states.aboveTarget',
    'monitorCommon.signHealth.states.alert',
    'monitorCommon.signHealth.states.unknown',
    'monitorCommon.signHealth.stale',
    'monitorCommon.signHealth.failed',
    'monitorCommon.signHealth.reasonAboveAlertThreshold',
    'monitorCommon.signHealth.reasonDeviationFlag',
  ]

  it.each([
    ['zh', zhDashboard],
    ['en', enDashboard],
  ] as const)('%s dashboard locale contains every health card key', (_locale, messages) => {
    const keys = new Set(flattenKeys(messages))
    expect(requiredKeys.filter((key) => !keys.has(key))).toEqual([])
  })

  it('keeps the zh/en key sets identical for the health card subtree', () => {
    const zhKeys = flattenKeys(zhDashboard.monitorCommon.signHealth).sort()
    const enKeys = flattenKeys(enDashboard.monitorCommon.signHealth).sort()
    expect(zhKeys).toEqual(enKeys)
  })

  // 票 29 补验教训：说明文案要带字面字段名，运维才能把界面文案对回快照字段。
  it('names the snapshot fields literally in both locales', () => {
    const zhHealth = zhDashboard.monitorCommon.signHealth
    const enHealth = enDashboard.monitorCommon.signHealth

    for (const health of [zhHealth, enHealth]) {
      expect(health.effectiveRateTooltip).toContain('sign_effective_rate')
      expect(health.peakFactorTooltip).toContain('sign_peak_factor')
      expect(health.reconciledAtTooltip).toContain('sign_reconciled_at')
      expect(health.reasonDeviationFlag).toContain('sign_reconcile_deviation')
      // 目标与阈值口径必须在文案里可见
      expect(health.effectiveRateTooltip).toContain('0.67')
      expect(health.reasonAboveAlertThreshold).toContain('0.70')
    }
  })
})
