import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import CNProviderQuotaCell from '../CNProviderQuotaCell.vue'
import type { Account } from '@/types'

const { queryQuota } = vi.hoisted(() => ({
  queryQuota: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    cnProviders: { queryQuota }
  }
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => key
  })
}))

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

describe('CNProviderQuotaCell', () => {
  beforeEach(() => {
    queryQuota.mockReset()
  })

  it('keeps the compact quota stack readable inside the account table cell', async () => {
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

    const probeButton = root.get('button')
    expect(probeButton.classes()).toContain('whitespace-nowrap')
    expect(probeButton.classes()).toContain('leading-4')
    await probeButton.trigger('click')
    await flushPromises()

    const tiers = root.findAll('[data-test="cn-provider-quota-tier"]')
    expect(tiers).toHaveLength(2)
    for (const tier of tiers) {
      expect(tier.classes()).toContain('min-w-0')
      expect(tier.classes()).toContain('leading-4')
    }

    const labels = root.findAll('[data-test="cn-provider-quota-label"]')
    expect(labels).toHaveLength(2)
    for (const label of labels) {
      expect(label.classes()).toContain('w-14')
      expect(label.classes()).toContain('whitespace-nowrap')
    }

    expect(queryQuota).toHaveBeenCalledWith(account.id)
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
  beforeEach(() => {
    queryQuota.mockReset()
  })

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
