import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AccountCard from '../AccountCard.vue'
import type { Account } from '@/types'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key })
  }
})

vi.mock('@/utils/format', () => ({
  formatDateTime: (value: string) => value,
  formatNumber: (value: number) => String(value),
  formatRelativeTime: (value: string) => value
}))

vi.mock('@/utils/formatters', () => ({
  formatMultiplier: (value: number) => String(value)
}))

function makeAccount(): Account {
  return {
    id: 7,
    name: 'OpenCode primary',
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
    created_at: '2026-08-22T00:00:00Z',
    updated_at: '2026-08-22T00:00:00Z',
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    session_window_start: null,
    session_window_end: null,
    session_window_status: null,
  }
}

function mountAccountCard(account: Account = makeAccount()) {
  return mount(AccountCard, {
    props: {
      account,
      selected: false,
      cardFields: new Set<string>()
    },
    global: {
      stubs: {
        PlatformIcon: { template: '<span>platform-icon</span>' },
        PlatformTypeBadge: { template: '<span data-testid="platform-key">platform-key</span>' },
        AccountStatusIndicator: { template: '<span data-testid="account-status">account-status</span>' },
        AccountCapacityCell: true,
        AccountUsageCell: true,
        UpstreamBillingRateCell: true,
        UsageSummary: true
      }
    }
  })
}

describe('AccountCard', () => {
  it('renders platform key and status directly below the account name', () => {
    const wrapper = mountAccountCard()

    const primaryHeader = wrapper.get('[data-testid="account-card-primary-header"]')
    const statusRow = wrapper.get('[data-testid="account-card-status-row"]')
    const actions = wrapper.get('[data-testid="account-card-actions"]')

    // Name is in the same column as status, above it
    expect(primaryHeader.text()).toContain('OpenCode primary')
    // Status row is a descendant of primary header (below the name)
    expect(primaryHeader.find('[data-testid="account-card-status-row"]').exists()).toBe(true)
    expect(primaryHeader.get('[data-testid="platform-key"]').exists()).toBe(true)
    expect(primaryHeader.get('[data-testid="account-status"]').exists()).toBe(true)
    // Actions button is a sibling of primary header, not inside it
    expect(actions.element.parentElement).toBe(primaryHeader.element.parentElement)
  })

  it('renders the status row as a wrapping container without max-width clamp', () => {
    const wrapper = mountAccountCard()

    const statusRow = wrapper.get('[data-testid="account-card-status-row"]')

    expect(statusRow.classes()).toContain('flex-wrap')
    expect(statusRow.classes()).not.toContain('flex-shrink-0')
    expect(statusRow.classes().some((c) => c.startsWith('max-w-'))).toBe(false)
    // Badge and status indicator are direct children
    expect(statusRow.element.children).toHaveLength(2)
  })

  it('keeps toggle-select, edit and show-actions events wired', async () => {
    const account = makeAccount()
    const wrapper = mountAccountCard(account)

    // Checkbox is outside primaryHeader now (sibling in header)
    await wrapper.get('input[type="checkbox"]').trigger('click')
    expect(wrapper.emitted('toggle-select')).toHaveLength(1)
    expect(wrapper.emitted('toggle-select')?.[0]).toEqual([account.id])

    // Clicking the name area emits edit
    const primaryHeader = wrapper.get('[data-testid="account-card-primary-header"]')
    await primaryHeader.trigger('click')
    expect(wrapper.emitted('edit')?.[0]?.[0]).toEqual(account)

    await wrapper.get('[data-testid="account-card-actions"]').trigger('click')
    const showActions = wrapper.emitted('show-actions')
    expect(showActions).toHaveLength(1)
    expect(showActions?.[0]?.[0]).toEqual(account)
    expect(showActions?.[0]?.[1]).toBeInstanceOf(MouseEvent)
  })
})