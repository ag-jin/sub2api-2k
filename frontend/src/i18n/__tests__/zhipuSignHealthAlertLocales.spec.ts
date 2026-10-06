/**
 * 票 30：有效系数健康卡、签名降级徽标与告警中心补充明细的 i18n 完整性。
 *
 * 与票 29 的补验教训一致：除 zh/en 键集合必须一致外，说明性文案必须**字面**包含
 * 快照字段名 / 指标类型名 / 阈值口径 —— 界面文案要能对回后端契约，运维才知道
 * 该看哪个字段、哪条内置规则。
 */
import { describe, expect, it } from 'vitest'

import en from '../locales/en'
import zh from '../locales/zh'

type LocaleNode = Record<string, unknown>

function flatten(node: unknown, prefix = ''): string[] {
  if (typeof node !== 'object' || node === null || Array.isArray(node)) {
    return prefix ? [prefix] : []
  }
  return Object.entries(node as Record<string, unknown>).flatMap(([key, value]) =>
    flatten(value, prefix ? `${prefix}.${key}` : key)
  )
}

function node(locale: Record<string, unknown>, path: string, label: string): LocaleNode {
  const value = path
    .split('.')
    .reduce<unknown>((acc, part) => (acc as LocaleNode | undefined)?.[part], locale)
  expect(typeof value, `${label}: ${path} must exist`).toBe('object')
  return value as LocaleNode
}

function leaf(locale: Record<string, unknown>, path: string, label: string): string {
  const value = path
    .split('.')
    .reduce<unknown>((acc, part) => (acc as LocaleNode | undefined)?.[part], locale)
  expect(typeof value, `${label}: ${path} must be a string leaf`).toBe('string')
  return value as string
}

const BLOCKS = [
  'monitorCommon.signHealth',
  'admin.accounts.cnProviders.zhipuSign',
  'admin.ops.alertEvents.zhipuSign',
  'admin.ops.alertEvents.filters',
]

const zhLocale = zh as unknown as Record<string, unknown>
const enLocale = en as unknown as Record<string, unknown>

describe('ticket 30 locales', () => {
  it.each(BLOCKS)('defines the same key set in zh and en for %s', (path) => {
    const zhKeys = flatten(node(zhLocale, path, 'zh')).sort()
    const enKeys = flatten(node(enLocale, path, 'en')).sort()

    expect(zhKeys).toEqual(enKeys)
    expect(zhKeys.length).toBeGreaterThan(0)
  })

  it('covers every health-card state in both languages', () => {
    for (const [label, locale] of [
      ['zh', zhLocale],
      ['en', enLocale],
    ] as const) {
      const keys = flatten(node(locale, 'monitorCommon.signHealth', label))
      for (const required of [
        'title',
        'current',
        'target',
        'variance',
        'peakFactor',
        'reconciledAt',
        'states.onTarget',
        'states.aboveTarget',
        'states.alert',
        'states.unknown',
        'stale',
        'failed',
      ]) {
        expect(keys).toContain(required)
      }
    }
  })

  it('names the snapshot fields literally in both languages', () => {
    for (const [label, locale] of [
      ['zh', zhLocale],
      ['en', enLocale],
    ] as const) {
      const signHealth = node(locale, 'monitorCommon.signHealth', label)
      expect(leaf(signHealth as Record<string, unknown>, 'effectiveRateTooltip', label)).toContain(
        'sign_effective_rate'
      )
      expect(leaf(signHealth as Record<string, unknown>, 'peakFactorTooltip', label)).toContain(
        'sign_peak_factor'
      )
      expect(leaf(signHealth as Record<string, unknown>, 'reconciledAtTooltip', label)).toContain(
        'sign_reconciled_at'
      )
      expect(leaf(signHealth as Record<string, unknown>, 'reasonDeviationFlag', label)).toContain(
        'sign_reconcile_deviation'
      )
      // 目标 0.67 与默认告警阈值 0.70 的口径都在文案里
      expect(leaf(signHealth as Record<string, unknown>, 'effectiveRateTooltip', label)).toContain(
        '0.67'
      )
      expect(
        leaf(signHealth as Record<string, unknown>, 'reasonAboveAlertThreshold', label)
      ).toContain('0.70')
    }
  })

  it('names the two built-in alert metric types literally in both languages', () => {
    for (const [label, locale] of [
      ['zh', zhLocale],
      ['en', enLocale],
    ] as const) {
      const signAlert = node(locale, 'admin.ops.alertEvents.zhipuSign', label) as Record<
        string,
        unknown
      >
      const kind = signAlert.kind as Record<string, unknown>
      expect(String(kind.fail_window)).toContain('zhipu_sign_fail_window')
      expect(String(kind.effective_rate)).toContain('zhipu_sign_effective_rate')
      // L2 数值行必须给出目标 0.67 与阈值口径
      expect(String(signAlert.effectiveRate)).toContain('0.67')
      expect(String(signAlert.effectiveRate)).toContain('{threshold}')
      // L1 数值行给出窗口计数与阈值
      expect(String(signAlert.windowCount)).toContain('{value}')
      expect(String(signAlert.windowCount)).toContain('{threshold}')
    }
  })

  it('labels the account-list signing badge and the zhipu platform filter', () => {
    for (const [label, locale] of [
      ['zh', zhLocale],
      ['en', enLocale],
    ] as const) {
      const badge = node(locale, 'admin.accounts.cnProviders.zhipuSign', label)
      expect(typeof badge.degraded).toBe('string')
      expect(String(badge.degraded).length).toBeGreaterThan(0)
      expect(typeof badge.degradedTooltip).toBe('string')
      expect(String(badge.degradedTooltip).length).toBeGreaterThan(0)

      expect(
        String(leaf(locale, 'admin.ops.alertEvents.filters.platformZhipu', label))
      ).toContain('zhipu')
    }
  })
})
