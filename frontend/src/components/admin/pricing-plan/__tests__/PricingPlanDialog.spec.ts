import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import PricingPlanDialog from '../PricingPlanDialog.vue'

const { getById, createPlan, updatePlan, getAllGroups, showError } = vi.hoisted(() => ({
  getById: vi.fn(),
  createPlan: vi.fn(),
  updatePlan: vi.fn(),
  getAllGroups: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    pricingPlans: {
      getById,
      create: createPlan,
      update: updatePlan
    },
    groups: {
      getAll: getAllGroups
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

// Select 桩：值通过 options 反查，保证 number 类型的 group_id 不被字符串化
const SelectStub = {
  props: ['modelValue', 'options', 'placeholder', 'error'],
  emits: ['update:modelValue'],
  template: `
    <select :data-testid="'stub-select'" @change="onChange">
      <option v-for="opt in options" :key="String(opt.value)" :value="String(opt.value)" :selected="opt.value === modelValue">{{ opt.label }}</option>
    </select>`,
  methods: {
    onChange(event: Event) {
      const raw = (event.target as HTMLSelectElement).value
      const opt = this.options.find((o: { value: unknown }) => String(o.value) === raw)
      this.$emit('update:modelValue', opt ? opt.value : null)
    }
  }
}

const BaseDialogStub = {
  props: ['show', 'title', 'width'],
  emits: ['close'],
  template: '<div><slot /><slot name="footer" /></div>'
}

function detailPlan(overrides: Record<string, unknown> = {}) {
  return {
    id: 7,
    name: 'pro',
    title: 'Pro',
    description: 'desc',
    status: 'active',
    is_public: true,
    sort_order: 5,
    created_at: '2026-08-01T00:00:00Z',
    updated_at: '2026-08-01T00:00:00Z',
    models: [
      {
        id: 11,
        plan_id: 7,
        public_model: 'claude-sonnet-4',
        protocol: 'messages',
        upstream_model: 'claude-sonnet-4-latest',
        direct: true,
        allow_compatibility_fallback: false,
        priority: 100,
        enabled: true,
        notes: 'n',
        pricing: {
          billing_mode: 'token',
          input_price: 1.5,
          output_price: 7.5,
          cache_write_price: null,
          cache_read_price: null,
          fast_multiplier: null,
          flex_multiplier: null,
          image_input_price: null,
          image_output_price: null,
          per_request_price: null,
          intervals: [
            {
              min_tokens: 0,
              max_tokens: 1000,
              tier_label: 't1',
              input_price: 1.2,
              output_price: 6,
              per_request_price: null,
              sort_order: 0
            }
          ],
          time_pricing: { timezone: 'UTC', periods: [{ start_time: '09:00', end_time: '12:00', multiplier: 1.5 }] }
        },
        created_at: '2026-08-01T00:00:00Z',
        updated_at: '2026-08-01T00:00:00Z'
      }
    ],
    routes: [{ id: 9, plan_id: 7, group_id: 3, priority: 0, enabled: true, created_at: '', updated_at: '' }],
    ...overrides
  }
}

function mountDialog(props: { show: boolean; planId: number | null }) {
  return mount(PricingPlanDialog, {
    props,
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        Select: SelectStub,
        Icon: true,
        LoadingSpinner: true
      }
    }
  })
}

describe('PricingPlanDialog', () => {
  beforeEach(() => {
    getById.mockReset()
    createPlan.mockReset()
    updatePlan.mockReset()
    showError.mockReset()
    getAllGroups.mockReset()
    getAllGroups.mockResolvedValue([
      { id: 3, name: 'primary', platform: 'anthropic' },
      { id: 4, name: 'backup', platform: 'openai' }
    ])
  })

  it('does not fetch detail in create mode', async () => {
    mountDialog({ show: true, planId: null })
    await flushPromises()
    expect(getById).not.toHaveBeenCalled()
  })

  it('applies explicit defaults (direct/enabled true, priority 100) to new model entries', async () => {
    const wrapper = mountDialog({ show: true, planId: null })
    await flushPromises()

    await wrapper.find('[data-testid="add-model"]').trigger('click')
    const entry = wrapper.find('[data-testid="model-entry-0"]')

    // 展示层断言：直接读表单状态
    const form = (wrapper.vm as unknown as { form: { models: unknown[] } }).form
    const row = form.models[0] as Record<string, unknown>
    expect(row.direct).toBe(true)
    expect(row.enabled).toBe(true)
    expect(row.priority).toBe(100)
    expect(row.protocol).toBe('chat_completions')
    expect(row.billing_mode).toBe('token')
    expect(entry.exists()).toBe(true)
  })

  it('fetches full detail by id on edit and preserves pricing intervals/time pricing on update', async () => {
    getById.mockResolvedValue(detailPlan())
    updatePlan.mockResolvedValue(detailPlan())

    const wrapper = mountDialog({ show: true, planId: 7 })
    await flushPromises()

    expect(getById).toHaveBeenCalledWith(7)

    const nameInput = wrapper.find('[data-testid="plan-name"]')
    expect((nameInput.element as HTMLInputElement).value).toBe('pro')

    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(updatePlan).toHaveBeenCalledTimes(1)
    const [id, payload] = updatePlan.mock.calls[0]
    expect(id).toBe(7)
    expect(payload.models[0].public_model).toBe('claude-sonnet-4')
    expect(payload.models[0].pricing.billing_mode).toBe('token')
    // 后端全量明细原样保留：区间与分时定价
    expect(payload.models[0].pricing.intervals).toHaveLength(1)
    expect(payload.models[0].pricing.intervals[0].tier_label).toBe('t1')
    expect(payload.models[0].pricing.time_pricing.timezone).toBe('UTC')
    expect(payload.models[0].pricing.time_pricing.periods[0].multiplier).toBe(1.5)
    expect(payload.routes).toHaveLength(1)
    expect(payload.routes[0].group_id).toBe(3)
  })

  it('drops time pricing when the billing mode is switched away from token', async () => {
    getById.mockResolvedValue(detailPlan())
    updatePlan.mockResolvedValue(detailPlan())

    const wrapper = mountDialog({ show: true, planId: 7 })
    await flushPromises()

    // 切到 image 模式后保存：time_pricing 不再进入 payload
    await wrapper.find('[data-testid="billing-mode-image"]').trigger('click')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    const payload = updatePlan.mock.calls[0][1]
    expect(payload.models[0].pricing.billing_mode).toBe('image')
    expect(payload.models[0].pricing.time_pricing).toBeUndefined()
    expect(payload.models[0].pricing.per_request_price).toBeNull()
    expect(payload.models[0].pricing.input_price).toBeUndefined()
  })

  it('blocks submit and shows inline errors on duplicate route group', async () => {
    const wrapper = mountDialog({ show: true, planId: null })
    await flushPromises()

    await wrapper.find('[data-testid="add-route"]').trigger('click')
    await wrapper.find('[data-testid="add-route"]').trigger('click')

    const selects = wrapper.findAll('[data-testid="stub-select"]')
    // 第一个 select 是 status，后面两个分别是两条路由的 group select
    await selects[1].setValue('3')
    await selects[2].setValue('3')

    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(createPlan).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="route-group-error-1"]').exists()).toBe(true)
  })

  it('blocks submit on duplicate route priority', async () => {
    const wrapper = mountDialog({ show: true, planId: null })
    await flushPromises()

    await wrapper.find('[data-testid="add-route"]').trigger('click')
    await wrapper.find('[data-testid="add-route"]').trigger('click')

    // 保持两条路由 priority 均为默认 0：重复优先级应被拦截
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(createPlan).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="route-priority-error-0"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="route-priority-error-1"]').exists()).toBe(true)
  })

  it('clears compatibility fallback when the model becomes direct or switches protocol', async () => {
    const wrapper = mountDialog({ show: true, planId: null })
    await flushPromises()

    await wrapper.find('[data-testid="add-model"]').trigger('click')
    const form = (wrapper.vm as unknown as { form: { models: Array<Record<string, unknown>> } }).form
    const row = form.models[0]
    row.direct = false
    row.allow_compatibility_fallback = true

    await wrapper.findAll('[data-testid="stub-select"]')[1].setValue('messages')
    expect(row.allow_compatibility_fallback).toBe(false)

    row.protocol = 'chat_completions'
    row.allow_compatibility_fallback = true
    await wrapper.findAllComponents({ name: 'Toggle' })[1].vm.$emit('update:modelValue', true)
    expect(row.direct).toBe(true)
    expect(row.allow_compatibility_fallback).toBe(false)
  })

  it('uses per-image pricing for image billing mode', async () => {
    const wrapper = mountDialog({ show: true, planId: null })
    await flushPromises()

    await wrapper.find('[data-testid="plan-name"]').setValue('image-plan')
    await wrapper.find('[data-testid="add-model"]').trigger('click')
    await wrapper.find('[data-testid="add-route"]').trigger('click')
    const inputs = wrapper.findAll('[data-testid="model-entry-0"] input[type="text"]')
    await inputs[0].setValue('image-model')
    await wrapper.findAll('[data-testid="stub-select"]')[2].setValue('3')
    await wrapper.find('[data-testid="billing-mode-image"]').trigger('click')
    await wrapper.find('[data-testid="price-per_request_price"]').setValue('0.04')
    createPlan.mockResolvedValue(detailPlan())

    await wrapper.find('form').trigger('submit')
    await flushPromises()

    const payload = createPlan.mock.calls[0][0]
    expect(payload.models[0].pricing).toMatchObject({
      billing_mode: 'image',
      per_request_price: 0.04
    })
    expect(payload.models[0].pricing.image_input_price).toBeUndefined()
  })

  it('creates a plan with model entries and routes in create mode', async () => {
    createPlan.mockResolvedValue({ data: detailPlan() })

    const wrapper = mountDialog({ show: true, planId: null })
    await flushPromises()

    await wrapper.find('[data-testid="plan-name"]').setValue('new-plan')
    await wrapper.find('[data-testid="add-model"]').trigger('click')
    await wrapper.find('[data-testid="add-route"]').trigger('click')

    // 填模型条目 + 选择分组（避免重复校验拦截）
    const inputs = wrapper.findAll('[data-testid="model-entry-0"] input[type="text"]')
    await inputs[0].setValue('gpt-4o')
    // select 顺序：status / protocol(模型条目内) / group(路由层)
    const selects = wrapper.findAll('[data-testid="stub-select"]')
    await selects[2].setValue('4')

    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(createPlan).toHaveBeenCalledTimes(1)
    const payload = createPlan.mock.calls[0][0]
    expect(payload.name).toBe('new-plan')
    expect(payload.models).toHaveLength(1)
    expect(payload.models[0].public_model).toBe('gpt-4o')
    expect(payload.models[0].priority).toBe(100)
    expect(payload.models[0].direct).toBe(true)
    expect(payload.models[0].enabled).toBe(true)
    expect(payload.routes).toHaveLength(1)
    expect(payload.routes[0].group_id).toBe(4)
  })
})