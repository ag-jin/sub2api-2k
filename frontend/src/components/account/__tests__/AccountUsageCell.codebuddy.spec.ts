import { describe, expect, it, vi, beforeEach } from 'vitest'
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
      t: (key: string, params?: Record<string, unknown>) =>
        key === 'admin.accounts.codebuddy.usage.creditsValue'
          ? `${params?.value} credits`
          : key
    })
  }
})

function makeAccount(overrides: Partial<Account>): Account {
  return {
    id: 1,
    name: 'account',
    platform: 'codebuddy',
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

function mountCell(account: Account, extraProps: Record<string, unknown> = {}) {
  return mount(AccountUsageCell, {
    props: {
      account,
      ...extraProps,
    },
    global: {
      stubs: {
        UsageProgressBar: {
          props: ['label', 'utilization'],
          template: '<div class="usage-bar">{{ label }}|{{ utilization }}</div>'
        },
        AccountQuotaInfo: true,
        CNProviderQuotaCell: true,
        CNProviderBalanceCell: true,
        OllamaCloudUsageCell: true
      }
    }
  })
}

describe('AccountUsageCell — CodeBuddy 单值余额分支（A2）', () => {
  beforeEach(() => {
    getUsage.mockReset()
    Object.defineProperty(window, 'matchMedia', {
      writable: true,
      value: vi.fn().mockImplementation(() => ({
        matches: true,
        media: '(min-width: 768px)',
        onchange: null,
        addListener: vi.fn(),
        removeListener: vi.fn(),
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        dispatchEvent: vi.fn(),
      }))
    })
  })

  it('有余额：单值形态展示余额（非 opencode 三进度条）', async () => {
    getUsage.mockResolvedValue({
      upstream_balance: { balance: 42.5, status: 'ok' }
    })

    const wrapper = mountCell(makeAccount({ id: 7101 }))

    await flushPromises()

    expect(getUsage).toHaveBeenCalledTimes(1)
    const value = wrapper.get('[data-testid="codebuddy-balance-value"]')
    expect(value.text()).toContain('admin.accounts.codebuddy.usage.balanceLabel')
    // 积分形态：数值 + i18n 单位文案，绝不套货币格式（$）
    expect(value.text()).toContain('42.5 credits')
    expect(value.text()).not.toContain('$')
    expect(wrapper.findAll('.usage-bar')).toHaveLength(0)
  })

  it('空余额：upstream_balance 无数值时显示占位符且仍可手动刷新', async () => {
    getUsage.mockResolvedValue({
      upstream_balance: { status: 'ok' }
    })

    const wrapper = mountCell(makeAccount({ id: 7102 }))

    await flushPromises()

    expect(wrapper.find('[data-testid="codebuddy-balance-value"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="codebuddy-balance-refresh"]').exists()).toBe(true)

    await wrapper.get('[data-testid="codebuddy-balance-refresh"]').trigger('click')
    await flushPromises()

    // 刷新按钮走 active 强刷通道（对齐既有 activeQuery 先例）
    expect(getUsage).toHaveBeenLastCalledWith(7102, 'active', true)
  })

  it('错误态：降级值通道显示失败文案（error/stale）', async () => {
    getUsage.mockResolvedValue({
      upstream_balance: { status: 'error', stale: true, error: 'upstream exploded' }
    })

    const wrapper = mountCell(makeAccount({ id: 7103 }))

    await flushPromises()

    expect(wrapper.get('[data-testid="codebuddy-balance-error"]').text()).toContain('upstream exploded')
  })

  it('错误态：无 error 文本时回退 usageError 文案', async () => {
    getUsage.mockResolvedValue({
      upstream_balance: { status: 'error', stale: true }
    })

    const wrapper = mountCell(makeAccount({ id: 7104 }))

    await flushPromises()

    expect(wrapper.get('[data-testid="codebuddy-balance-error"]').text()).toContain('admin.accounts.usageError')
  })

  it('needsReauth 态：显示重新授权徽章', async () => {
    getUsage.mockResolvedValue({
      needs_reauth: true
    })

    const wrapper = mountCell(makeAccount({ id: 7105 }))

    await flushPromises()

    expect(wrapper.text()).toContain('admin.accounts.needsReauth')
  })

  it('门控：桌面批量托管路径下 codebuddy 仍自行拉取（批量首期不开）', async () => {
    getUsage.mockResolvedValue({
      upstream_balance: { balance: 7, status: 'ok' }
    })

    const wrapper = mountCell(makeAccount({ id: 7106 }), {
      requestBatchedUsage: vi.fn()
    })

    await flushPromises()

    // 父级批量函数对 codebuddy 是摆设：单元格必须自己发 /usage 请求
    expect(getUsage).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[data-testid="codebuddy-balance-value"]').text()).toContain('7')
  })

  it('门控：对照——批量支持平台（opencode）在有批量函数时仍走托管不自取', async () => {
    getUsage.mockResolvedValue({ opencode: { status: 'ok' } })

    const wrapper = mountCell(makeAccount({ id: 7107, platform: 'opencode' }), {
      requestBatchedUsage: vi.fn()
    })

    await flushPromises()

    expect(getUsage).not.toHaveBeenCalled()
  })

  it('门控：codebuddy 不落入底部 Key 账号分支（today stats 徽章不渲染）', async () => {
    getUsage.mockResolvedValue({
      upstream_balance: { balance: 3, status: 'ok' }
    })

    const wrapper = mountCell(makeAccount({ id: 7108 }), {
      todayStats: {
        requests: 10,
        tokens: 1000,
        cost: 0.1,
        standard_cost: 0.1,
        user_cost: 0.1
      }
    })

    await flushPromises()

    expect(wrapper.text()).not.toContain('10 req')
    expect(wrapper.get('[data-testid="codebuddy-balance-value"]').text()).toContain('3')
  })

  it('积分形态：整数余额无小数位，且渲染不含 $ 货币符号', async () => {
    getUsage.mockResolvedValue({
      upstream_balance: { balance: 700, status: 'ok' }
    })

    const wrapper = mountCell(makeAccount({ id: 7109 }))

    await flushPromises()

    const value = wrapper.get('[data-testid="codebuddy-balance-value"]')
    expect(value.text()).toContain('700 credits')
    expect(value.text()).not.toContain('$')
  })
})
