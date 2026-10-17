import { nextTick } from 'vue'
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
  name: 'SelectStub',
  props: ['modelValue', 'options', 'disabled', 'error'],
  emits: ['update:modelValue'],
  template: `
    <div
      data-select-stub="true"
      :data-model-value="modelValue"
      :data-option-values="(options || []).map(option => option.value).join(',')"
    >
      <button type="button" data-testid="select-custom" @click="$emit('update:modelValue', 'custom-free-model')">custom</button>
      <button type="button" data-testid="select-clear" @click="$emit('update:modelValue', null)">clear</button>
    </div>
  `
}

type EditorProps = {
  candidates?: Array<{ id: string; label?: string; capability: 'available' | 'unavailable' | 'unknown' }>
  candidatesState?: 'idle' | 'loading' | 'ready' | 'error'
}

function fallback(overrides: Partial<OpenCodeQuotaFallbackConfig> = {}): OpenCodeQuotaFallbackConfig {
  return {
    enabled: false,
    threshold_percent: 98,
    model_mapping: {},
    ...overrides
  }
}

function mountEditor(
  config = fallback(),
  options: string[] = ['normal-a', 'normal-b'],
  props: EditorProps = {}
) {
  return mount(OpenCodeQuotaFallbackEditor, {
    props: {
      modelValue: config,
      modelOptions: options,
      candidates: [{ id: 'free-a', capability: 'available' as const }],
      ...props
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

function lastEmitted(wrapper: ReturnType<typeof mountEditor>): OpenCodeQuotaFallbackConfig | undefined {
  const events = wrapper.emitted('update:modelValue') as Array<[OpenCodeQuotaFallbackConfig]> | undefined
  return events?.[events.length - 1]?.[0]
}

function sourceInput(wrapper: ReturnType<typeof mountEditor>, testId: string) {
  return wrapper.get<HTMLInputElement>(`[data-testid="opencode-quota-fallback-source-${testId}"]`)
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

  it('re-enables with the same source and target after a close reopen round trip', async () => {
    const wrapper = mountEditor(fallback({
      enabled: true,
      model_mapping: { 'normal-a': 'free-a' }
    }))

    await wrapper.get('[data-testid="opencode-quota-fallback-toggle"]').trigger('click')
    const disabled = lastEmitted(wrapper)!
    expect(disabled).toEqual({
      enabled: false,
      threshold_percent: 98,
      model_mapping: { 'normal-a': 'free-a' }
    })

    await wrapper.setProps({ modelValue: disabled })
    await wrapper.get('[data-testid="opencode-quota-fallback-toggle"]').trigger('click')
    expect(lastEmitted(wrapper)).toEqual({
      enabled: true,
      threshold_percent: 98,
      model_mapping: { 'normal-a': 'free-a' }
    })
  })

  it('adds a blank editable row without preselecting a source or target', async () => {
    const wrapper = mountEditor(fallback({ enabled: true }), [])

    await wrapper.get('[data-testid="opencode-quota-fallback-add"]').trigger('click')

    const input = wrapper.get<HTMLInputElement>('[data-testid^="opencode-quota-fallback-source-draft-"]')
    expect(input.element.value).toBe('')
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()

    const select = wrapper.get('[data-select-stub]')
    expect(select.attributes('data-model-value')).toBe('')
    expect(select.attributes('data-option-values')).toBe('free-a')
  })

  it('accepts a manually typed normal upstream model ID when the catalog failed', async () => {
    const wrapper = mountEditor(fallback({ enabled: true }), [], { candidatesState: 'error' })

    await wrapper.get('[data-testid="opencode-quota-fallback-add"]').trigger('click')
    const input = wrapper.get<HTMLInputElement>('[data-testid^="opencode-quota-fallback-source-draft-"]')
    await input.setValue('my-upstream-model')
    await input.trigger('blur')

    expect(lastEmitted(wrapper)).toEqual({
      enabled: true,
      threshold_percent: 98,
      model_mapping: { 'my-upstream-model': '' }
    })
    expect(wrapper.text()).toContain('admin.accounts.openCodeQuotaFallback.candidatesError')
  })

  it('commits the source on Enter and prevents implicit form submission', async () => {
    const wrapper = mountEditor(fallback({ enabled: true }), [])

    await wrapper.get('[data-testid="opencode-quota-fallback-add"]').trigger('click')
    const input = wrapper.get<HTMLInputElement>('[data-testid^="opencode-quota-fallback-source-draft-"]')
    await input.setValue('typed-model')

    const event = new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true })
    input.element.dispatchEvent(event)
    await nextTick()

    expect(event.defaultPrevented).toBe(true)
    expect(lastEmitted(wrapper)).toEqual({
      enabled: true,
      threshold_percent: 98,
      model_mapping: { 'typed-model': '' }
    })
  })

  it('writes a target chosen while the source is still a draft when the source is committed', async () => {
    const wrapper = mountEditor(fallback({ enabled: true }), [])

    await wrapper.get('[data-testid="opencode-quota-fallback-add"]').trigger('click')
    const input = wrapper.get<HTMLInputElement>('[data-testid^="opencode-quota-fallback-source-draft-"]')
    await input.setValue('late-source')
    await wrapper.get('[data-testid="select-custom"]').trigger('click')

    // source 尚未提交时 target 只存本地草稿，不写旧 key。
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()

    await input.trigger('blur')
    expect(lastEmitted(wrapper)).toEqual({
      enabled: true,
      threshold_percent: 98,
      model_mapping: { 'late-source': 'custom-free-model' }
    })
  })

  it('suggests only normal model mapping values as source IDs', () => {
    const wrapper = mountEditor(
      fallback({ enabled: true }),
      ['normal-a', 'normal-a', 'normal-b'],
      { candidates: [{ id: 'catalog-only', capability: 'available' }] }
    )

    const options = wrapper.get('[data-testid="opencode-quota-fallback-source-options"]').findAll('option')
    expect(options.map(option => option.attributes('value'))).toEqual(['normal-a', 'normal-b'])
    expect(
      wrapper.find('[data-testid="opencode-quota-fallback-source-options"] option[value="catalog-only"]').exists()
    ).toBe(false)
  })

  it('edits an existing source and preserves its target', async () => {
    const wrapper = mountEditor(fallback({
      enabled: true,
      model_mapping: { 'normal-a': 'free-a' }
    }))

    const input = sourceInput(wrapper, 'normal-a')
    await input.setValue('normal-b')
    await input.trigger('blur')

    expect(lastEmitted(wrapper)?.model_mapping).toEqual({ 'normal-b': 'free-a' })
    expect(wrapper.find('[data-testid="opencode-quota-fallback-source-normal-b"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="opencode-quota-fallback-source-normal-a"]').exists()).toBe(false)
  })

  it('rejects a duplicate source without overwriting either row', async () => {
    const wrapper = mountEditor(fallback({
      enabled: true,
      model_mapping: { 'normal-a': 'free-a', 'normal-b': 'free-b' }
    }))

    const input = sourceInput(wrapper, 'normal-b')
    await input.setValue('normal-a')
    await input.trigger('blur')

    expect(input.element.value).toBe('normal-b')
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(wrapper.find('[data-testid="opencode-quota-fallback-source-normal-a"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="opencode-quota-fallback-source-normal-b"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('admin.accounts.openCodeQuotaFallback.errors.duplicateSource')
  })

  it('reverts a cleared existing source and reports the required error', async () => {
    const wrapper = mountEditor(fallback({
      enabled: true,
      model_mapping: { 'normal-a': 'free-a' }
    }))

    const input = sourceInput(wrapper, 'normal-a')
    await input.setValue('   ')
    await input.trigger('blur')

    expect(input.element.value).toBe('normal-a')
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(wrapper.text()).toContain('admin.accounts.openCodeQuotaFallback.errors.sourceRequired')
  })

  it('keeps an in-progress draft source when the parent echoes the same config', async () => {
    const wrapper = mountEditor(fallback({ enabled: true }))

    await wrapper.get('[data-testid="opencode-quota-fallback-add"]').trigger('click')
    const input = wrapper.get<HTMLInputElement>('[data-testid^="opencode-quota-fallback-source-draft-"]')
    await input.setValue('draft-source')

    await wrapper.setProps({ modelValue: { enabled: true, threshold_percent: 98, model_mapping: {} } })

    expect(input.element.value).toBe('draft-source')
    expect(wrapper.find('[data-testid^="opencode-quota-fallback-source-draft-"]').exists()).toBe(true)
  })

  it('treats a parent reset to the pre-edit stored config as authoritative (no edit replay)', async () => {
    const original = fallback({
      enabled: true,
      threshold_percent: 95,
      model_mapping: { 'normal-a': 'free-a' }
    })
    const wrapper = mountEditor(original)

    const thresholdInput = wrapper.get<HTMLInputElement>('[data-testid="opencode-quota-fallback-threshold"]')
    await thresholdInput.setValue('90')
    await thresholdInput.trigger('change')
    const source = sourceInput(wrapper, 'normal-a')
    await source.setValue('normal-b')
    await source.trigger('blur')
    expect(lastEmitted(wrapper)).toEqual({
      enabled: true,
      threshold_percent: 90,
      model_mapping: { 'normal-b': 'free-a' }
    })
    const emittedAfterEdit = wrapper.emitted('update:modelValue')?.length ?? 0

    // 父级真实回退（例如服务端状态变化/新上下文）以父级 v-model 为准，不得复活旧编辑。
    await wrapper.setProps({ modelValue: original })

    expect(wrapper.get<HTMLInputElement>('[data-testid="opencode-quota-fallback-threshold"]').element.value).toBe('95')
    expect(sourceInput(wrapper, 'normal-a').element.value).toBe('normal-a')
    expect(wrapper.find('[data-testid="opencode-quota-fallback-source-normal-b"]').exists()).toBe(false)
    // 旧启发式会在这里重新 emit 本地编辑；新归属不得回写父级。
    expect(wrapper.emitted('update:modelValue')?.length ?? 0).toBe(emittedAfterEdit)
  })

  it('keeps an uncommitted draft row across a same-context external refresh', async () => {
    const wrapper = mountEditor(fallback({
      enabled: true,
      model_mapping: { 'normal-a': 'free-a' }
    }))

    await wrapper.get('[data-testid="opencode-quota-fallback-add"]').trigger('click')
    const draft = wrapper.get<HTMLInputElement>('[data-testid^="opencode-quota-fallback-source-draft-"]')
    await draft.setValue('draft-source')

    await wrapper.setProps({
      modelValue: {
        enabled: true,
        threshold_percent: 97,
        model_mapping: { 'normal-a': 'free-a', 'normal-b': 'free-b' }
      }
    })

    // 同一挂载上下文：父级 committed 值生效，未提交草稿保留。
    expect(draft.element.value).toBe('draft-source')
    expect(wrapper.get<HTMLInputElement>('[data-testid="opencode-quota-fallback-threshold"]').element.value).toBe('97')
    expect(wrapper.find('[data-testid="opencode-quota-fallback-source-normal-b"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="opencode-quota-fallback-target-normal-b"]').attributes('data-model-value'))
      .toBe('free-b')
  })

  it('blocks submit for a target-only draft row and surfaces sourceRequired', async () => {
    const wrapper = mountEditor(fallback({
      enabled: true,
      model_mapping: { 'normal-a': 'free-a' }
    }))

    await wrapper.get('[data-testid="opencode-quota-fallback-add"]').trigger('click')
    const draftTarget = wrapper.get('[data-testid^="opencode-quota-fallback-target-draft-"]')
    await draftTarget.get('[data-testid="select-custom"]').trigger('click')
    expect(draftTarget.attributes('data-model-value')).toBe('custom-free-model')

    const validateForSubmit = (wrapper.vm as unknown as {
      validateForSubmit: () => string | null
    }).validateForSubmit
    expect(validateForSubmit()).toBe('sourceRequired')
    expect(wrapper.text()).toContain('admin.accounts.openCodeQuotaFallback.errors.sourceRequired')
    // 合法行仍在，父级保存必须整体阻断而不是静默丢掉草稿行。
    expect(wrapper.find('[data-testid="opencode-quota-fallback-source-normal-a"]').exists()).toBe(true)
  })

  it('explicitly discards uncommitted draft rows when protection is turned off', async () => {
    const wrapper = mountEditor(fallback({
      enabled: true,
      model_mapping: { 'normal-a': 'free-a' }
    }))

    await wrapper.get('[data-testid="opencode-quota-fallback-add"]').trigger('click')
    await wrapper.get('[data-testid^="opencode-quota-fallback-target-draft-"] [data-testid="select-custom"]')
      .trigger('click')

    // 关闭操作必须始终可完成：显式回退草稿，而不是留下不可见的保存阻断。
    await wrapper.get('[data-testid="opencode-quota-fallback-toggle"]').trigger('click')
    const disabled = lastEmitted(wrapper)!
    expect(disabled).toEqual({
      enabled: false,
      threshold_percent: 98,
      model_mapping: { 'normal-a': 'free-a' }
    })

    await wrapper.setProps({ modelValue: disabled })
    await wrapper.get('[data-testid="opencode-quota-fallback-toggle"]').trigger('click')
    expect(wrapper.find('[data-testid^="opencode-quota-fallback-source-draft-"]').exists()).toBe(false)
    expect(wrapper.findAll('input[data-testid^="opencode-quota-fallback-source-"]')).toHaveLength(1)
  })

  it('keeps stored sources that are absent from the latest normal model options', () => {
    const wrapper = mountEditor(
      fallback({ enabled: true, model_mapping: { 'legacy-source': 'free-a' } }),
      ['normal-a']
    )

    expect(sourceInput(wrapper, 'legacy-source').element.value).toBe('legacy-source')
  })

  it('keeps a saved target that is no longer in the catalog', () => {
    const wrapper = mountEditor(fallback({
      enabled: true,
      model_mapping: { 'normal-a': 'removed-target' }
    }))

    const select = wrapper.get('[data-select-stub]')
    expect(select.attributes('data-model-value')).toBe('removed-target')
    expect(select.attributes('data-option-values')?.split(',')).toContain('removed-target')
  })

  it('keeps valid and broken rows visible together instead of silently dropping the broken one', () => {
    const wrapper = mountEditor(fallback({
      enabled: true,
      model_mapping: { good: 'free-a', bad: '' }
    }))

    expect(wrapper.findAll('input[data-testid^="opencode-quota-fallback-source-"]')).toHaveLength(2)
    expect(wrapper.get('[data-testid="opencode-quota-fallback-validation"]').text())
      .toContain('admin.accounts.openCodeQuotaFallback.errors.mappingInvalid')
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

  it('accepts an administrator-entered precise target and emits an upstream target refresh request', async () => {
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

  it('shows invalid configuration feedback for empty, identity, chained and cyclic mappings', () => {
    const empty = mountEditor(fallback({ enabled: true }))
    expect(empty.get('[data-testid="opencode-quota-fallback-validation"]').text())
      .toContain('admin.accounts.openCodeQuotaFallback.errors.mappingInvalid')

    const same = mountEditor(fallback({
      enabled: true,
      model_mapping: { 'normal-a': 'normal-a' }
    }))
    expect(same.get('[data-testid="opencode-quota-fallback-validation"]').text())
      .toContain('admin.accounts.openCodeQuotaFallback.errors.mappingInvalid')

    const chain = mountEditor(fallback({
      enabled: true,
      model_mapping: { a: 'b', b: 'c' }
    }))
    expect(chain.get('[data-testid="opencode-quota-fallback-validation"]').text())
      .toContain('admin.accounts.openCodeQuotaFallback.errors.mappingInvalid')

    const cycle = mountEditor(fallback({
      enabled: true,
      model_mapping: { a: 'b', b: 'a' }
    }))
    expect(cycle.get('[data-testid="opencode-quota-fallback-validation"]').text())
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
    expect(sourceInput(wrapper, 'normal-a').element.value).toBe('normal-a')
  })
})
