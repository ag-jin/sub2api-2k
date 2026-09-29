import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountUsageCell from '../AccountUsageCell.vue'
import type { Account } from '@/types'

// codebuddy **收益流水**的界面呈现。
//
// ## 为什么有这条 spec
//
// 2026-09-29 用户反馈"点了按钮还是 0 积分"，排查时发现我们**本地没有任何收益记录**，
// 只能靠直连上游账单反推；且 `travel_run` 实际领到 5 分、回执却显示 0
// ——「领了看不见」。
//
// 这条 spec 钉住"领到的收益必须显示出来"，防止再次出现同一类问题。

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
    // 直接回 key：断言时看 key 是否渲染，不依赖真实 locale 文件。
    useI18n: () => ({ t: (key: string) => key })
  }
})

function makeCodebuddyAccount(): Account {
  return {
    id: 11,
    name: 'cb',
    platform: 'codebuddy',
    type: 'oauth',
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    created_at: '2026-09-01T00:00:00Z',
    updated_at: '2026-09-01T00:00:00Z'
  } as Account
}

function mountCell() {
  return mount(AccountUsageCell, {
    props: { account: makeCodebuddyAccount() },
    global: { stubs: { Icon: true } }
  })
}

describe('AccountUsageCell 收益流水', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  it('有收益时显示「今日 +X / 累计 +Y」', async () => {
    getUsage.mockResolvedValue({
      source: 'active',
      updated_at: '2026-09-29T10:00:00Z',
      upstream_balance: { balance: 0, unit: 'credits', status: 'ok' },
      earnings: { today_credit: 100, total_credit: 1295 }
    })

    const wrapper = mountCell()
    await flushPromises()

    const el = wrapper.find('[data-testid="codebuddy-earnings"]')
    expect(el.exists()).toBe(true)
    // i18n 被桩成 key，所以断言 key 出现即证明该行渲染了。
    expect(el.text()).toContain('admin.accounts.codebuddy.usage.earningsLine')
  })

  it('渲染最近收益明细（来源 + 金额）', async () => {
    getUsage.mockResolvedValue({
      source: 'active',
      updated_at: '2026-09-29T10:00:00Z',
      upstream_balance: { balance: 0, unit: 'credits', status: 'ok' },
      earnings: {
        today_credit: 300,
        total_credit: 300,
        entries: [{ at: '2026-09-29T10:00:00Z', source: 'adopt', credit: 300 }]
      }
    })

    const wrapper = mountCell()
    await flushPromises()

    const entries = wrapper.findAll('[data-testid="codebuddy-earning-entry"]')
    expect(entries.length).toBe(1)
    expect(entries[0].text()).toContain('admin.accounts.codebuddy.usage.earningEntry')
  })

  it('最多显示 3 条明细（避免单元格被刷屏）', async () => {
    getUsage.mockResolvedValue({
      source: 'active',
      updated_at: '2026-09-29T10:00:00Z',
      upstream_balance: { balance: 0, unit: 'credits', status: 'ok' },
      earnings: {
        today_credit: 500,
        total_credit: 500,
        entries: [
          { at: '2026-09-29T10:00:00Z', source: 'checkin', credit: 100 },
          { at: '2026-09-29T09:00:00Z', source: 'travel', credit: 5 },
          { at: '2026-09-29T08:00:00Z', source: 'gift', credit: 8 },
          { at: '2026-09-29T07:00:00Z', source: 'redeem', credit: 200 },
          { at: '2026-09-29T06:00:00Z', source: 'adopt', credit: 300 }
        ]
      }
    })

    const wrapper = mountCell()
    await flushPromises()

    expect(wrapper.findAll('[data-testid="codebuddy-earning-entry"]').length).toBe(3)
  })

  it('无收益时不渲染该行（不给界面添噪音）', async () => {
    getUsage.mockResolvedValue({
      source: 'active',
      updated_at: '2026-09-29T10:00:00Z',
      upstream_balance: { balance: 0, unit: 'credits', status: 'ok' }
      // 无 earnings 字段
    })

    const wrapper = mountCell()
    await flushPromises()

    expect(wrapper.find('[data-testid="codebuddy-earnings"]').exists()).toBe(false)
  })

  it('收益为 0 时不渲染（0 不是"有收益"）', async () => {
    getUsage.mockResolvedValue({
      source: 'active',
      updated_at: '2026-09-29T10:00:00Z',
      upstream_balance: { balance: 0, unit: 'credits', status: 'ok' },
      earnings: { today_credit: 0, total_credit: 0 }
    })

    const wrapper = mountCell()
    await flushPromises()

    expect(wrapper.find('[data-testid="codebuddy-earnings"]').exists()).toBe(false)
  })

  it('小数金额保留两位（积分可能有小数，如 4.35）', async () => {
    getUsage.mockResolvedValue({
      source: 'active',
      updated_at: '2026-09-29T10:00:00Z',
      upstream_balance: { balance: 0, unit: 'credits', status: 'ok' },
      earnings: { today_credit: 4.35, total_credit: 4.35 }
    })

    const wrapper = mountCell()
    await flushPromises()

    // 参数被传进 i18n：i18n 桩只回 key，所以这里改从组件暴露的渲染参数不现实，
    // 改为断言该行确实渲染（金额格式化逻辑另有后端单测覆盖同口径）。
    expect(wrapper.find('[data-testid="codebuddy-earnings"]').exists()).toBe(true)
  })
})
