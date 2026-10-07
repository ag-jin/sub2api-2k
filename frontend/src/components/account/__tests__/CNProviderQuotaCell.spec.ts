import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import CNProviderQuotaCell from '../CNProviderQuotaCell.vue'
import { resetZhipuSignStatusCache } from '@/composables/useZhipuSignStatus'
import UsageProgressBar from '../UsageProgressBar.vue'
import type { Account } from '@/types'

const { queryQuota, getSignStatus } = vi.hoisted(() => ({
  queryQuota: vi.fn(),
  getSignStatus: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    cnProviders: { queryQuota }
  }
}))

// 保留 vue-i18n 真实导出：UsageProgressBar 依赖 @/utils/format → @/i18n，
// 其模块级 createI18n 需要真实 createI18n 存在。
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

// 票 30 的签名降级徽标读取签名状态（票 28 的 GET /admin/zhipu/sign/status）。
vi.mock('@/api/admin/zhipu', () => ({ getSignStatus }))

const account = {
  id: 7,
  platform: 'zhipu',
  type: 'apikey',
  credentials: { account_mode: 'coding' },
  extra: {
    zhipu_5h_used_percent: 0,
    zhipu_weekly_used_percent: 27,
    zhipu_5h_reset_at: '2026-08-18T12:30:00+08:00',
    zhipu_weekly_reset_at: '2026-08-22T00:00:00+08:00',
    zhipu_usage_updated_at: new Date().toISOString()
  }
} as Account

// 每个用例都从干净的 mock 与空缓存起步；默认「未启用签名」，降级徽标用例自行覆盖
// 数据源，既有用例不因降级徽标的数据源变化而改变断言。
beforeEach(() => {
  queryQuota.mockReset()
  getSignStatus.mockReset()
  getSignStatus.mockResolvedValue({ sign_v4_enabled: false, accounts: [] })
  resetZhipuSignStatusCache()
})

describe('CNProviderQuotaCell', () => {
  it('renders tier rows through the shared UsageProgressBar inside the account table cell', async () => {
    queryQuota.mockResolvedValue({
      success: true,
      tiers: [
        { window: '5h', used_percent: 0, reset_at: '2026-08-18T12:30:00+08:00' },
        { window: 'weekly', used_percent: 27, reset_at: '2026-08-22T00:00:00+08:00' }
      ]
    })
    const wrapper = mount(CNProviderQuotaCell, { props: { account } })

    const root = wrapper.get('[data-test="cn-provider-quota"]')
    expect(root.classes()).toContain('min-w-[220px]')

    // 新鲜快照：挂载即渲染条形图，不触发探测
    await flushPromises()
    expect(queryQuota).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('27%')

    // probe 按钮文案是动词 key（i18n mock 返回 key 本身），点击触发查询
    const probeButton = root.get('[data-test="cn-provider-quota-probe"]')
    expect(probeButton.text()).toBe('admin.accounts.cnProviders.probe')
    await probeButton.trigger('click')
    await flushPromises()
    expect(queryQuota).toHaveBeenCalledWith(account.id)

    // tier 行由 UsageProgressBar 渲染：数量、label/color/utilization/reset 逐行对齐
    expect(root.findAll('[data-test="cn-provider-quota-tier"]')).toHaveLength(2)
    const bars = root.findAllComponents(UsageProgressBar)
    expect(bars).toHaveLength(2)
    expect(bars[0].props('label')).toBe('admin.accounts.cnProviders.window5h')
    expect(bars[0].props('utilization')).toBe(0)
    expect(bars[0].props('color')).toBe('indigo')
    expect(bars[0].props('resetsAt')).toBe('2026-08-18T12:30:00+08:00')
    expect(bars[1].props('label')).toBe('admin.accounts.cnProviders.windowWeekly')
    expect(bars[1].props('utilization')).toBe(27)
    expect(bars[1].props('color')).toBe('emerald')
    expect(bars[1].props('resetsAt')).toBe('2026-08-22T00:00:00+08:00')
  })

  it('labels the refresh control with an explicit action verb, not a data caption', async () => {
    const wrapper = mount(CNProviderQuotaCell, { props: { account } })
    await flushPromises()

    // The snapshot is fresh (usage_updated_at = now): bars render without probing.
    expect(queryQuota).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('27%')

    // The control reads as an action ("query"), unlike the old noun label
    // ("5-hour window/weekly window") which looked like a passive caption.
    // The i18n mock returns the key itself.
    const probeButton = wrapper.get('[data-test="cn-provider-quota-probe"]')
    expect(probeButton.text()).toBe('admin.accounts.cnProviders.probe')

    await probeButton.trigger('click')
    await flushPromises()
    expect(queryQuota).toHaveBeenCalledWith(account.id)
  })
})

describe('CNProviderQuotaCell needs-relogin badge', () => {
  it('flags the cell when the managed sign-in expired, without exposing credential details', () => {
    const flagged = {
      ...account,
      extra: { ...account.extra, zhipu_needs_relogin: true }
    } as Account
    const wrapper = mount(CNProviderQuotaCell, { props: { account: flagged } })

    const badge = wrapper.get('[data-test="cn-provider-quota-needs-relogin"]')
    expect(badge.text()).toBe('admin.accounts.cnProviders.zhipuLogin.needsRelogin')
    expect(badge.attributes('title')).toBe(
      'admin.accounts.cnProviders.zhipuLogin.needsReloginTooltip'
    )
    // 列表徽标只提示，不触发任何操作（重登入口在账号编辑弹窗）。
    expect(badge.find('button').exists()).toBe(false)
    wrapper.unmount()
  })

  it('omits the badge when the flag is false or absent', () => {
    const variants = [
      { ...account.extra, zhipu_needs_relogin: false },
      { ...account.extra }
    ]
    for (const extra of variants) {
      const wrapper = mount(CNProviderQuotaCell, {
        props: { account: { ...account, extra } as Account }
      })
      expect(wrapper.find('[data-test="cn-provider-quota-needs-relogin"]').exists()).toBe(false)
      wrapper.unmount()
    }
  })
})

// 票 30 / ui-panels §6.3：VERIFY_* 降级 + 账号级熔断徽标（有 / 无 / 字段缺失三态）。
describe('CNProviderQuotaCell sign-degraded badge', () => {
  function makeStatus(overrides: Record<string, unknown> = {}) {
    return {
      sign_v4_enabled: true,
      sign_fail_policy: 'open',
      accounts: [
        {
          account_id: account.id,
          account_name: 'zhipu-managed',
          key_cached: true,
          last_handshake_at: '2026-10-06T00:00:00Z',
          key_expires_at: '2026-10-06T06:00:00Z',
          consecutive_failures: 12,
          circuit_break_tripped: true,
          circuit_break_reason: 'verify_invalid > 10 in 5m',
          runtime_state_available: true
        }
      ],
      ...overrides
    }
  }

  it('shows an amber non-interactive badge when the account signing is circuit-broken', async () => {
    getSignStatus.mockResolvedValue(makeStatus())
    const wrapper = mount(CNProviderQuotaCell, { props: { account } })
    await flushPromises()

    const badge = wrapper.get('[data-test="cn-provider-quota-sign-degraded"]')
    expect(badge.text()).toBe('admin.accounts.cnProviders.zhipuSign.degraded')
    expect(badge.html()).toContain('text-amber')
    // tooltip = 本地化文案 + 后端熔断原因（可定位到具体 VERIFY_* 类别）
    expect(badge.attributes('title')).toBe(
      'admin.accounts.cnProviders.zhipuSign.degradedTooltip · verify_invalid > 10 in 5m'
    )
    // 列表徽标只提示，不伪装成可点击（无对应告警详情链接时不提供入口）
    expect(badge.find('button').exists()).toBe(false)
    expect(badge.element.tagName).toBe('SPAN')
    expect(getSignStatus).toHaveBeenCalledTimes(1)
  })

  it('renders both zhipu badges in order: re-login first, signing degraded second', async () => {
    getSignStatus.mockResolvedValue(makeStatus())
    const flagged = {
      ...account,
      extra: { ...account.extra, zhipu_needs_relogin: true }
    } as Account
    const wrapper = mount(CNProviderQuotaCell, { props: { account: flagged } })
    await flushPromises()

    const row = wrapper.get('[data-test="cn-provider-quota-needs-relogin"]').element.parentElement as HTMLElement
    const order = Array.from(row.querySelectorAll('[data-test$="-needs-relogin"], [data-test$="-sign-degraded"]')).map(
      (el) => el.getAttribute('data-test')
    )
    expect(order).toEqual([
      'cn-provider-quota-needs-relogin',
      'cn-provider-quota-sign-degraded',
    ])
  })

  it('omits the badge when the account is not circuit-broken, unknown, or absent from the status', async () => {
    const variants: Array<Record<string, unknown>> = [
      // 已接线且未熔断
      { accounts: [{ ...makeStatus().accounts[0], circuit_break_tripped: false }] },
      // 运行时状态源未接线：渲染「未知」而不是「未熔断」
      { accounts: [{ ...makeStatus().accounts[0], runtime_state_available: false }] },
      // 账号不在签名状态列表里（未启用签名/未托管）
      { accounts: [] },
      // 全局签名开关关闭
      { sign_v4_enabled: false },
    ]

    for (const [index, overrides] of variants.entries()) {
      getSignStatus.mockResolvedValue(makeStatus(overrides))
      resetZhipuSignStatusCache()
      const wrapper = mount(CNProviderQuotaCell, { props: { account } })
      await flushPromises()
      expect(`${index}: ${wrapper.find('[data-test="cn-provider-quota-sign-degraded"]').exists()}`).toBe(
        `${index}: false`
      )
      wrapper.unmount()
    }
  })

  it('stays quiet when the sign status read fails, and never calls it without signing enabled', async () => {
    getSignStatus.mockRejectedValue(new Error('forbidden'))
    const wrapper = mount(CNProviderQuotaCell, { props: { account } })
    await flushPromises()
    expect(wrapper.find('[data-test="cn-provider-quota-sign-degraded"]').exists()).toBe(false)
    // 配额单元格照常渲染
    expect(wrapper.get('[data-test="cn-provider-quota"]').text()).toContain('27%')

    // kimi 之类的非智谱 coding 账号不读签名状态
    resetZhipuSignStatusCache()
    getSignStatus.mockClear()
    const kimi = {
      ...account,
      platform: 'kimi',
      extra: { kimi_5h_used_percent: 10, kimi_usage_updated_at: new Date().toISOString() }
    } as unknown as Account
    const kimiWrapper = mount(CNProviderQuotaCell, { props: { account: kimi } })
    await flushPromises()
    expect(getSignStatus).not.toHaveBeenCalled()
    expect(kimiWrapper.find('[data-test="cn-provider-quota-sign-degraded"]').exists()).toBe(false)
  })

  it('fetches the sign status once for many cells (module-level cache)', async () => {
    getSignStatus.mockResolvedValue(makeStatus())
    const cells = [0, 1, 2].map((offset) =>
      mount(CNProviderQuotaCell, {
        props: { account: { ...account, id: account.id + offset } as Account }
      })
    )
    await flushPromises()

    expect(getSignStatus).toHaveBeenCalledTimes(1)
    // 只有列表里真正熔断的那个账号（id=7）带徽标
    expect(cells[0].find('[data-test="cn-provider-quota-sign-degraded"]').exists()).toBe(true)
    expect(cells[1].find('[data-test="cn-provider-quota-sign-degraded"]').exists()).toBe(false)
    expect(cells[2].find('[data-test="cn-provider-quota-sign-degraded"]').exists()).toBe(false)
    for (const cell of cells) cell.unmount()
  })
})
