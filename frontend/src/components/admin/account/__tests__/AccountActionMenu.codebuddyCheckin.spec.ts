import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountActionMenu from '../AccountActionMenu.vue'
import type { Account } from '@/types'

const { checkinMock, showSuccessMock, showInfoMock, showErrorMock } = vi.hoisted(() => ({
  checkinMock: vi.fn(),
  showSuccessMock: vi.fn(),
  showInfoMock: vi.fn(),
  showErrorMock: vi.fn(),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    codebuddy: {
      checkin: checkinMock
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showSuccess: showSuccessMock,
    showInfo: showInfoMock,
    showError: showErrorMock
  })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key}:${JSON.stringify(params)}` : key
    })
  }
})

function makeAccount(overrides: Partial<Account>): Account {
  return {
    id: 1,
    name: 'test-account',
    platform: 'codebuddy',
    type: 'apikey',
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: false,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    session_window_start: null,
    session_window_end: null,
    session_window_status: null,
    ...overrides
  }
}

const position = { top: 100, left: 100 }

// AccountActionMenu 使用 <Teleport to="body">，内容渲染在 document.body。
const getBodyButtons = () => Array.from(document.body.querySelectorAll('button'))

function mountMenu(account: Account) {
  return mount(AccountActionMenu, {
    props: { show: true, account, position },
    attachTo: document.body
  })
}

describe('AccountActionMenu — CodeBuddy 每日签到（A3）', () => {
  beforeEach(() => {
    document.body.innerHTML = ''
    checkinMock.mockReset()
    showSuccessMock.mockReset()
    showInfoMock.mockReset()
    showErrorMock.mockReset()
  })

  it('codebuddy 账号显示「每日签到」入口', () => {
    const wrapper = mountMenu(makeAccount({ id: 31 }))
    expect(document.body.textContent).toContain('admin.accounts.codebuddy.checkin.action')
    wrapper.unmount()
  })

  it('非 codebuddy 账号隐藏「每日签到」入口', () => {
    const wrapper = mountMenu(makeAccount({ id: 32, platform: 'openai', type: 'oauth' }))
    expect(document.body.textContent).not.toContain('admin.accounts.codebuddy.checkin.action')
    wrapper.unmount()
  })

  it('签到成功：toast 携带 credit 与 streak，并关闭菜单', async () => {
    checkinMock.mockResolvedValue({ already_checked_in: false, credit: 5, streak_days: 3 })
    const wrapper = mountMenu(makeAccount({ id: 33 }))

    const btn = getBodyButtons().find(b => b.textContent?.includes('admin.accounts.codebuddy.checkin.action'))
    expect(btn).toBeDefined()
    btn!.click()
    await flushPromises()

    expect(checkinMock).toHaveBeenCalledWith(33)
    expect(showSuccessMock).toHaveBeenCalledWith(
      'admin.accounts.codebuddy.checkin.success:{"credit":5} · admin.accounts.codebuddy.checkin.streak:{"days":3}'
    )
    expect(wrapper.emitted('close')).toBeTruthy()
    wrapper.unmount()
  })

  it('签到成功但无 streak 时只显示 credit', async () => {
    checkinMock.mockResolvedValue({ already_checked_in: false, credit: 2 })
    const wrapper = mountMenu(makeAccount({ id: 34 }))

    const btn = getBodyButtons().find(b => b.textContent?.includes('admin.accounts.codebuddy.checkin.action'))
    btn!.click()
    await flushPromises()

    expect(showSuccessMock).toHaveBeenCalledWith(
      'admin.accounts.codebuddy.checkin.success:{"credit":2}'
    )
    wrapper.unmount()
  })

  it('已签到（already_checked_in）：提示已签到文案', async () => {
    checkinMock.mockResolvedValue({ already_checked_in: true, credit: 0, streak_days: 1 })
    const wrapper = mountMenu(makeAccount({ id: 35 }))

    const btn = getBodyButtons().find(b => b.textContent?.includes('admin.accounts.codebuddy.checkin.action'))
    btn!.click()
    await flushPromises()

    expect(checkinMock).toHaveBeenCalledTimes(1)
    expect(showInfoMock).toHaveBeenCalledWith('admin.accounts.codebuddy.checkin.already')
    expect(showSuccessMock).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('签到失败：显示失败 toast', async () => {
    checkinMock.mockRejectedValue(new Error('boom'))
    const wrapper = mountMenu(makeAccount({ id: 36 }))

    const btn = getBodyButtons().find(b => b.textContent?.includes('admin.accounts.codebuddy.checkin.action'))
    btn!.click()
    await flushPromises()

    expect(showErrorMock).toHaveBeenCalledWith('admin.accounts.codebuddy.checkin.failed')
    expect(showSuccessMock).not.toHaveBeenCalled()
    wrapper.unmount()
  })
})
