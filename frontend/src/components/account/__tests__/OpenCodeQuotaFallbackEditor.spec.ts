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

const ToggleStub = {
  props: ['modelValue'],
  emits: ['update:modelValue'],
  template: '<button type="button" data-testid="toggle-stub" @click="$emit(\'update:modelValue\', !modelValue)"><slot /></button>'
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

    await wrapper.get('[data-testid="toggle-stub"]').trigger('click')
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

  it('clamps the threshold to an integer in the 1 to 100 range', async () => {
    const wrapper = mountEditor(fallback({ enabled: true }))
    const input = wrapper.get<HTMLInputElement>('[data-testid="opencode-quota-fallback-threshold"]')

    await input.setValue('101.4')
    await input.trigger('change')
    expect(wrapper.emitted('update:modelValue')?.[0]?.[0]).toMatchObject({ threshold_percent: 100 })

    await input.setValue('0')
    await input.trigger('change')
    expect(wrapper.emitted('update:modelValue')?.[1]?.[0]).toMatchObject({ threshold_percent: 1 })
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
