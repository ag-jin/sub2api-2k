import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const { get, post, put, del } = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
  put: vi.fn(),
  del: vi.fn()
}))

vi.mock('@/api/client', () => ({
  apiClient: { get, post, put, delete: del }
}))

import { pricingPlansAPI, type PricingPlanUpsertRequest } from '@/api/admin/pricingPlans'

function makePlan(overrides: Record<string, unknown> = {}) {
  return {
    id: 42,
    name: 'pro',
    title: 'Pro',
    description: '',
    status: 'active',
    is_public: false,
    sort_order: 0,
    created_at: '2026-08-01T00:00:00Z',
    updated_at: '2026-08-01T00:00:00Z',
    models: [],
    routes: [],
    ...overrides
  }
}

describe('admin pricing plans API', () => {
  beforeEach(() => {
    get.mockReset()
    post.mockReset()
    put.mockReset()
    del.mockReset()
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('lists plans without include_disabled by default', async () => {
    get.mockResolvedValue({ data: [makePlan()] })

    const plans = await pricingPlansAPI.list()

    expect(get).toHaveBeenCalledWith('/admin/pricing-plans', { params: undefined })
    expect(plans).toHaveLength(1)
    expect(plans[0].name).toBe('pro')
  })

  it('passes include_disabled=true to the list endpoint', async () => {
    get.mockResolvedValue({ data: [] })

    await pricingPlansAPI.list(true)

    expect(get).toHaveBeenCalledWith('/admin/pricing-plans', {
      params: { include_disabled: true }
    })
  })

  it('fetches the full detail by id', async () => {
    const detail = makePlan({
      models: [
        {
          id: 1,
          plan_id: 42,
          public_model: 'claude-sonnet-4',
          protocol: 'messages',
          upstream_model: '',
          direct: true,
          allow_compatibility_fallback: false,
          priority: 100,
          enabled: true,
          notes: '',
          pricing: { billing_mode: 'token', input_price: 1.5, output_price: 7.5 },
          created_at: '2026-08-01T00:00:00Z',
          updated_at: '2026-08-01T00:00:00Z'
        }
      ],
      routes: [{ id: 9, plan_id: 42, group_id: 3, priority: 0, enabled: true, created_at: '', updated_at: '' }]
    })
    get.mockResolvedValue({ data: detail })

    const plan = await pricingPlansAPI.getById(42)

    expect(get).toHaveBeenCalledWith('/admin/pricing-plans/42')
    expect(plan.models[0].pricing?.billing_mode).toBe('token')
    expect(plan.routes[0].group_id).toBe(3)
  })

  it('creates a plan with the full replace payload', async () => {
    const request: PricingPlanUpsertRequest = {
      name: 'pro',
      status: 'active',
      is_public: true,
      models: [{ public_model: 'gpt-4o', protocol: 'chat_completions', direct: true, enabled: true, priority: 100 }],
      routes: [{ group_id: 3, priority: 0, enabled: true }]
    }
    get.mockResolvedValue({})
    post.mockResolvedValue({ data: makePlan({ is_public: true }) })

    const created = await pricingPlansAPI.create(request)

    expect(post).toHaveBeenCalledWith('/admin/pricing-plans', request)
    expect(created.id).toBe(42)
  })

  it('updates a plan by id with the full replace payload', async () => {
    const request: PricingPlanUpsertRequest = {
      name: 'pro-v2',
      models: [],
      routes: []
    }
    put.mockResolvedValue({ data: makePlan({ name: 'pro-v2' }) })

    const updated = await pricingPlansAPI.update(42, request)

    expect(put).toHaveBeenCalledWith('/admin/pricing-plans/42', request)
    expect(updated.name).toBe('pro-v2')
  })

  it('deletes a plan by id', async () => {
    del.mockResolvedValue({ data: { message: 'Pricing plan deleted successfully' } })

    const res = await pricingPlansAPI.delete(42)

    expect(del).toHaveBeenCalledWith('/admin/pricing-plans/42')
    expect(res.message).toContain('deleted')
  })
})