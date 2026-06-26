import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

const root = resolve(__dirname, '../../../..')

function source(path: string): string {
  return readFileSync(resolve(root, path), 'utf8')
}

describe('pool recovery minimal UI exposure contract', () => {
  it('keeps account create and edit forms exposing scheduling cost factor as non-billing control', () => {
    const createModal = source('src/components/account/CreateAccountModal.vue')
    const editModal = source('src/components/account/EditAccountModal.vue')
    const zh = source('src/i18n/locales/zh.ts')
    const en = source('src/i18n/locales/en.ts')

    expect(createModal).toContain('v-model.number="form.upstream_cost_factor"')
    expect(editModal).toContain('v-model.number="form.upstream_cost_factor"')
    expect(zh).toContain('仅影响池模式调度优先级，不影响用户计费')
    expect(en).toContain('Affects pool scheduling priority only, not user billing')
  })

  it('shows cost factor in the account list as a scheduling cost, not a billing multiplier', () => {
    const accountsView = source('src/views/admin/AccountsView.vue')
    const zh = source('src/i18n/locales/zh.ts')
    const en = source('src/i18n/locales/en.ts')

    expect(accountsView).toContain("key: 'upstream_cost_factor'")
    expect(accountsView).toContain('#cell-upstream_cost_factor')
    expect(zh).toContain("upstreamCostFactor: '调度成本'")
    expect(en).toContain("upstreamCostFactor: 'Scheduling Cost'")
  })

  it('exposes only the minimal stream continuation settings in system settings', () => {
    const settingsView = source('src/views/admin/SettingsView.vue')
    const settingsTypes = source('src/api/admin/settings.ts')
    const zh = source('src/i18n/locales/zh.ts')
    const en = source('src/i18n/locales/en.ts')

    expect(settingsView).toContain('form.openai_stream_continuation_enabled')
    expect(settingsView).toContain('form.openai_stream_continuation_budget_seconds')
    expect(settingsTypes).toContain('openai_stream_continuation_enabled?: boolean')
    expect(settingsTypes).toContain('openai_stream_continuation_budget_seconds?: number')
    expect(zh).toContain('启用文本流不中断续写')
    expect(zh).toContain('流中恢复总预算')
    expect(en).toContain('Continue interrupted text streams')
    expect(en).toContain('Stream recovery budget')

    expect(settingsView).not.toContain('openai_stream_continuation_same_account_retries')
    expect(settingsView).not.toContain('openai_stream_continuation_tail_window')
  })

  it('does not expose same-account retry internals in account forms or visible copy', () => {
    const createModal = source('src/components/account/CreateAccountModal.vue')
    const editModal = source('src/components/account/EditAccountModal.vue')
    const zh = source('src/i18n/locales/zh.ts')
    const en = source('src/i18n/locales/en.ts')

    for (const modal of [createModal, editModal]) {
      expect(modal).not.toContain('poolModeRetryCount')
      expect(modal).not.toContain('poolModeRetryStatusCodesInput')
      expect(modal).not.toContain('poolModeRetryCountHint')
      expect(modal).not.toContain('poolModeRetryStatusCodesHint')
    }

    expect(zh).not.toContain('同账号重试次数')
    expect(zh).not.toContain('同账号重试状态码')
    expect(en).not.toContain('Same-Account Retries')
    expect(en).not.toContain('Retry Status Codes')
  })
})
