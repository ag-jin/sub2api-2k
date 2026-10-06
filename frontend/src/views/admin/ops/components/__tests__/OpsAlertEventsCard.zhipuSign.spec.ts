import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'

import type { AlertEvent } from '@/api/admin/ops'
import OpsAlertEventsCard from '../OpsAlertEventsCard.vue'

const { listAlertEvents, getAlertEvent, updateAlertEventStatus, createAlertSilence } = vi.hoisted(
  () => ({
    listAlertEvents: vi.fn(),
    getAlertEvent: vi.fn(),
    updateAlertEventStatus: vi.fn(),
    createAlertSilence: vi.fn(),
  })
)

vi.mock('@/api/admin/ops', () => ({
  opsAPI: { listAlertEvents, getAlertEvent, updateAlertEventStatus, createAlertSilence },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() }),
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key}|${JSON.stringify(params)}` : key,
      te: () => true,
    }),
  }
})

const SelectStub = defineComponent({
  name: 'SelectStub',
  props: { modelValue: { type: [String, Number], default: '' } },
  emits: ['change', 'update:modelValue'],
  template: '<div class="select-stub" />',
})

/** 票 25 后端真实事件形状：L1 带账号维度与窗口桶，L2 只有 platform 维度。 */
const L1_EVENT: AlertEvent = {
  id: 11,
  rule_id: 42,
  severity: 'P1',
  status: 'firing',
  title: '智谱签名失效（L1 实时窗口）',
  description:
    'zhipu_sign_fail_window > 0.00 (current 2.00) over last 5m (platform=zhipu)；建议动作：检查 X-Client-Version 是否为 0.16.9',
  metric_value: 2,
  threshold_value: 0,
  dimensions: {
    platform: 'zhipu',
    zhipu_sign_accounts: [1024],
    zhipu_sign_window_bucket: 5_879_040,
  },
  fired_at: '2026-10-06T01:00:00Z',
  resolved_at: null,
  email_sent: true,
  created_at: '2026-10-06T01:00:00Z',
}

const L2_EVENT: AlertEvent = {
  ...L1_EVENT,
  id: 12,
  rule_id: 43,
  title: '智谱签名有效系数偏离（L2 费率对账）',
  description: 'zhipu_sign_effective_rate > 0.70 (current 0.71) over last 360m (platform=zhipu)；建议动作：检查 X-Client-Version',
  metric_value: 0.71,
  threshold_value: 0.7,
  dimensions: { platform: 'zhipu' },
  status: 'resolved',
  resolved_at: '2026-10-06T06:00:00Z',
}

const OTHER_EVENT: AlertEvent = {
  ...L1_EVENT,
  id: 13,
  title: 'OpenAI 错误率偏高',
  description: 'error_rate > 5.00 (current 9.00) over last 5m (platform=openai)',
  dimensions: { platform: 'openai', group_id: 3 },
}

function mountCard(events: AlertEvent[]) {
  listAlertEvents.mockResolvedValue(events)
  return mount(OpsAlertEventsCard, {
    global: { stubs: { Select: SelectStub } },
  })
}

/** 筛选下拉顺序：timeRange / platform / severity / status / emailSent。 */
const PLATFORM_FILTER_INDEX = 1

describe('OpsAlertEventsCard zhipu sign alerts (ticket 30)', () => {
  beforeEach(() => {
    listAlertEvents.mockReset()
  })

  it('queries without a platform filter by default and lists the events', async () => {
    const wrapper = mountCard([L1_EVENT, OTHER_EVENT])
    await flushPromises()

    expect(listAlertEvents).toHaveBeenCalledTimes(1)
    expect(listAlertEvents.mock.calls[0][0]).toMatchObject({ limit: 10, time_range: '24h' })
    expect(listAlertEvents.mock.calls[0][0]).not.toHaveProperty('platform')

    // 非签名事件不出现补充明细（保持既有行渲染）
    const details = wrapper.findAll('[data-testid="zhipu-sign-alert-detail"]')
    expect(details).toHaveLength(1)
    expect(details[0].attributes('data-kind')).toBe('fail_window')
  })

  it('filters by the platform=zhipu dimension through the existing query', async () => {
    const wrapper = mountCard([L1_EVENT])
    await flushPromises()

    const filters = wrapper.findAllComponents({ name: 'SelectStub' })
    expect(filters.length).toBeGreaterThanOrEqual(5)
    filters[PLATFORM_FILTER_INDEX].vm.$emit('change', 'zhipu')
    await flushPromises()

    expect(listAlertEvents).toHaveBeenCalledTimes(2)
    expect(listAlertEvents.mock.calls[1][0]).toMatchObject({
      limit: 10,
      time_range: '24h',
      platform: 'zhipu',
    })

    // 清空筛选回到全量
    filters[PLATFORM_FILTER_INDEX].vm.$emit('change', '')
    await flushPromises()
    expect(listAlertEvents).toHaveBeenCalledTimes(3)
    expect(listAlertEvents.mock.calls[2][0]).not.toHaveProperty('platform')
  })

  it('renders window count, masked accounts and the suggested action on the desktop row', async () => {
    const wrapper = mountCard([L1_EVENT, L2_EVENT])
    await flushPromises()

    const rows = wrapper.findAll('[data-testid="zhipu-sign-alert-detail"]')
    expect(rows).toHaveLength(2)

    const l1 = rows[0]
    expect(l1.get('[data-testid="zhipu-sign-alert-kind"]').text()).toBe(
      'admin.ops.alertEvents.zhipuSign.kind.fail_window'
    )
    expect(l1.get('[data-testid="zhipu-sign-alert-metric"]').text()).toContain(
      '{"value":"2","threshold":"0"}'
    )
    const account = l1.get('[data-testid="zhipu-sign-alert-account"]')
    expect(account.text()).toBe('#10**')
    expect(account.attributes('title')).toBe('1024')
    expect(l1.get('[data-testid="zhipu-sign-alert-action"]').text()).toContain(
      '检查 X-Client-Version 是否为 0.16.9'
    )

    const l2 = rows[1]
    expect(l2.attributes('data-kind')).toBe('effective_rate')
    expect(l2.get('[data-testid="zhipu-sign-alert-metric"]').text()).toBe(
      'admin.ops.alertEvents.zhipuSign.effectiveRate|{"value":"0.71","threshold":"0.70"}'
    )
    // L2 事件没有账号维度：不虚构账号行
    expect(l2.find('[data-testid="zhipu-sign-alert-account"]').exists()).toBe(false)

    // 恢复状态沿用既有徽标渲染（已解决）
    expect(wrapper.text()).toContain('admin.ops.alertEvents.status.resolved')
    expect(wrapper.text()).not.toContain('NaN')
  })

  it('renders the same detail on the narrow-viewport card list', async () => {
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
      const wrapper = mountCard([L1_EVENT])
      await flushPromises()

      // 窄屏走卡片列表分支，不是表格
      expect(wrapper.find('table').exists()).toBe(false)
      const detail = wrapper.get('[data-testid="zhipu-sign-alert-detail"]')
      expect(detail.get('[data-testid="zhipu-sign-alert-account"]').text()).toBe('#10**')
      expect(detail.get('[data-testid="zhipu-sign-alert-title"]').text()).toBe(
        'admin.ops.alertEvents.zhipuSign.l1Title'
      )
      expect(wrapper.text()).not.toContain('NaN')
    } finally {
      Object.defineProperty(window, 'matchMedia', { writable: true, value: previousMatchMedia })
    }
  })
})
