import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import OpenCodeQuotaFallbackEditor from '../OpenCodeQuotaFallbackEditor.vue'
import type { OpenCodeQuotaFallbackConfig } from '../openCodeQuotaFallback'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, string>) => params?.model ? `${key}:${params.model}` : key
    })
  }
})

// Toggle 的真实契约是编辑器透传的 data-testid="opencode-quota-fallback-toggle"：
// 父组件属性会落在本 stub 的根 button 上并覆盖此处的同名标记，因此不在此声明标记，
// 一律按产品契约 testid 定位（与真实 Toggle 渲染结果一致）。
const ToggleStub = {
  props: ['modelValue'],
  emits: ['update:modelValue'],
  template: '<button type="button" @click="$emit(\'update:modelValue\', !modelValue)"><slot /></button>'
}

const SelectStub = {
  props: ['modelValue', 'options', 'disabled', 'error'],
  emits: ['update:modelValue'],
  template: `
    <div data-testid="select-stub">
      <button type="button" data-testid="select-custom" @click="$emit('update:modelValue', 'custom-free-model')">custom</button>
      <button type="button" data-testid="select-clear" @click="$emit('update:modelValue', null)">clear</button>
    </div>
  `
}

function fallback(overrides: Partial<OpenCodeQuotaFallbackConfig> = {}): OpenCodeQuotaFallbackConfig {
  return {
    enabled: false,
    threshold_percent: 98,
    model_mapping: {},
    ...overrides
  }
}

function mountEditor(config = fallback(), options = ['normal-a', 'normal-b']) {
  return mount(OpenCodeQuotaFallbackEditor, {
    props: {
      modelValue: config,
      modelOptions: options,
      candidates: [{ id: 'free-a', capability: 'available' }]
    },
    global: {
      stubs: {
        Toggle: ToggleStub,
        Select: SelectStub,
        Icon: true
      }
    }
  })
}

describe('OpenCodeQuotaFallbackEditor', () => {
  it('preserves its mappings and threshold when disabled', async () => {
    const wrapper = mountEditor(fallback({
      enabled: true,
      threshold_percent: 91,
      model_mapping: { 'normal-a': 'free-a' }
    }))

    await wrapper.get('[data-testid="opencode-quota-fallback-toggle"]').trigger('click')
    expect(wrapper.emitted('update:modelValue')?.[0]?.[0]).toEqual({
      enabled: false,
      threshold_percent: 91,
      model_mapping: { 'normal-a': 'free-a' }
    })
  })

  it('creates mappings with normal upstream IDs as keys', async () => {
    const wrapper = mountEditor(fallback({ enabled: true }))

    await wrapper.get('[data-testid="opencode-quota-fallback-add"]').trigger('click')
    expect(wrapper.emitted('update:modelValue')?.[0]?.[0]).toEqual({
      enabled: true,
      threshold_percent: 98,
      model_mapping: { 'normal-a': '' }
    })
  })

  it('never silently clamps an out-of-range threshold', async () => {
    const wrapper = mountEditor(fallback({ enabled: true, model_mapping: { 'normal-a': 'free-a' } }))
    const input = wrapper.get<HTMLInputElement>('[data-testid="opencode-quota-fallback-threshold"]')
    // 冻结契约（contract.md §启用约束）：threshold 必须是整数 1..100，越界/非整数由共享校验
    // 拒绝（后端 400），组件不得把管理员输入静默改写成边界值。
    const thresholds = () => (wrapper.emitted('update:modelValue') ?? [])
      .map(event => (event[0] as OpenCodeQuotaFallbackConfig).threshold_percent)

    await input.setValue('101.4')
    await input.trigger('change')
    expect(thresholds()).toContain(101.4)
    expect(thresholds()).not.toContain(100)

    await input.setValue('0')
    await input.trigger('change')
    expect(thresholds()).toContain(0)
    expect(thresholds()).not.toContain(1)

    await input.setValue('96')
    await input.trigger('change')
    expect(thresholds()).toContain(96)
  })

  it('surfaces the frozen threshold error to the administrator instead of coercing the value', () => {
    const outOfRange = mountEditor(fallback({
      enabled: true,
      threshold_percent: 101.4,
      model_mapping: { 'normal-a': 'free-a' }
    }))
    expect(outOfRange.get('[data-testid="opencode-quota-fallback-validation"]').text())
      .toContain('admin.accounts.openCodeQuotaFallback.errors.thresholdInvalid')

    const inRange = mountEditor(fallback({
      enabled: true,
      threshold_percent: 96,
      model_mapping: { 'normal-a': 'free-a' }
    }))
    expect(inRange.find('[data-testid="opencode-quota-fallback-validation"]').exists()).toBe(false)
  })

  it('accepts an administrator-entered precise ID and emits an upstream target refresh request', async () => {
    const wrapper = mountEditor(fallback({
      enabled: true,
      model_mapping: { 'normal-a': '' }
    }))

    await wrapper.get('[data-testid="select-custom"]').trigger('click')
    expect(wrapper.emitted('update:modelValue')?.[0]?.[0]).toMatchObject({
      model_mapping: { 'normal-a': 'custom-free-model' }
    })

    await wrapper.get('[data-testid="opencode-quota-fallback-sync-targets"]').trigger('click')
    expect(wrapper.emitted('sync-targets')).toHaveLength(1)
  })

  it('shows invalid configuration feedback for empty and identity mappings', () => {
    const empty = mountEditor(fallback({ enabled: true }))
    expect(empty.get('[data-testid="opencode-quota-fallback-validation"]').text())
      .toContain('admin.accounts.openCodeQuotaFallback.errors.mappingInvalid')

    const same = mountEditor(fallback({
      enabled: true,
      model_mapping: { 'normal-a': 'normal-a' }
    }))
    expect(same.get('[data-testid="opencode-quota-fallback-validation"]').text())
      .toContain('admin.accounts.openCodeQuotaFallback.errors.mappingInvalid')
  })

  it('renders a translated parent error without discarding editable configuration', () => {
    const wrapper = mount(OpenCodeQuotaFallbackEditor, {
      props: {
        modelValue: fallback({ enabled: true, model_mapping: { 'normal-a': 'free-a' } }),
        modelOptions: ['normal-a'],
        error: 'Request rejected by server'
      },
      global: { stubs: { Toggle: ToggleStub, Select: SelectStub, Icon: true } }
    })

    expect(wrapper.get('[data-testid="opencode-quota-fallback-validation"]').text())
      .toContain('Request rejected by server')
    expect(wrapper.text()).toContain('normal-a')
  })
})
