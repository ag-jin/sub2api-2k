import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountUsageCell from '../AccountUsageCell.vue'
import type { Account } from '@/types'

const { getUsage } = vi.hoisted(() => ({
  getUsage: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getUsage
    }
  }
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

const STATUS_KEY_PREFIX = 'admin.accounts.openCodeQuotaFallback.status'

function makeAccount(overrides: Partial<Account>): Account {
  return {
    id: 1,
    name: 'account',
    platform: 'opencode',
    type: 'apikey',
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: true,
    created_at: '2026-03-15T00:00:00Z',
    updated_at: '2026-03-15T00:00:00Z',
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    session_window_start: null,
    session_window_end: null,
    session_window_status: null,
    ...overrides,
  }
}

function mountCell(account: Account) {
  return mount(AccountUsageCell, { props: { account } })
}

describe('AccountUsageCell OpenCode quota fallback status', () => {
  beforeEach(() => {
    getUsage.mockReset().mockResolvedValue({ opencode: { rolling: { percent: 42 } } })
  })

  it('renders the trigger windows and percentage for a fallback_active policy', async () => {
    const account = makeAccount({
      opencode_quota_fallback: {
        enabled: true,
        state: 'fallback_active',
        threshold_percent: 98,
        triggering_windows: [
          { window: '5h', used_percent: 98.2, resets_at: '2026-10-16T12:00:00Z' },
          { window: 'weekly', used_percent: 100 }
        ],
        snapshot_at: '2026-10-16T11:00:00Z'
      }
    })
    const wrapper = mountCell(account)
    await flushPromises()

    expect(wrapper.find('[data-testid="opencode-quota-fallback"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="opencode-quota-fallback-badge"]').text()).toBe(
      `${STATUS_KEY_PREFIX}.fallbackActive`
    )
    expect(wrapper.findAll('[data-testid="opencode-quota-fallback-window"]')).toHaveLength(2)
    expect(wrapper.get('[data-testid="opencode-quota-fallback-hint"]').text()).toContain(
      `${STATUS_KEY_PREFIX}.fallbackHint`
    )
  })

  it('reports an unknown state explicitly instead of pretending normal', async () => {
    const account = makeAccount({
      opencode_quota_fallback: {
        enabled: true,
        state: 'unknown',
        threshold_percent: 98,
        triggering_windows: [],
        reason: 'no_snapshot'
      }
    })
    const wrapper = mountCell(account)
    await flushPromises()

    expect(wrapper.get('[data-testid="opencode-quota-fallback-badge"]').text()).toBe(
      `${STATUS_KEY_PREFIX}.unknown`
    )
    const hint = wrapper.get('[data-testid="opencode-quota-fallback-hint"]').text()
    expect(hint).toContain(`${STATUS_KEY_PREFIX}.unknownHint`)
    expect(hint).toContain(`${STATUS_KEY_PREFIX}.reason.no_snapshot`)
    expect(wrapper.find('[data-testid="opencode-quota-fallback-window"]').exists()).toBe(false)
  })

  it('never exposes an unrecognized reason short code to users', async () => {
    const account = makeAccount({
      opencode_quota_fallback: {
        enabled: true,
        state: 'unknown',
        threshold_percent: 98,
        triggering_windows: [],
        reason: 'internal_code_42'
      }
    })
    const wrapper = mountCell(account)
    await flushPromises()

    const hint = wrapper.get('[data-testid="opencode-quota-fallback-hint"]').text()
    expect(hint).toContain(`${STATUS_KEY_PREFIX}.unknownHint`)
    expect(hint).toContain(`${STATUS_KEY_PREFIX}.reason.unknown`)
    expect(hint).not.toContain('internal_code_42')
  })

  it('maps stale_* reason codes to the stale copy without leaking the raw code', async () => {
    const account = makeAccount({
      opencode_quota_fallback: {
        enabled: true,
        state: 'unknown',
        threshold_percent: 98,
        triggering_windows: [],
        reason: 'stale_snapshot'
      }
    })
    const wrapper = mountCell(account)
    await flushPromises()

    const hint = wrapper.get('[data-testid="opencode-quota-fallback-hint"]').text()
    expect(hint).toContain(`${STATUS_KEY_PREFIX}.reason.stale`)
    expect(hint).not.toContain('stale_snapshot')
  })

  it('renders a normal state with the configured threshold', async () => {
    const account = makeAccount({
      opencode_quota_fallback: {
        enabled: true,
        state: 'normal',
        threshold_percent: 95,
        triggering_windows: []
      }
    })
    const wrapper = mountCell(account)
    await flushPromises()

    expect(wrapper.get('[data-testid="opencode-quota-fallback-badge"]').text()).toBe(
      `${STATUS_KEY_PREFIX}.normal`
    )
  })

  it('stays empty for non OpenCode platforms and for accounts without the DTO', async () => {
    const kimi = mountCell(
      makeAccount({
        platform: 'kimi',
        credentials: { account_mode: 'coding' },
        opencode_quota_fallback: {
          enabled: true,
          state: 'fallback_active',
          threshold_percent: 98,
          triggering_windows: []
        }
      })
    )
    await flushPromises()
    expect(kimi.find('[data-testid="opencode-quota-fallback"]').exists()).toBe(false)

    const withoutDto = mountCell(makeAccount({}))
    await flushPromises()
    expect(withoutDto.find('[data-testid="opencode-quota-fallback"]').exists()).toBe(false)
  })

  it('does not add a badge while the policy is off (no layout regression)', async () => {
    const account = makeAccount({
      opencode_quota_fallback: {
        enabled: false,
        state: 'disabled',
        threshold_percent: 98,
        triggering_windows: []
      }
    })
    const wrapper = mountCell(account)
    await flushPromises()

    expect(wrapper.find('[data-testid="opencode-quota-fallback"]').exists()).toBe(false)
  })

  it('renders the status next to the OpenCode GO cell for go-mode accounts', async () => {
    const account = makeAccount({
      platform: 'opencode_go',
      credentials: { account_mode: 'go' },
      opencode_go_usage: {
        account_id: 1,
        eligible: true,
        auto_refresh_enabled: false,
        snapshot: {
          status: 'ok',
          data: { rolling: { percent: 99 } }
        }
      },
      opencode_quota_fallback: {
        enabled: true,
        state: 'fallback_active',
        threshold_percent: 98,
        triggering_windows: [{ window: '5h', used_percent: 99 }]
      }
    })
    const wrapper = mountCell(account)
    await flushPromises()

    expect(wrapper.find('[data-testid="opencode-go-usage-cell"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="opencode-quota-fallback"]').exists()).toBe(true)
  })
})
