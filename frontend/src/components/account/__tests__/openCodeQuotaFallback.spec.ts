import { describe, expect, it } from 'vitest'
import {
  DEFAULT_OPENCODE_QUOTA_FALLBACK_THRESHOLD,
  OPENCODE_QUOTA_FALLBACK_CREDENTIAL_KEY,
  applyOpenCodeQuotaFallbackConfig,
  createOpenCodeQuotaFallbackConfig,
  isOpenCodeQuotaFallbackEligible,
  mergeOpenCodeQuotaFallbackCandidates,
  normalizeOpenCodeQuotaFallbackConfig,
  normalizeOpenCodeQuotaFallbackStatus,
  openCodeQuotaFallbackCandidatesFromSync,
  openCodeQuotaFallbackSourceOptions,
  parseOpenCodeQuotaFallbackConfig,
  validateOpenCodeQuotaFallbackConfig,
  type OpenCodeQuotaFallbackConfig
} from '../openCodeQuotaFallback'

function enabledConfig(
  overrides: Partial<OpenCodeQuotaFallbackConfig> = {}
): OpenCodeQuotaFallbackConfig {
  return {
    enabled: true,
    threshold_percent: 98,
    model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' },
    ...overrides
  }
}

describe('createOpenCodeQuotaFallbackConfig', () => {
  it('defaults to disabled with threshold 98 and no mapping', () => {
    expect(createOpenCodeQuotaFallbackConfig()).toEqual({
      enabled: false,
      threshold_percent: DEFAULT_OPENCODE_QUOTA_FALLBACK_THRESHOLD,
      model_mapping: {}
    })
  })
})

describe('parseOpenCodeQuotaFallbackConfig', () => {
  it('returns null for absent or non-object values (legacy accounts stay fieldless)', () => {
    expect(parseOpenCodeQuotaFallbackConfig(undefined)).toBeNull()
    expect(parseOpenCodeQuotaFallbackConfig(null)).toBeNull()
    expect(parseOpenCodeQuotaFallbackConfig('enabled')).toBeNull()
    expect(parseOpenCodeQuotaFallbackConfig([1, 2])).toBeNull()
  })

  it('reads the frozen schema verbatim', () => {
    expect(
      parseOpenCodeQuotaFallbackConfig({
        enabled: true,
        threshold_percent: 95,
        model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' }
      })
    ).toEqual({
      enabled: true,
      threshold_percent: 95,
      model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' }
    })
  })

  it('applies safe defaults for dirty scalar values instead of trusting them', () => {
    expect(parseOpenCodeQuotaFallbackConfig({})).toEqual({
      enabled: false,
      threshold_percent: 98,
      model_mapping: {}
    })
    const dirty = parseOpenCodeQuotaFallbackConfig({
      enabled: 'yes',
      threshold_percent: '98',
      model_mapping: { 'a': 1, ' b ': ' c ', '': 'x' }
    })
    expect(dirty).toEqual({
      enabled: false,
      threshold_percent: 98,
      model_mapping: { b: 'c' }
    })
  })

  it('keeps a finite numeric threshold for echo so validation can flag it on enable', () => {
    expect(
      parseOpenCodeQuotaFallbackConfig({ enabled: false, threshold_percent: 120.5, model_mapping: {} })
    ).toEqual({ enabled: false, threshold_percent: 120.5, model_mapping: {} })
  })

  it('treats a non-object model_mapping as empty', () => {
    expect(
      parseOpenCodeQuotaFallbackConfig({ enabled: false, threshold_percent: 98, model_mapping: [1, 2] })
    ).toEqual({ enabled: false, threshold_percent: 98, model_mapping: {} })
  })
})

describe('isOpenCodeQuotaFallbackEligible', () => {
  it('accepts apikey accounts on legacy opencode and opencode_go in go mode', () => {
    expect(isOpenCodeQuotaFallbackEligible('opencode', 'apikey')).toBe(true)
    expect(isOpenCodeQuotaFallbackEligible('opencode', 'apikey', 'go')).toBe(true)
    expect(isOpenCodeQuotaFallbackEligible('opencode_go', 'apikey', 'go')).toBe(true)
    // 缺省 mode 即 go（resolveOpenCodeAccountMode 语义）
    expect(isOpenCodeQuotaFallbackEligible('opencode_go', 'apikey')).toBe(true)
  })

  it('rejects oauth, zen mode (legacy included) and every other platform', () => {
    expect(isOpenCodeQuotaFallbackEligible('opencode', 'oauth')).toBe(false)
    // Zen 不因 legacy opencode 身份豁免
    expect(isOpenCodeQuotaFallbackEligible('opencode', 'apikey', 'zen')).toBe(false)
    expect(isOpenCodeQuotaFallbackEligible('opencode_go', 'apikey', 'zen')).toBe(false)
    expect(isOpenCodeQuotaFallbackEligible('opencode_go', 'oauth', 'go')).toBe(false)
    expect(isOpenCodeQuotaFallbackEligible('openai', 'apikey')).toBe(false)
    expect(isOpenCodeQuotaFallbackEligible('kimi', 'apikey', 'coding')).toBe(false)
  })
})

describe('validateOpenCodeQuotaFallbackConfig', () => {
  it('never blocks a disabled config', () => {
    expect(
      validateOpenCodeQuotaFallbackConfig({ enabled: false, threshold_percent: 0, model_mapping: {} })
    ).toBeNull()
  })

  it('requires an integer threshold in 1..100 when enabled', () => {
    for (const threshold of [0, 101, 97.5, Number.NaN]) {
      expect(validateOpenCodeQuotaFallbackConfig(enabledConfig({ threshold_percent: threshold }))).toBe(
        'thresholdInvalid'
      )
    }
    expect(validateOpenCodeQuotaFallbackConfig(enabledConfig({ threshold_percent: 1 }))).toBeNull()
    expect(validateOpenCodeQuotaFallbackConfig(enabledConfig({ threshold_percent: 100 }))).toBeNull()
  })

  it('requires at least one complete non-identity mapping when enabled', () => {
    expect(validateOpenCodeQuotaFallbackConfig(enabledConfig({ model_mapping: {} }))).toBe('mappingInvalid')
    expect(validateOpenCodeQuotaFallbackConfig(enabledConfig({ model_mapping: { a: '' } }))).toBe(
      'mappingInvalid'
    )
    expect(validateOpenCodeQuotaFallbackConfig(enabledConfig({ model_mapping: { a: 'a' } }))).toBe(
      'mappingInvalid'
    )
    expect(validateOpenCodeQuotaFallbackConfig(enabledConfig({ model_mapping: { '': 'b' } }))).toBe(
      'mappingInvalid'
    )
    expect(validateOpenCodeQuotaFallbackConfig(enabledConfig({ model_mapping: { '': '' } }))).toBe(
      'mappingInvalid'
    )
    expect(validateOpenCodeQuotaFallbackConfig(enabledConfig())).toBeNull()
  })

  it('rejects a mixed valid + broken row instead of silently dropping the broken one', () => {
    expect(
      validateOpenCodeQuotaFallbackConfig(
        enabledConfig({ model_mapping: { good: 'target', 'bad-source': '' } })
      )
    ).toBe('mappingInvalid')
    expect(
      validateOpenCodeQuotaFallbackConfig(
        enabledConfig({ model_mapping: { good: 'target', self: 'self' } })
      )
    ).toBe('mappingInvalid')
  })

  it('rejects chains and cycles as mappingInvalid', () => {
    // A->B 与 B->C 链式：B 既是替代目标又是正常上游，effective 映射出现多跳歧义
    expect(
      validateOpenCodeQuotaFallbackConfig(
        enabledConfig({ model_mapping: { a: 'b', b: 'c' } })
      )
    ).toBe('mappingInvalid')
    // A->B 与 B->A 自环
    expect(
      validateOpenCodeQuotaFallbackConfig(
        enabledConfig({ model_mapping: { a: 'b', b: 'a' } })
      )
    ).toBe('mappingInvalid')
    // 两条互不相交的映射仍合法
    expect(
      validateOpenCodeQuotaFallbackConfig(
        enabledConfig({ model_mapping: { a: 'b', c: 'd' } })
      )
    ).toBeNull()
  })
})

describe('normalizeOpenCodeQuotaFallbackConfig', () => {
  it('completeOnly keeps only trimmed, non-empty, non-identity rows (enabled save)', () => {
    const normalized = normalizeOpenCodeQuotaFallbackConfig(
      enabledConfig({
        model_mapping: {
          ' deepseek-v4.1-flash ': ' step-5-preview ',
          '': 'orphan',
          'empty-target': '',
          'self': 'self'
        }
      }),
      { completeOnly: true }
    )
    expect(normalized.model_mapping).toEqual({ 'deepseek-v4.1-flash': 'step-5-preview' })
  })

  it('preserves in-progress rows when disabled so the admin does not lose work', () => {
    const normalized = normalizeOpenCodeQuotaFallbackConfig(
      {
        enabled: false,
        threshold_percent: 98,
        model_mapping: { ' deepseek-v4.1-flash ': '', '': 'orphan' }
      },
      { completeOnly: false }
    )
    expect(normalized.model_mapping).toEqual({ 'deepseek-v4.1-flash': '' })
  })

  it('falls back to the default threshold for non-finite values', () => {
    expect(
      normalizeOpenCodeQuotaFallbackConfig({
        enabled: false,
        threshold_percent: Number.NaN,
        model_mapping: {}
      }).threshold_percent
    ).toBe(98)
  })
})

describe('applyOpenCodeQuotaFallbackConfig', () => {
  it('writes the frozen schema when enabled and eligible', () => {
    const credentials: Record<string, unknown> = { api_key: 'k' }
    applyOpenCodeQuotaFallbackConfig(credentials, enabledConfig({ threshold_percent: 95 }), {
      eligible: true
    })
    expect(credentials[OPENCODE_QUOTA_FALLBACK_CREDENTIAL_KEY]).toEqual({
      enabled: true,
      threshold_percent: 95,
      model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' }
    })
  })

  it('forces enabled=false when the account is not eligible (zen or other platform) without dropping the mapping', () => {
    const credentials: Record<string, unknown> = {}
    applyOpenCodeQuotaFallbackConfig(credentials, enabledConfig(), { eligible: false })
    expect(credentials[OPENCODE_QUOTA_FALLBACK_CREDENTIAL_KEY]).toEqual({
      enabled: false,
      threshold_percent: 98,
      model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' }
    })
  })

  it('leaves a legacy fieldless disabled account untouched (no enabled=true, no key)', () => {
    const credentials: Record<string, unknown> = { api_key: 'k' }
    applyOpenCodeQuotaFallbackConfig(credentials, createOpenCodeQuotaFallbackConfig(), { eligible: true })
    expect(OPENCODE_QUOTA_FALLBACK_CREDENTIAL_KEY in credentials).toBe(false)
  })

  it('keeps a disabled config (with mapping) instead of deleting it', () => {
    const credentials: Record<string, unknown> = {}
    applyOpenCodeQuotaFallbackConfig(
      credentials,
      { enabled: false, threshold_percent: 98, model_mapping: { a: 'b' } },
      { eligible: true }
    )
    expect(credentials[OPENCODE_QUOTA_FALLBACK_CREDENTIAL_KEY]).toEqual({
      enabled: false,
      threshold_percent: 98,
      model_mapping: { a: 'b' }
    })
  })

  it('rewrites an existing stored config back in disabled form when switched off', () => {
    const credentials: Record<string, unknown> = {
      [OPENCODE_QUOTA_FALLBACK_CREDENTIAL_KEY]: {
        enabled: true,
        threshold_percent: 98,
        model_mapping: { a: 'b' }
      },
      untouched: 'keep'
    }
    applyOpenCodeQuotaFallbackConfig(
      credentials,
      { enabled: false, threshold_percent: 90, model_mapping: { a: 'b' } },
      { eligible: true }
    )
    expect(credentials[OPENCODE_QUOTA_FALLBACK_CREDENTIAL_KEY]).toEqual({
      enabled: false,
      threshold_percent: 90,
      model_mapping: { a: 'b' }
    })
    expect(credentials.untouched).toBe('keep')
  })

  it('never writes an enabled config containing broken rows (defensive normalization)', () => {
    // 启用态校验会先拒收此类配置（modal 层阻断）；apply 仍兜底只落完整行
    const credentials: Record<string, unknown> = {}
    applyOpenCodeQuotaFallbackConfig(credentials, enabledConfig({ model_mapping: { a: '' } }), {
      eligible: true
    })
    expect(credentials[OPENCODE_QUOTA_FALLBACK_CREDENTIAL_KEY]).toEqual({
      enabled: true,
      threshold_percent: 98,
      model_mapping: {}
    })
  })
})

describe('openCodeQuotaFallbackSourceOptions', () => {
  it('returns the unique actual upstream targets of the normal model_mapping', () => {
    expect(
      openCodeQuotaFallbackSourceOptions({
        'claude-sonnet-4-5': 'deepseek-v4.1-flash',
        'deepseek-flash': 'deepseek-v4.1-flash',
        'passthrough': 'passthrough',
        'blank': '   '
      })
    ).toEqual(['deepseek-v4.1-flash', 'passthrough'])
  })

  it('returns an empty list for absent mappings', () => {
    expect(openCodeQuotaFallbackSourceOptions(undefined)).toEqual([])
    expect(openCodeQuotaFallbackSourceOptions(null)).toEqual([])
  })
})

describe('openCodeQuotaFallbackCandidatesFromSync', () => {
  it('never claims availability from a bare id: public catalogs only give unknown', () => {
    expect(
      openCodeQuotaFallbackCandidatesFromSync({
        models: ['step-5-preview', 'free-model'],
        metadata: { 'step-5-preview': { id: 'step-5-preview', display_name: 'Step 5 Preview' } }
      })
    ).toEqual([
      { id: 'step-5-preview', label: 'Step 5 Preview', capability: 'unknown' },
      { id: 'free-model', capability: 'unknown' }
    ])
  })

  it('handles empty and absent results', () => {
    expect(openCodeQuotaFallbackCandidatesFromSync(undefined)).toEqual([])
    expect(openCodeQuotaFallbackCandidatesFromSync({ models: [] })).toEqual([])
  })
})

describe('mergeOpenCodeQuotaFallbackCandidates', () => {
  it('keeps last-known ids as they were when the fresh catalog no longer lists them', () => {
    // 未出现在本次目录不代表协议不支持，也不得因此禁止管理员手输
    expect(
      mergeOpenCodeQuotaFallbackCandidates(
        [
          { id: 'gone', label: 'Gone', capability: 'unknown' },
          { id: 'kept', capability: 'unknown' }
        ],
        [{ id: 'kept', capability: 'unknown' }, { id: 'new', capability: 'unknown' }]
      )
    ).toEqual([
      { id: 'kept', capability: 'unknown' },
      { id: 'new', capability: 'unknown' },
      { id: 'gone', label: 'Gone', capability: 'unknown' }
    ])
  })
})

describe('normalizeOpenCodeQuotaFallbackStatus', () => {
  it('returns null for absent or non-object DTOs', () => {
    expect(normalizeOpenCodeQuotaFallbackStatus(undefined)).toBeNull()
    expect(normalizeOpenCodeQuotaFallbackStatus('fallback_active')).toBeNull()
  })

  it('reads the frozen read-only DTO shape verbatim', () => {
    expect(
      normalizeOpenCodeQuotaFallbackStatus({
        enabled: true,
        state: 'fallback_active',
        threshold_percent: 98,
        triggering_windows: [
          { window: '5h', used_percent: 98.2, resets_at: '2026-10-16T12:00:00Z' },
          { window: 'weekly', used_percent: 100 }
        ],
        snapshot_at: '2026-10-16T11:00:00Z',
        reason: ''
      })
    ).toEqual({
      enabled: true,
      state: 'fallback_active',
      threshold_percent: 98,
      triggering_windows: [
        { window: '5h', used_percent: 98.2, resets_at: '2026-10-16T12:00:00Z' },
        { window: 'weekly', used_percent: 100 }
      ],
      snapshot_at: '2026-10-16T11:00:00Z',
      reason: ''
    })
  })

  it('never maps an unknown or legacy state to normal', () => {
    expect(normalizeOpenCodeQuotaFallbackStatus({ state: 'weird' })?.state).toBe('unknown')
    expect(normalizeOpenCodeQuotaFallbackStatus({})?.state).toBe('unknown')
    // 冻结字段之外的定义（如 inactive）不作为别名接受
    expect(normalizeOpenCodeQuotaFallbackStatus({ state: 'inactive' })?.state).toBe('unknown')
  })

  it('drops malformed windows and avoids percentage claims when absent', () => {
    const status = normalizeOpenCodeQuotaFallbackStatus({
      enabled: true,
      state: 'unknown',
      threshold_percent: 300,
      triggering_windows: [{ window: '5h' }, { window: 'nope', used_percent: 10 }, 'weekly']
    })
    expect(status).toEqual({
      enabled: true,
      state: 'unknown',
      threshold_percent: 98,
      triggering_windows: [{ window: '5h' }]
    })
  })
})
