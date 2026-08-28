import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ModelPlazaContent from '../ModelPlazaContent.vue'
import type { PricingCatalog } from '@/api/modelPlaza'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    isAuthenticated: false
  })
}))

function tokenProtocol(protocol: string, direct = true) {
  return {
    protocol,
    direct,
    billing_mode: 'token' as const,
    pricing: {
      input_price: 3e-6,
      output_price: 1.5e-5,
      cache_write_price: null,
      cache_read_price: null,
      image_input_price: null,
      image_output_price: null,
      per_request_price: null,
      intervals: []
    }
  }
}

function catalog(): PricingCatalog {
  return {
    description: '',
    plans: [
      {
        code: 'standard',
        name: 'Standard',
        description: 'Standard plan',
        models: [
          {
            id: 'claude-sonnet-4-5',
            display_name: 'Claude Sonnet 4.5',
            protocols: [tokenProtocol('anthropic'), tokenProtocol('openai', false)]
          },
          {
            id: 'gpt-5.6',
            display_name: 'GPT-5.6',
            protocols: [tokenProtocol('openai')]
          }
        ]
      },
      {
        code: 'pro',
        name: 'Pro',
        description: 'Pro plan',
        models: [
          {
            id: 'deepseek-r2',
            display_name: 'DeepSeek R2',
            protocols: [tokenProtocol('deepseek')]
          }
        ]
      }
    ]
  }
}

function mountContent(response: PricingCatalog | null, overrides: { loading?: boolean; error?: boolean } = {}) {
  return mount(ModelPlazaContent, {
    props: {
      response,
      loading: overrides.loading ?? false,
      error: overrides.error ?? false
    }
  })
}

describe('ModelPlazaContent (PricingPlan 目录页)', () => {
  it('加载中只显示 spinner,出错显示 loadFailed', () => {
    const loading = mountContent(null, { loading: true })
    expect(loading.find('.animate-spin').exists()).toBe(true)
    expect(loading.text()).not.toContain('standard')

    const error = mountContent(null, { error: true })
    expect(error.text()).toContain('modelPlaza.loadFailed')
  })

  it('空目录显示 empty;未登录显示匿名提示', () => {
    const wrapper = mountContent({ description: '', plans: [] })
    expect(wrapper.text()).toContain('modelPlaza.empty')
    expect(wrapper.text()).toContain('modelPlaza.anonymousHint')
  })

  it('按套餐渲染目录:代号/名称/描述 + 模型协议行', () => {
    const wrapper = mountContent(catalog())
    const text = wrapper.text()
    expect(text).toContain('standard')
    expect(text).toContain('Standard')
    expect(text).toContain('Standard plan')
    expect(text).toContain('pro')
    expect(text).toContain('Pro')
    expect(text).toContain('Claude Sonnet 4.5')
    expect(text).toContain('GPT-5.6')
    expect(text).toContain('DeepSeek R2')
    // 两个协议行(anthropic 直连 + openai 中转)
    expect(wrapper.findAll('tbody tr')).toHaveLength(4)
  })

  it('套餐筛选:点击套餐 chip 后只保留该套餐', async () => {
    const wrapper = mountContent(catalog())
    await wrapper.findAll('button').find((b) => b.text() === 'Pro')!.trigger('click')
    await flushPromises()
    const text = wrapper.text()
    expect(text).toContain('DeepSeek R2')
    expect(text).not.toContain('Claude Sonnet 4.5')
    expect(text).not.toContain('GPT-5.6')
    expect(wrapper.findAll('tbody tr')).toHaveLength(1)
  })

  it('协议筛选:只留命中协议行,不含该协议的模型与套餐整体隐藏', async () => {
    const wrapper = mountContent(catalog())
    await wrapper.findAll('button').find((b) => b.text() === 'anthropic')!.trigger('click')
    await flushPromises()
    const text = wrapper.text()
    expect(text).toContain('Claude Sonnet 4.5')
    expect(text).not.toContain('GPT-5.6')
    expect(text).not.toContain('DeepSeek R2')
    expect(wrapper.findAll('tbody tr')).toHaveLength(1)
  })

  it('模型搜索:展示名与模型标识均可命中,无命中显示 noSearchResult', async () => {
    const wrapper = mountContent(catalog())
    await wrapper.find('input').setValue('deepseek')
    await flushPromises()
    expect(wrapper.text()).toContain('DeepSeek R2')
    expect(wrapper.text()).not.toContain('Claude Sonnet 4.5')

    await wrapper.find('input').setValue('gpt-5.6')
    await flushPromises()
    expect(wrapper.text()).toContain('GPT-5.6')
    expect(wrapper.text()).not.toContain('DeepSeek R2')

    await wrapper.find('input').setValue('zzz-no-match')
    await flushPromises()
    expect(wrapper.text()).toContain('modelPlaza.noSearchResult')
  })
})