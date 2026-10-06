import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import type { MonitorQuotaSnapshot } from '@/api/admin/channelMonitor'
import enDashboard from '@/i18n/locales/en/dashboard'
import zhDashboard from '@/i18n/locales/zh/dashboard'
import MonitorQuotaView from '../MonitorQuotaView.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    // te() 恒真：已知 token 直接返回 i18n key，便于断言 window/label 映射。
    // 带插值的调用把参数一并透出（`key|{"count":2}`），便于断言张数等数值确实传给了 i18n。
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key}|${JSON.stringify(params)}` : key,
      te: () => true,
    }),
  }
})

function makeSnapshot(overrides: Partial<MonitorQuotaSnapshot> = {}): MonitorQuotaSnapshot {
  return {
    source: 'usage',
    success: true,
    fetched_at: '2026-08-18T00:00:00Z',
    ...overrides,
  }
}

/** 快照新鲜度以 fetched_at 相对当前时间判定，测试用相对时间构造。 */
function fetchedAtMinutesAgo(minutes: number): string {
  return new Date(Date.now() - minutes * 60_000).toISOString()
}

/** 重置卡到期时间：以「距今 N 天」构造，避免测试与实现用同一套日期算术。 */
function daysFromNow(days: number): string {
  return new Date(Date.now() + days * 24 * 60 * 60 * 1000).toISOString()
}

describe('MonitorQuotaView', () => {
  it('renders nothing without a snapshot', () => {
    const wrapper = mount(MonitorQuotaView, { props: { snapshot: null } })
    expect(wrapper.find('[data-testid="monitor-quota-view"]').exists()).toBe(false)
    expect(wrapper.text()).toBe('')
  })

  it('maps tier window/label tokens through i18n and colors utilization', () => {
    const wrapper = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          tiers: [
            { window: '5h', used_percent: 42.4 },
            { window: '7d', label: 'pro', used_percent: 80 },
            { window: 'weekly', label: 'unknown-token', used_percent: 95 },
          ],
        }),
      },
    })

    const rows = wrapper.findAll('[data-testid="monitor-quota-view"] .flex.items-center')
    // tier 行只在 success 且有数据时渲染
    expect(rows.length).toBeGreaterThanOrEqual(3)
    const text = wrapper.text()
    // 已知 window token 走 i18n
    expect(text).toContain('monitorCommon.quota.windows.5h')
    expect(text).toContain('monitorCommon.quota.windows.7d')
    // 已知 label token 拼成 label/window
    expect(text).toContain('monitorCommon.quota.labels.pro/monitorCommon.quota.windows.7d')
    // 未知 label 原样透出（前向兼容）
    expect(text).toContain('unknown-token/monitorCommon.quota.windows.weekly')
    // 百分比取整
    expect(text).toContain('42%')
    expect(text).toContain('80%')
    expect(text).toContain('95%')

    const html = wrapper.html()
    // 阈值配色：≥90 红 / ≥75 黄 / 其余绿（与账号页 CNProviderQuotaCell 对齐）
    expect(html).toContain('bg-emerald-500')
    expect(html).toContain('bg-amber-500')
    expect(html).toContain('bg-red-500')
  })

  it('clamps the tier bar width into 0-100', () => {
    const wrapper = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({ tiers: [{ window: '5h', used_percent: 240 }] }),
      },
    })
    expect(wrapper.html()).toContain('width: 100%')
  })

  it('shows the plan level badge and multi-currency balances', () => {
    const wrapper = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          plan_level: 'Max20',
          balances: [
            { currency: 'CNY', balance: 12.5 },
            { currency: 'USD', balance: 0 },
          ],
        }),
      },
    })

    expect(wrapper.text()).toContain('Max20')
    expect(wrapper.text()).toContain('12.50 CNY')
    expect(wrapper.text()).toContain('0.00 USD')
    // 余额为 0 用红色警示
    expect(wrapper.html()).toContain('text-red-600')
  })

  it('falls back to the single balance + currency pair', () => {
    const wrapper = mount(MonitorQuotaView, {
      props: { snapshot: makeSnapshot({ balance: 3.2, currency: 'CNY' }) },
    })
    expect(wrapper.text()).toContain('3.20 CNY')
  })

  it('renders a truncated error state when the fetch failed', () => {
    const longError = 'x'.repeat(60)
    const wrapper = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({ success: false, error: longError }),
      },
    })

    const error = wrapper.get('[data-testid="monitor-quota-error"]')
    expect(error.text()).toBe(`${'x'.repeat(48)}…`)
    expect(error.attributes('title')).toBe(longError)
  })

  it('keeps failed snapshots from rendering tier rows', () => {
    const wrapper = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          success: false,
          tiers: [{ window: '5h', used_percent: 10 }],
        }),
      },
    })
    expect(wrapper.text()).not.toContain('10%')
  })
})

// 智谱登录托管账号：逐模型积分明细（design M4 `model_credits`）。
describe('MonitorQuotaView model credits panel', () => {
  it('renders credits sorted by date then credits, with compact numbers and exact titles', () => {
    const wrapper = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          fetched_at: fetchedAtMinutesAgo(1),
          model_credits: [
            { model: 'glm-4.5', date: '2026-10-04', input_tokens: 300, cached_tokens: 0, output_tokens: 5, credits: 9 },
            { model: 'glm-4.6', date: '2026-10-05', input_tokens: 1200, cached_tokens: 300, output_tokens: 45, credits: 0.5 },
            { model: 'glm-4.6-flash', date: '2026-10-05', input_tokens: 2000, cached_tokens: 100, output_tokens: 10, credits: 2.5 },
          ],
        }),
      },
    })

    const panel = wrapper.get('[data-testid="zhipu-model-credits-panel"]')
    // 日期降序；同日按积分降序（2.5 在 0.5 之前）
    const models = panel.findAll('[data-testid="zhipu-model-credits-model"]')
    expect(models.map((el) => el.attributes('title'))).toEqual([
      'glm-4.6-flash',
      'glm-4.6',
      'glm-4.5',
    ])

    // Tokens 按项目 K/M 格式缩写，精确值放 title
    const inputs = panel.findAll('[data-testid="zhipu-model-credits-input_tokens"]')
    expect(inputs.map((el) => el.text())).toEqual(['2.0K', '1.2K', '300'])
    expect(inputs[1].attributes('title')).toBe('1200')

    const credits = panel.findAll('[data-testid="zhipu-model-credits-credits"]')
    expect(credits.map((el) => el.text())).toEqual(['2.5', '0.5', '9'])

    expect(panel.text()).not.toContain('NaN')
    expect(panel.text()).toContain('monitorCommon.credits.title')
  })

  it('truncates long model names but keeps the full name and raw date accessible', () => {
    const longModel = `glm-4.6-${'x'.repeat(80)}`
    const invalidDate = '2026-13-45'
    const wrapper = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          fetched_at: fetchedAtMinutesAgo(1),
          model_credits: [
            { model: longModel, date: invalidDate, input_tokens: 10, cached_tokens: 0, output_tokens: 0, credits: 1 },
          ],
        }),
      },
    })

    const panel = wrapper.get('[data-testid="zhipu-model-credits-panel"]')
    const model = panel.get('[data-testid="zhipu-model-credits-model"]')
    expect(model.classes()).toContain('truncate')
    expect(model.classes()).toContain('max-w-[280px]')
    expect(model.attributes('title')).toBe(longModel)

    // 非法日期不得被 Date 静默进位成另一个日期：原样展示 + title 保留原始字段
    const date = panel.get('[data-testid="zhipu-model-credits-date"]')
    expect(date.text()).toBe(invalidDate)
    expect(date.attributes('title')).toBe(invalidDate)
    expect(panel.text()).not.toContain('Invalid Date')
  })

  it('hides the credits panel for legacy snapshots without model_credits', () => {
    const wrapper = mount(MonitorQuotaView, {
      props: { snapshot: makeSnapshot({ fetched_at: fetchedAtMinutesAgo(1) }) },
    })
    expect(wrapper.find('[data-testid="zhipu-model-credits-panel"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="zhipu-reset-cards-panel"]').exists()).toBe(false)
  })

  it('renders the credits empty state for an empty list and keeps partial rows with "-"', () => {
    const empty = mount(MonitorQuotaView, {
      props: { snapshot: makeSnapshot({ fetched_at: fetchedAtMinutesAgo(1), model_credits: [] }) },
    })
    expect(empty.get('[data-testid="zhipu-model-credits-empty"]').text()).toContain(
      'monitorCommon.credits.empty'
    )

    // 部分字段缺失：保留行与其余字段，缺失值为 "-"，不与 0 混淆
    const partial = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          fetched_at: fetchedAtMinutesAgo(1),
          model_credits: [
            { model: 'glm-4.6', date: '2026-10-05', input_tokens: 1500, credits: 1 },
          ],
        }),
      },
    })
    const panel = partial.get('[data-testid="zhipu-model-credits-panel"]')
    expect(panel.get('[data-testid="zhipu-model-credits-cached_tokens"]').text()).toBe('-')
    expect(panel.get('[data-testid="zhipu-model-credits-output_tokens"]').text()).toBe('-')
    expect(panel.get('[data-testid="zhipu-model-credits-input_tokens"]').text()).toBe('1.5K')
    expect(panel.text()).not.toContain('NaN')
    expect(panel.text()).not.toContain('undefined')
  })

  it('renders field cards instead of a desktop table on a narrow viewport', () => {
    const previousMatchMedia = window.matchMedia
    Object.defineProperty(window, 'matchMedia', {
      writable: true,
      value: vi.fn().mockImplementation((query: string) => ({
        matches: false,
        media: query,
        onchange: null,
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        addListener: vi.fn(),
        removeListener: vi.fn(),
        dispatchEvent: vi.fn(),
      })),
    })

    try {
      const wrapper = mount(MonitorQuotaView, {
        props: {
          snapshot: makeSnapshot({
            fetched_at: fetchedAtMinutesAgo(1),
            model_credits: [
              { model: 'glm-4.6', date: '2026-10-05', input_tokens: 1200, cached_tokens: 0, output_tokens: 1, credits: 1 },
              { model: 'glm-4.5', date: '2026-10-04', input_tokens: 300, cached_tokens: 0, output_tokens: 1, credits: 2 },
            ],
          }),
        },
      })

      const panel = wrapper.get('[data-testid="zhipu-model-credits-panel"]')
      // 窄屏（~430px）不保留压缩桌面表格：复用 DataTable 的字段卡片模式
      expect(panel.find('table').exists()).toBe(false)
      expect(panel.findAll('[data-field="model"]')).toHaveLength(2)
      expect(panel.findAll('[data-field="credits"]')).toHaveLength(2)
      expect(panel.findAll('[data-testid="zhipu-model-credits-model"]')).toHaveLength(2)
      expect(panel.text()).not.toContain('NaN')
    } finally {
      Object.defineProperty(window, 'matchMedia', { writable: true, value: previousMatchMedia })
    }
  })

  it('keeps the desktop table with all six columns on a wide viewport', () => {
    const wrapper = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          fetched_at: fetchedAtMinutesAgo(1),
          model_credits: [
            { model: 'glm-4.6', date: '2026-10-05', input_tokens: 1200, cached_tokens: 0, output_tokens: 1, credits: 1 },
          ],
        }),
      },
    })

    const panel = wrapper.get('[data-testid="zhipu-model-credits-panel"]')
    expect(panel.find('table').exists()).toBe(true)
    const headers = panel.findAll('thead th').map((th) => th.text())
    expect(headers).toEqual([
      'monitorCommon.credits.columns.date',
      'monitorCommon.credits.columns.model',
      'monitorCommon.credits.columns.inputTokens',
      'monitorCommon.credits.columns.cachedTokens',
      'monitorCommon.credits.columns.outputTokens',
      'monitorCommon.credits.columns.credits',
    ])
  })

  it('falls back to the credits unavailable state when no row carries any detail field', () => {
    const wrapper = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          fetched_at: fetchedAtMinutesAgo(1),
          model_credits: [{ model: 'glm-4.6', date: '2026-10-05' } as never],
        }),
      },
    })
    const panel = wrapper.get('[data-testid="zhipu-model-credits-panel"]')
    expect(panel.get('[data-testid="zhipu-model-credits-unavailable"]').text()).toContain(
      'monitorCommon.credits.unavailable'
    )
    expect(panel.findAll('[data-testid="zhipu-model-credits-model"]')).toHaveLength(0)
  })
})

// 智谱登录托管账号：重置卡只读卡片（design M4 `reset_cards`，R0 严格只读）。
describe('MonitorQuotaView reset cards panel', () => {
  it('shows per-type counts and the nearest expiry state', () => {
    const nearestFiveHour = daysFromNow(5)
    const wrapper = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          fetched_at: fetchedAtMinutesAgo(1),
          reset_cards: [
            { type: 'five_hour', expire_at: daysFromNow(6) },
            { type: 'five_hour', expire_at: nearestFiveHour },
            { type: 'week', expire_at: daysFromNow(30) },
          ],
        }),
      },
    })

    const panel = wrapper.get('[data-testid="zhipu-reset-cards-panel"]')
    expect(panel.get('[data-testid="zhipu-reset-cards-readonly"]').text()).toBe(
      'monitorCommon.resetCards.readOnly'
    )

    // 张数取同类型卡片数量（2 / 1），"最近到期"取最小值（2 天后那张）
    const fiveHour = panel.get('[data-testid="zhipu-reset-card-five_hour"]')
    expect(fiveHour.attributes('data-state')).toBe('expiring')
    expect(fiveHour.get('[data-testid="zhipu-reset-card-count"]').text()).toContain(
      'monitorCommon.resetCards.count|{"count":2}'
    )
    const fiveHourExpiry = fiveHour.get('[data-testid="zhipu-reset-card-expiry"]')
    expect(fiveHourExpiry.text()).toContain('monitorCommon.resetCards.expiring')
    expect(fiveHourExpiry.attributes('title')).toBe(nearestFiveHour)
    expect(fiveHourExpiry.html()).toContain('text-amber-600')

    const week = panel.get('[data-testid="zhipu-reset-card-week"]')
    expect(week.attributes('data-state')).toBe('ok')
    expect(week.get('[data-testid="zhipu-reset-card-count"]').text()).toContain('{"count":1}')
    expect(week.get('[data-testid="zhipu-reset-card-expiry"]').text()).toContain(
      'monitorCommon.resetCards.expiresIn'
    )

    expect(panel.text()).not.toContain('NaN')
    expect(panel.text()).not.toContain('Invalid Date')
  })

  // 票 17：到期分档 有效(>7d) / 注意(≤7d) / 紧急(≤3d) / 已过期 / 未知。
  const expiryTiers: Array<[number, string]> = [
    [10, 'ok'],
    [6, 'expiring'],
    [2.5, 'expiring_soon'],
    [-0.5, 'expired'],
  ]

  it.each(expiryTiers)(
    'tiers a card expiring in %s days as %s',
    (days: number, expectedState: string) => {
      const wrapper = mount(MonitorQuotaView, {
        props: {
          snapshot: makeSnapshot({
            fetched_at: fetchedAtMinutesAgo(1),
            reset_cards: [{ type: 'five_hour', expire_at: daysFromNow(days) }],
          }),
        },
      })

      const fiveHour = wrapper.get('[data-testid="zhipu-reset-card-five_hour"]')
      expect(fiveHour.attributes('data-state')).toBe(expectedState)
      // 张数与到期时间在任何分档下都必须仍然可见
      expect(fiveHour.get('[data-testid="zhipu-reset-card-count"]').text()).toContain('{"count":1}')
      expect(fiveHour.get('[data-testid="zhipu-reset-card-expiry"]').exists()).toBe(true)
      expect(wrapper.text()).not.toContain('Invalid Date')
    }
  )

  it('escalates cards expiring within 3 days and states the void-on-expiry reminder', () => {
    const nearest = daysFromNow(2)
    const wrapper = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          fetched_at: fetchedAtMinutesAgo(1),
          reset_cards: [
            { type: 'five_hour', expire_at: nearest },
            { type: 'week', expire_at: daysFromNow(10) },
          ],
        }),
      },
    })

    const panel = wrapper.get('[data-testid="zhipu-reset-cards-panel"]')
    const fiveHour = panel.get('[data-testid="zhipu-reset-card-five_hour"]')
    expect(fiveHour.attributes('data-state')).toBe('expiring_soon')
    const expiry = fiveHour.get('[data-testid="zhipu-reset-card-expiry"]')
    expect(expiry.text()).toContain('monitorCommon.resetCards.expiring')
    expect(expiry.attributes('title')).toBe(nearest)
    // 3 天档是强调档：比 ≤7 天档更重的 amber（浅底 + 更深文字），但不与已过期的 red 混淆
    expect(expiry.html()).toContain('text-amber-700')
    expect(expiry.html()).toContain('bg-amber-100')
    expect(expiry.html()).not.toContain('text-red-600')

    // 信息性提醒：到期未使用将失效（不是操作引导）
    const notice = fiveHour.get('[data-testid="zhipu-reset-card-void-notice"]')
    expect(notice.text()).toBe('monitorCommon.resetCards.expiringSoonNotice')
    expect(notice.html()).toContain('text-amber-700')

    // ≤7 天但 >3 天的卡不受影响
    expect(panel.get('[data-testid="zhipu-reset-card-week"]').attributes('data-state')).toBe('ok')
    expect(panel.text()).not.toContain('NaN')
  })

  it('marks expired and unknown-expiry cards as unusable/unknown', () => {
    const wrapper = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          fetched_at: fetchedAtMinutesAgo(1),
          reset_cards: [
            { type: 'five_hour', expire_at: daysFromNow(-1) },
            { type: 'week', expire_at: 'not-a-date' },
          ],
        }),
      },
    })

    const panel = wrapper.get('[data-testid="zhipu-reset-cards-panel"]')
    const fiveHour = panel.get('[data-testid="zhipu-reset-card-five_hour"]')
    expect(fiveHour.attributes('data-state')).toBe('expired')
    const fiveHourExpiry = fiveHour.get('[data-testid="zhipu-reset-card-expiry"]')
    expect(fiveHourExpiry.text()).toContain('monitorCommon.resetCards.expired')
    expect(fiveHourExpiry.html()).toContain('text-red-600')
    // 卡片仍显示最后返回张数，但不描述为可用
    expect(fiveHour.get('[data-testid="zhipu-reset-card-count"]').text()).toContain('{"count":1}')

    const week = panel.get('[data-testid="zhipu-reset-card-week"]')
    expect(week.attributes('data-state')).toBe('unknown')
    const weekExpiry = week.get('[data-testid="zhipu-reset-card-expiry"]')
    expect(weekExpiry.text()).toBe('monitorCommon.resetCards.unknownExpiry')
    // 非法日期不得渲染成 Invalid Date（到期未知时 title 缺省，保留原始字段由后端排查）
    expect(weekExpiry.attributes('title')).toBeUndefined()
    expect(week.get('[data-testid="zhipu-reset-card-count"]').text()).toContain('{"count":1}')
    expect(panel.text()).not.toContain('Invalid Date')
  })

  it('shows a type-empty placeholder instead of fabricating zero cards', () => {
    const wrapper = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          fetched_at: fetchedAtMinutesAgo(1),
          reset_cards: [{ type: 'five_hour', expire_at: daysFromNow(10) }],
        }),
      },
    })

    const panel = wrapper.get('[data-testid="zhipu-reset-cards-panel"]')
    const week = panel.get('[data-testid="zhipu-reset-card-week"]')
    expect(week.get('[data-testid="zhipu-reset-card-empty"]').text()).toBe(
      'monitorCommon.resetCards.typeEmpty'
    )
    expect(week.find('[data-testid="zhipu-reset-card-count"]').exists()).toBe(false)
    // 另一类型不受影响
    expect(panel.get('[data-testid="zhipu-reset-card-five_hour"]').text()).toContain('{"count":1}')

    // 两类都缺失 → 整面板空态
    const none = mount(MonitorQuotaView, {
      props: { snapshot: makeSnapshot({ fetched_at: fetchedAtMinutesAgo(1), reset_cards: [] }) },
    })
    const emptyPanel = none.get('[data-testid="zhipu-reset-cards-panel"]')
    expect(emptyPanel.get('[data-testid="zhipu-reset-cards-empty"]').text()).toContain(
      'monitorCommon.resetCards.empty'
    )
    expect(emptyPanel.find('[data-testid="zhipu-reset-card-five_hour"]').exists()).toBe(false)
  })

  it('surfaces the most severe expiry hint in the panel footer', () => {
    const urgent = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          fetched_at: fetchedAtMinutesAgo(1),
          reset_cards: [
            { type: 'five_hour', expire_at: daysFromNow(1) },
            { type: 'week', expire_at: daysFromNow(20) },
          ],
        }),
      },
    })
    const urgentNote = urgent.get('[data-testid="zhipu-reset-cards-expiring-soon"]')
    expect(urgentNote.text()).toBe('monitorCommon.resetCards.expiringSoonNotice')
    expect(urgentNote.html()).toContain('text-amber-700')

    // 已过期优先于紧急档
    const expired = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          fetched_at: fetchedAtMinutesAgo(1),
          reset_cards: [
            { type: 'five_hour', expire_at: daysFromNow(2) },
            { type: 'week', expire_at: daysFromNow(-1) },
          ],
        }),
      },
    })
    expect(expired.get('[data-testid="zhipu-reset-cards-expired"]').exists()).toBe(true)
    expect(expired.find('[data-testid="zhipu-reset-cards-expiring-soon"]').exists()).toBe(false)

    // 仅「注意档」（>3 天）时不得升级为紧急提醒
    const soon = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          fetched_at: fetchedAtMinutesAgo(1),
          reset_cards: [{ type: 'five_hour', expire_at: daysFromNow(6) }],
        }),
      },
    })
    expect(soon.get('[data-testid="zhipu-reset-cards-expiring"]').exists()).toBe(true)
    expect(soon.find('[data-testid="zhipu-reset-cards-expiring-soon"]').exists()).toBe(false)
  })

  it('always states the observe-only R0 copy inside the panel', () => {
    const cases: Array<[string, Partial<MonitorQuotaSnapshot>]> = [
      ['ok', { reset_cards: [{ type: 'five_hour', expire_at: daysFromNow(10) }] }],
      ['expiring', { reset_cards: [{ type: 'five_hour', expire_at: daysFromNow(6) }] }],
      ['expiring_soon', { reset_cards: [{ type: 'five_hour', expire_at: daysFromNow(1) }] }],
      ['expired', { reset_cards: [{ type: 'five_hour', expire_at: daysFromNow(-1) }] }],
      ['unknown', { reset_cards: [{ type: 'five_hour', expire_at: 'not-a-date' }] }],
      ['week-only', { reset_cards: [{ type: 'week', expire_at: daysFromNow(20) }] }],
      ['panel-empty', { reset_cards: [] }],
      ['needs-relogin', { needs_relogin: true }],
      ['failed-with-old-values', { success: false, reset_cards: [{ type: 'five_hour', expire_at: daysFromNow(10) }] }],
    ]

    for (const [label, overrides] of cases) {
      const wrapper = mount(MonitorQuotaView, {
        props: { snapshot: makeSnapshot({ fetched_at: fetchedAtMinutesAgo(1), ...overrides }) },
      })
      const panel = wrapper.get('[data-testid="zhipu-reset-cards-panel"]')
      const statement = panel.get('[data-testid="zhipu-reset-cards-observe-only"]')
      expect(`${label}: ${statement.text()}`).toBe(
        `${label}: monitorCommon.resetCards.observeOnly`
      )
      // 说明性文本，不是操作入口
      const kind = `${label}: ${statement.element.tagName}/${statement.attributes('role') ?? '-'}/${statement.attributes('tabindex') ?? '-'}`
      expect(kind).toBe(`${label}: P/-/-`)
    }
  })

  it('renders needs_relogin as read-only text with no affordance at all (R0)', () => {
    const wrapper = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          fetched_at: fetchedAtMinutesAgo(1),
          needs_relogin: true,
          reset_cards: [{ type: 'five_hour', expire_at: daysFromNow(1) }],
        }),
      },
    })

    const panel = wrapper.get('[data-testid="zhipu-reset-cards-panel"]')
    expect(panel.get('[data-testid="zhipu-reset-cards-relogin"]').text()).toBe(
      'monitorCommon.resetCards.needsRelogin'
    )
    expect(panel.get('[data-testid="zhipu-reset-cards-relogin"]').html()).toContain('text-red-600')

    // R0 负向断言：重置卡区块内不存在任何可交互元素或操作暗示
    expect(panel.findAll('button, a, input, select, textarea, [role="button"], [tabindex]')).toHaveLength(0)
    expect(panel.html()).not.toContain('<button')
    expect(panel.html()).not.toContain('<a ')
    expect(panel.html()).not.toContain('cursor-pointer')
    expect(panel.html()).not.toContain('reset/use')
  })

  it('offers no use/consume affordance in any reset card state (R0)', () => {
    const states: Array<[string, Partial<MonitorQuotaSnapshot>]> = [
      ['ok', { reset_cards: [{ type: 'five_hour', expire_at: daysFromNow(10) }] }],
      ['expiring', { reset_cards: [{ type: 'five_hour', expire_at: daysFromNow(6) }] }],
      ['expiring_soon', { reset_cards: [{ type: 'five_hour', expire_at: daysFromNow(1) }] }],
      ['expired', { reset_cards: [{ type: 'five_hour', expire_at: daysFromNow(-1) }] }],
      ['unknown', { reset_cards: [{ type: 'five_hour', expire_at: 'not-a-date' }] }],
      ['panel-empty', { reset_cards: [] }],
      ['needs-relogin', { needs_relogin: true }],
      ['failed', { success: false, error: 'boom', reset_cards: [{ type: 'five_hour', expire_at: daysFromNow(10) }] }],
    ]
    const interactive = [
      'button',
      'a',
      'input',
      'select',
      'textarea',
      'label',
      '[role="button"]',
      '[role="link"]',
      '[tabindex]',
      '[contenteditable="true"]',
    ].join(', ')
    const forbidden = [
      '<button',
      '<a ',
      'cursor-pointer',
      'reset/use',
      'use-reset',
      'resetcarduse',
      'onclick',
      '@click',
      '使用重置卡',
      '消耗重置卡',
    ]

    for (const [label, overrides] of states) {
      const wrapper = mount(MonitorQuotaView, {
        props: { snapshot: makeSnapshot({ fetched_at: fetchedAtMinutesAgo(1), ...overrides }) },
      })
      const panel = wrapper.get('[data-testid="zhipu-reset-cards-panel"]')
      const html = panel.html().toLowerCase()

      expect(`${label}: ${panel.findAll(interactive).length}`).toBe(`${label}: 0`)
      for (const needle of forbidden) {
        expect(`${label}: ${html.includes(needle)}`).toBe(`${label}: false`)
      }
      // 使用/消耗/领取类 DOM 名称 0 命中
      expect(
        `${label}: ${panel.findAll('[data-testid*="use"], [data-testid*="consume"], [data-testid*="redeem"], [data-testid*="apply"]').length}`
      ).toBe(`${label}: 0`)
      // 组件被嵌进运行结果弹窗/账号列表单元格时同样不得出现入口
      expect(`${label}: ${wrapper.findAll('button, a, [role="button"]').length}`).toBe(`${label}: 0`)
    }
  })

  it('shows the relogin notice alone when reset_cards is absent (no fabricated cards)', () => {
    const wrapper = mount(MonitorQuotaView, {
      props: { snapshot: makeSnapshot({ fetched_at: fetchedAtMinutesAgo(1), needs_relogin: true }) },
    })

    const panel = wrapper.get('[data-testid="zhipu-reset-cards-panel"]')
    expect(panel.get('[data-testid="zhipu-reset-cards-relogin"]').exists()).toBe(true)
    expect(panel.find('[data-testid="zhipu-reset-card-five_hour"]').exists()).toBe(false)
    expect(panel.find('[data-testid="zhipu-reset-card-week"]').exists()).toBe(false)
    expect(panel.find('[data-testid="zhipu-reset-cards-empty"]').exists()).toBe(false)
    expect(wrapper.html()).not.toContain('"count":0')
  })

  it('flags stale snapshots while keeping the last values visible', () => {
    const wrapper = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          fetched_at: fetchedAtMinutesAgo(30),
          model_credits: [
            { model: 'glm-4.6', date: '2026-10-05', input_tokens: 1200, cached_tokens: 0, output_tokens: 1, credits: 1 },
          ],
          reset_cards: [{ type: 'five_hour', expire_at: daysFromNow(30) }],
        }),
      },
    })

    const creditsStale = wrapper.get('[data-testid="zhipu-model-credits-stale"]')
    expect(creditsStale.text()).toBe('monitorCommon.credits.stale')
    expect(creditsStale.html()).toContain('text-amber-600')
    const resetStale = wrapper.get('[data-testid="zhipu-reset-cards-stale"]')
    expect(resetStale.text()).toBe('monitorCommon.resetCards.stale')
    expect(resetStale.html()).toContain('text-amber-600')

    // 陈旧不清空面板：旧值仍在
    expect(wrapper.get('[data-testid="zhipu-model-credits-input_tokens"]').text()).toBe('1.2K')
    expect(wrapper.get('[data-testid="zhipu-reset-card-five_hour"]').attributes('data-state')).toBe('ok')

    const fresh = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          fetched_at: fetchedAtMinutesAgo(1),
          model_credits: [
            { model: 'glm-4.6', date: '2026-10-05', input_tokens: 1200, cached_tokens: 0, output_tokens: 1, credits: 1 },
          ],
          reset_cards: [{ type: 'five_hour', expire_at: daysFromNow(30) }],
        }),
      },
    })
    expect(fresh.find('[data-testid="zhipu-model-credits-stale"]').exists()).toBe(false)
    expect(fresh.find('[data-testid="zhipu-reset-cards-stale"]').exists()).toBe(false)
  })

  it('keeps last values with a failure note when the fetch failed but data exists', () => {
    const wrapper = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          success: false,
          error: 'upstream 500',
          fetched_at: fetchedAtMinutesAgo(1),
          model_credits: [
            { model: 'glm-4.6', date: '2026-10-05', input_tokens: 1200, cached_tokens: 0, output_tokens: 1, credits: 1 },
          ],
          reset_cards: [{ type: 'five_hour', expire_at: daysFromNow(30) }],
        }),
      },
    })

    expect(wrapper.get('[data-testid="zhipu-model-credits-failed"]').text()).toBe(
      'monitorCommon.credits.failed'
    )
    expect(wrapper.get('[data-testid="zhipu-reset-cards-failed"]').text()).toBe(
      'monitorCommon.resetCards.failed'
    )
    // 有旧数据时不退化成空态；既有错误行仍保留
    expect(wrapper.find('[data-testid="zhipu-model-credits-empty"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="monitor-quota-error"]').exists()).toBe(true)

    // 失败且无新字段（老快照）：不新增面板，维持既有红字错误行
    const noData = mount(MonitorQuotaView, {
      props: { snapshot: makeSnapshot({ success: false, error: 'boom' }) },
    })
    expect(noData.get('[data-testid="monitor-quota-error"]').text()).toBe('boom')
    expect(noData.find('[data-testid="zhipu-model-credits-panel"]').exists()).toBe(false)
    expect(noData.find('[data-testid="zhipu-reset-cards-panel"]').exists()).toBe(false)
  })
})

// 两块新面板的全部文案必须 zh/en 齐备且键集合一致（票 16 验收项）。
function flattenKeys(node: unknown, prefix = ''): string[] {
  if (typeof node !== 'object' || node === null || Array.isArray(node)) {
    return prefix ? [prefix] : []
  }
  return Object.entries(node as Record<string, unknown>).flatMap(([key, value]) =>
    flattenKeys(value, prefix ? `${prefix}.${key}` : key)
  )
}

describe('MonitorQuotaView zhipu panel i18n keys', () => {
  const requiredKeys = [
    'monitorCommon.credits.title',
    'monitorCommon.credits.range',
    'monitorCommon.credits.updatedAt',
    'monitorCommon.credits.columns.date',
    'monitorCommon.credits.columns.model',
    'monitorCommon.credits.columns.inputTokens',
    'monitorCommon.credits.columns.cachedTokens',
    'monitorCommon.credits.columns.outputTokens',
    'monitorCommon.credits.columns.credits',
    'monitorCommon.credits.empty',
    'monitorCommon.credits.unavailable',
    'monitorCommon.credits.stale',
    'monitorCommon.credits.failed',
    'monitorCommon.resetCards.title',
    'monitorCommon.resetCards.readOnly',
    'monitorCommon.resetCards.types.five_hour',
    'monitorCommon.resetCards.types.week',
    'monitorCommon.resetCards.count',
    'monitorCommon.resetCards.expiresAt',
    'monitorCommon.resetCards.expiresIn',
    'monitorCommon.resetCards.expiring',
    'monitorCommon.resetCards.expiringSoonNotice',
    'monitorCommon.resetCards.expired',
    'monitorCommon.resetCards.unknownExpiry',
    'monitorCommon.resetCards.empty',
    'monitorCommon.resetCards.typeEmpty',
    'monitorCommon.resetCards.observeOnly',
    'monitorCommon.resetCards.stale',
    'monitorCommon.resetCards.needsRelogin',
    'monitorCommon.resetCards.failed',
  ]

  it.each([
    ['zh', zhDashboard],
    ['en', enDashboard],
  ] as const)('%s dashboard locale contains every panel key', (_locale, messages) => {
    const keys = new Set(flattenKeys(messages))
    expect(requiredKeys.filter((key) => !keys.has(key))).toEqual([])
  })

  it('keeps the zh/en key sets identical for both new subtrees', () => {
    const creditsZh = flattenKeys(zhDashboard.monitorCommon.credits).sort()
    const creditsEn = flattenKeys(enDashboard.monitorCommon.credits).sort()
    expect(creditsZh).toEqual(creditsEn)
    const resetZh = flattenKeys(zhDashboard.monitorCommon.resetCards).sort()
    const resetEn = flattenKeys(enDashboard.monitorCommon.resetCards).sort()
    expect(resetZh).toEqual(resetEn)
  })

  // 票 17：R0 文案与 3 天档提醒必须在两种语言下都表达「仅观测/不消耗」与「到期未使用将失效」。
  it('states the observe-only and void-on-expiry copy in both locales', () => {
    const zhReset = zhDashboard.monitorCommon.resetCards
    const enReset = enDashboard.monitorCommon.resetCards

    expect(zhReset.observeOnly).toContain('仅观测重置卡')
    expect(zhReset.observeOnly).toContain('不会使用或消耗')
    expect(zhReset.expiringSoonNotice).toBe('到期未使用将失效')

    expect(enReset.observeOnly.toLowerCase()).toContain('never uses or consumes')
    expect(enReset.expiringSoonNotice.toLowerCase()).toContain('unused cards are void')

    // 两语言不得同时给出「可使用」的正向措辞
    expect(`${zhReset.observeOnly} ${zhReset.expiringSoonNotice}`).not.toContain('可使用')
    expect(`${enReset.observeOnly} ${enReset.expiringSoonNotice}`.toLowerCase()).not.toContain('you can use')
  })
})
