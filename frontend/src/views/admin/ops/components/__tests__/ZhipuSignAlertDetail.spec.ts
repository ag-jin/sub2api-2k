import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import type { AlertEvent } from '@/api/admin/ops'
import ZhipuSignAlertDetail from '../ZhipuSignAlertDetail.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key}|${JSON.stringify(params)}` : key,
      te: () => true,
    }),
  }
})

/** 事件 fixture 与票 25 后端真实形状一致（见 ops_alert_evaluator_service.go）。 */
function makeEvent(overrides: Partial<AlertEvent> = {}): AlertEvent {
  return {
    id: 1,
    rule_id: 42,
    severity: 'P1',
    status: 'firing',
    title: '智谱签名失效（L1 实时窗口）',
    description:
      'zhipu_sign_fail_window > 0.00 (current 2.00) over last 5m (platform=zhipu)；建议动作：检查 X-Client-Version 是否为 0.16.9',
    metric_value: 2,
    threshold_value: 0,
    dimensions: { platform: 'zhipu', zhipu_sign_accounts: [1024], zhipu_sign_window_bucket: 5_879_040 },
    fired_at: '2026-10-06T01:00:00Z',
    resolved_at: null,
    email_sent: true,
    created_at: '2026-10-06T01:00:00Z',
    ...overrides,
  }
}

const L2_EVENT = makeEvent({
  id: 2,
  title: '智谱签名有效系数偏离（L2 费率对账）',
  description: 'zhipu_sign_effective_rate > 0.70 (current 0.71) over last 360m (platform=zhipu)；建议动作：检查 X-Client-Version',
  metric_value: 0.71,
  threshold_value: 0.7,
  dimensions: { platform: 'zhipu' },
  status: 'resolved',
  resolved_at: '2026-10-06T06:00:00Z',
})

describe('ZhipuSignAlertDetail', () => {
  it('renders the L1 window-count detail with masked accounts and the suggested action', () => {
    const wrapper = mount(ZhipuSignAlertDetail, { props: { event: makeEvent() } })

    const detail = wrapper.get('[data-testid="zhipu-sign-alert-detail"]')
    expect(detail.attributes('data-kind')).toBe('fail_window')
    expect(detail.get('[data-testid="zhipu-sign-alert-kind"]').text()).toBe(
      'admin.ops.alertEvents.zhipuSign.kind.fail_window'
    )
    expect(detail.get('[data-testid="zhipu-sign-alert-title"]').text()).toBe(
      'admin.ops.alertEvents.zhipuSign.l1Title'
    )
    // 当前值 / 阈值以等宽数值渲染，负数与缺失都不会变成 NaN
    const metric = detail.get('[data-testid="zhipu-sign-alert-metric"]')
    expect(metric.text()).toContain('admin.ops.alertEvents.zhipuSign.windowCount')
    expect(metric.text()).toContain('{"value":"2","threshold":"0"}')
    expect(metric.classes()).toContain('font-mono')

    // 涉及账号：正文是脱敏标识，完整 id 只在 title
    const accounts = detail.findAll('[data-testid="zhipu-sign-alert-account"]')
    expect(accounts).toHaveLength(1)
    expect(accounts[0].text()).toBe('#10**')
    expect(accounts[0].attributes('title')).toBe('1024')
    expect(detail.get('[data-testid="zhipu-sign-alert-accounts"]').text()).not.toContain('1024')

    // 建议动作沿用后端文案
    const action = detail.get('[data-testid="zhipu-sign-alert-action"]')
    expect(action.text()).toContain('admin.ops.alertEvents.zhipuSign.suggestedAction')
    expect(action.text()).toContain('检查 X-Client-Version 是否为 0.16.9')

    expect(detail.text()).not.toContain('NaN')
    expect(detail.text()).not.toContain('undefined')
  })

  it('renders the L2 rate detail with current / target / threshold and no account row', () => {
    const wrapper = mount(ZhipuSignAlertDetail, { props: { event: L2_EVENT } })

    const detail = wrapper.get('[data-testid="zhipu-sign-alert-detail"]')
    expect(detail.attributes('data-kind')).toBe('effective_rate')
    expect(detail.get('[data-testid="zhipu-sign-alert-kind"]').text()).toBe(
      'admin.ops.alertEvents.zhipuSign.kind.effective_rate'
    )
    // 标题带阈值参数，默认 0.70 时与设计文案「智谱有效系数高于 0.70」一致
    expect(detail.get('[data-testid="zhipu-sign-alert-title"]').text()).toBe(
      'admin.ops.alertEvents.zhipuSign.l2Title|{"threshold":"0.70"}'
    )
    expect(detail.get('[data-testid="zhipu-sign-alert-metric"]').text()).toBe(
      'admin.ops.alertEvents.zhipuSign.effectiveRate|{"value":"0.71","threshold":"0.70"}'
    )
    // L2 事件没有账号维度：不虚构账号行
    expect(detail.find('[data-testid="zhipu-sign-alert-accounts"]').exists()).toBe(false)
    expect(detail.text()).not.toContain('NaN')
  })

  it('renders nothing for non-zhipu alerts or zhipu alerts outside the two signing metrics', () => {
    const otherPlatform = mount(ZhipuSignAlertDetail, {
      props: { event: makeEvent({ dimensions: { platform: 'openai' } }) },
    })
    expect(otherPlatform.find('[data-testid="zhipu-sign-alert-detail"]').exists()).toBe(false)
    expect(otherPlatform.text()).toBe('')

    const otherMetric = mount(ZhipuSignAlertDetail, {
      props: {
        event: makeEvent({
          dimensions: { platform: 'zhipu' },
          description: 'group_available_ratio < 0.50 (current 0.20) over last 5m (platform=zhipu)',
        }),
      },
    })
    expect(otherMetric.find('[data-testid="zhipu-sign-alert-detail"]').exists()).toBe(false)
  })

  it('keeps missing metric fields as placeholders and offers no action affordance', () => {
    const wrapper = mount(ZhipuSignAlertDetail, {
      props: {
        event: makeEvent({
          metric_value: undefined,
          threshold_value: undefined,
          dimensions: { platform: 'zhipu', zhipu_sign_window_bucket: 5_879_040 },
          description: 'zhipu_sign_fail_window > 0.00 over last 5m (platform=zhipu)',
        }),
      },
    })

    const detail = wrapper.get('[data-testid="zhipu-sign-alert-detail"]')
    expect(detail.get('[data-testid="zhipu-sign-alert-metric"]').text()).toContain(
      '{"value":"-","threshold":"-"}'
    )
    // 后端描述没有建议动作时不编造：不渲染动作行
    expect(detail.find('[data-testid="zhipu-sign-alert-action"]').exists()).toBe(false)
    expect(detail.text()).not.toContain('NaN')
    expect(detail.text()).not.toContain('undefined')

    // 明细块只读：静默/手动解决仍在既有详情弹窗里
    expect(detail.findAll('button, a, input, select, [role="button"], [tabindex]')).toHaveLength(0)
  })
})
