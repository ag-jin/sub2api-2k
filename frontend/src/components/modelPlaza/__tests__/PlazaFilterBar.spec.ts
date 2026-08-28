import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import PlazaFilterBar from '../PlazaFilterBar.vue'
import type { PricingPlan } from '@/api/modelPlaza'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

function plan(code: string, name: string, protocols: string[]): PricingPlan {
  return {
    code,
    name,
    description: '',
    models: [
      {
        id: `model-${code}`,
        display_name: `Model ${code}`,
        protocols: protocols.map((p) => ({
          protocol: p,
          direct: true,
          billing_mode: 'token',
          pricing: {
            input_price: 1e-6,
            output_price: 2e-6,
            cache_write_price: null,
            cache_read_price: null,
            image_input_price: null,
            image_output_price: null,
            per_request_price: null,
            intervals: []
          }
        }))
      }
    ]
  }
}

function mountBar(plans: PricingPlan[], props: { plan?: string; protocol?: string; search?: string } = {}) {
  return mount(PlazaFilterBar, {
    props: {
      plans,
      protocols: [...new Set(plans.flatMap((p) => p.models.flatMap((m) => m.protocols.map((r) => r.protocol))))].sort(),
      plan: 'all',
      protocol: 'all',
      search: '',
      ...props
    }
  })
}

describe('PlazaFilterBar (套餐/协议/模型搜索)', () => {
  it('渲染套餐与协议 chip,点击后发出对应 update 事件', async () => {
    const wrapper = mountBar([plan('standard', 'Standard', ['openai']), plan('pro', 'Pro', ['anthropic'])])
    expect(wrapper.text()).toContain('Standard')
    expect(wrapper.text()).toContain('Pro')
    expect(wrapper.text()).toContain('openai')
    expect(wrapper.text()).toContain('anthropic')

    await wrapper.findAll('button').find((b) => b.text() === 'Standard')!.trigger('click')
    expect(wrapper.emitted('update:plan')).toEqual([['standard']])

    await wrapper.findAll('button').find((b) => b.text() === 'anthropic')!.trigger('click')
    expect(wrapper.emitted('update:protocol')).toEqual([['anthropic']])
  })

  it('搜索输入发出 update:search,清空按钮发出空串', async () => {
    const wrapper = mountBar([plan('standard', 'Standard', ['openai'])], { search: 'gpt' })
    const input = wrapper.find('input')
    await input.setValue('claude')
    expect(wrapper.emitted('update:search')).toEqual([['claude']])

    await wrapper.find('[aria-label="clear-search"]').trigger('click')
    expect(wrapper.emitted('update:search')!.at(-1)).toEqual([''])
  })

  it('协议维度联动置灰:当前套餐下不存在的协议不可点', async () => {
    // standard 只有 openai,pro 才有 anthropic
    const wrapper = mountBar([plan('standard', 'Standard', ['openai']), plan('pro', 'Pro', ['anthropic'])], {
      plan: 'standard'
    })
    const anthropic = wrapper.findAll('button').find((b) => b.text() === 'anthropic')!
    expect(anthropic.attributes('disabled')).toBeDefined()
    const openai = wrapper.findAll('button').find((b) => b.text() === 'openai')!
    expect(openai.attributes('disabled')).toBeUndefined()
  })

  it('套餐维度联动置灰:当前协议下无该协议行的套餐不可点', async () => {
    const wrapper = mountBar([plan('standard', 'Standard', ['openai']), plan('pro', 'Pro', ['anthropic'])], {
      protocol: 'openai'
    })
    const pro = wrapper.findAll('button').find((b) => b.text() === 'Pro')!
    expect(pro.attributes('disabled')).toBeDefined()
    const standard = wrapper.findAll('button').find((b) => b.text() === 'Standard')!
    expect(standard.attributes('disabled')).toBeUndefined()
  })
})