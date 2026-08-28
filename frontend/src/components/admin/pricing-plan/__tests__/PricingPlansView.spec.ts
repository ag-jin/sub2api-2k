import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import type { AdminPricingPlan } from '@/api/admin/pricingPlans'
import PricingPlansView from '../PricingPlansView.vue'

const { listPlans, deletePlan, showSuccess, showError } = vi.hoisted(() => ({
  listPlans: vi.fn(),
  deletePlan: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    pricingPlans: {
      list: listPlans,
      delete: deletePlan
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showSuccess, showError })
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

const AppLayoutStub = defineComponent({
  template: '<main><slot /></main>'
})

const TablePageLayoutStub = defineComponent({
  template: '<section><slot name="table" /></section>'
})

// 渲染 cell slot，按行输出，便于断言
const DataTableStub = defineComponent({
  props: {
    data: { type: Array, default: () => [] },
    columns: { type: Array, default: () => [] },
    loading: { type: Boolean, default: false }
  },
  template:
    '<div data-testid="datatable"><div v-for="row in data" :key="row.id" class="row"><slot name="cell-name" :row="row" :value="row.name" /><slot name="cell-status" :row="row" :value="row.status" /><slot name="cell-actions" :row="row" /></div><slot name="empty" /></div>'
})

const PricingPlanDialogStub = defineComponent({
  props: ['show', 'planId'],
  emits: ['close', 'saved'],
  template: '<div data-testid="pp-dialog-stub" />'
})

function makePlan(overrides: Partial<AdminPricingPlan> = {}): AdminPricingPlan {
  return {
    id: 42,
    name: 'pro',
    title: 'Pro',
    description: '',
    status: 'active',
    is_public: true,
    sort_order: 0,
    created_at: '2026-08-01T00:00:00Z',
    updated_at: '2026-08-01T00:00:00Z',
    models: [],
    routes: [],
    ...overrides
  }
}

function mountView() {
  return mount(PricingPlansView, {
    global: {
      stubs: {
        AppLayout: AppLayoutStub,
        TablePageLayout: TablePageLayoutStub,
        DataTable: DataTableStub,
        PricingPlanDialog: PricingPlanDialogStub,
        ConfirmDialog: true,
        EmptyState: true,
        StatusBadge: true,
        Icon: true
      }
    }
  })
}

describe('PricingPlansView', () => {
  beforeEach(() => {
    listPlans.mockReset()
    deletePlan.mockReset()
    showSuccess.mockReset()
    showError.mockReset()
  })

  it('loads plans on mount and renders rows', async () => {
    listPlans.mockResolvedValue([makePlan(), makePlan({ id: 43, name: 'basic', status: 'disabled' })])
    const wrapper = mountView()
    await flushPromises()

    expect(listPlans).toHaveBeenCalledWith(false)
    expect(wrapper.findAll('.row')).toHaveLength(2)
    expect(wrapper.find('[data-testid="plan-name-42"]').text()).toBe('pro')
  })

  it('reloads with include_disabled when the toggle is enabled', async () => {
    listPlans.mockResolvedValue([])
    const wrapper = mountView()
    await flushPromises()

    await wrapper.find('[data-testid="include-disabled"]').trigger('click')
    await flushPromises()

    expect(listPlans).toHaveBeenLastCalledWith(true)
  })

  it('opens the dialog in create mode on the create button', async () => {
    listPlans.mockResolvedValue([])
    const wrapper = mountView()
    await flushPromises()

    await wrapper.find('[data-testid="create-plan"]').trigger('click')

    const dialog = wrapper.findComponent(PricingPlanDialogStub)
    expect(dialog.props('show')).toBe(true)
    expect(dialog.props('planId')).toBeNull()
  })

  it('opens the dialog in edit mode with the row id, and fetches detail inside the dialog', async () => {
    listPlans.mockResolvedValue([makePlan()])
    const wrapper = mountView()
    await flushPromises()

    await wrapper.find('[data-testid="edit-plan"]').trigger('click')

    const dialog = wrapper.findComponent(PricingPlanDialogStub)
    expect(dialog.props('show')).toBe(true)
    expect(dialog.props('planId')).toBe(42)
  })

  it('deletes a plan after confirmation and refreshes the list', async () => {
    listPlans.mockResolvedValue([makePlan()])
    deletePlan.mockResolvedValue({ message: 'deleted' })
    const wrapper = mountView()
    await flushPromises()

    await wrapper.find('[data-testid="delete-plan"]').trigger('click')
    expect(deletePlan).not.toHaveBeenCalled()

    // 触发 ConfirmDialog 的确认事件（stub 直接触发）
    const confirm = wrapper.findComponent({ name: 'ConfirmDialog' })
    confirm.vm.$emit('confirm')
    await flushPromises()

    expect(deletePlan).toHaveBeenCalledWith(42)
    expect(showSuccess).toHaveBeenCalled()
    expect(listPlans).toHaveBeenCalledTimes(2)
  })
})