/**
 * 票 29：渠道「签名 V4」区块的 i18n 完整性（zh/en 键集合必须一致，缺键即失败）。
 *
 * 费率影响文案是运营口径的一部分（降级 = 1.0 系数 = 成本 ×1.5），因此除键完整性外
 * 还断言两种语言都携带该数字口径，且 open/closed 的文案确实不同（不是同一句话复用）。
 */
import { describe, expect, it } from 'vitest'

import en from '../locales/en'
import zh from '../locales/zh'

type LocaleNode = Record<string, unknown>

const SIGN_V4 = 'signV4'

function signV4(locale: LocaleNode, label: string): LocaleNode {
  const admin = locale.admin as LocaleNode | undefined
  const channels = admin?.channels as LocaleNode | undefined
  const block = channels?.[SIGN_V4] as LocaleNode | undefined
  expect(block, `${label} admin.channels.${SIGN_V4} must exist`).toBeTruthy()
  return block as LocaleNode
}

function flatten(node: LocaleNode, prefix = ''): string[] {
  const out: string[] = []
  for (const [key, value] of Object.entries(node)) {
    const path = prefix ? `${prefix}.${key}` : key
    if (value && typeof value === 'object' && !Array.isArray(value)) {
      out.push(...flatten(value as LocaleNode, path))
    } else {
      out.push(path)
    }
  }
  return out.sort()
}

function leaf(node: LocaleNode, path: string): string {
  const value = path
    .split('.')
    .reduce<unknown>((acc, part) => (acc as LocaleNode | undefined)?.[part], node)
  expect(typeof value, `${path} must be a string leaf`).toBe('string')
  return value as string
}

describe('zhipu sign V4 channel block locales', () => {
  it('defines the same key set in zh and en', () => {
    const zhKeys = flatten(signV4(zh as LocaleNode, 'zh'))
    const enKeys = flatten(signV4(en as LocaleNode, 'en'))

    expect(zhKeys).toEqual(enKeys)
    expect(zhKeys.length).toBeGreaterThan(0)
  })

  it('covers the four status states and the account-level circuit-break wording', () => {
    for (const [label, locale] of [['zh', zh], ['en', en]] as const) {
      const node = signV4(locale as LocaleNode, label)
      const keys = flatten(node)
      for (const required of [
        'status.loading',
        'status.error',
        'status.retry',
        'status.empty',
        'status.keyCachedYes',
        'status.keyCachedNo',
        'status.lastHandshake',
        'status.neverHandshake',
        'status.consecutiveFailures',
        'status.circuitBreakTripped',
        'status.circuitBreakNone',
        'status.circuitBreakUnknown',
      ]) {
        expect(keys).toContain(required)
      }
    }
  })

  it('maps every backend reason code of ticket 28 to a message plus a permission fallback', () => {
    for (const [label, locale] of [['zh', zh], ['en', en]] as const) {
      const keys = flatten(signV4(locale as LocaleNode, label))
      for (const required of [
        'errors.ZHIPU_SIGN_CONFIG_INVALID',
        'errors.ZHIPU_SIGN_CONFIG_EMPTY',
        'errors.ZHIPU_SIGN_CONFIG_UNAVAILABLE',
        'errors.forbidden',
        'errors.configFailed',
        'errors.saveFailed',
      ]) {
        expect(keys).toContain(required)
      }
    }
  })

  it('states the 1.0-rate cost multiple for fail-open in both languages', () => {
    for (const [label, locale] of [['zh', zh], ['en', en]] as const) {
      const node = signV4(locale as LocaleNode, label)
      const impact = leaf(node, 'policy.open.impact')
      expect(impact).toContain('1.5')
      expect(impact.length).toBeGreaterThan(10)
    }
  })

  it('describes fail-closed as refusing the 1.0 rate instead of reusing the open copy', () => {
    const zhNode = signV4(zh as LocaleNode, 'zh')
    const enNode = signV4(en as LocaleNode, 'en')

    for (const node of [zhNode, enNode]) {
      const keys = flatten(node)
      expect(keys).toContain('policy.closed.impact')
      expect(leaf(node, 'policy.closed.impact')).not.toBe(leaf(node, 'policy.open.impact'))
    }
  })

  it('names the effective scope as global flag plus the zcode_client_sign account marker', () => {
    for (const [label, locale] of [['zh', zh], ['en', en]] as const) {
      const scope = leaf(signV4(locale as LocaleNode, label), 'scope')
      expect(scope).toContain('sign_v4_enabled')
      expect(scope).toContain('zcode_client_sign')
    }
  })
})
