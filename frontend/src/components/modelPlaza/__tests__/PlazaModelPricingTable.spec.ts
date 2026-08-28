import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import PlazaModelPricingTable from '../PlazaModelPricingTable.vue'
import type { CatalogModel, CatalogProtocol } from '@/api/modelPlaza'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

function protocol(overrides: Partial<CatalogProtocol> = {}): CatalogProtocol {
  return {
    protocol: 'anthropic',
    direct: true,
    billing_mode: 'token',
    pricing: {
      input_price: 3e-6,
      output_price: 1.5e-5,
      cache_write_price: 3.75e-6,
      cache_read_price: 3e-7,
      image_input_price: null,
      image_output_price: null,
      per_request_price: null,
      intervals: []
    },
    ...overrides
  }
}

function model(overrides: Partial<CatalogModel> = {}): CatalogModel {
  return {
    id: 'claude-sonnet-4-5',
    display_name: 'Claude Sonnet 4.5',
    protocols: [protocol()],
    ...overrides
  }
}

function mountTable(models: CatalogModel[]) {
  return mount(PlazaModelPricingTable, { props: { models } })
}

describe('PlazaModelPricingTable (PricingPlan 目录表)', () => {
  it('按协议行渲染:展示名 + 模型标识 + 输入/输出/缓存价($/1M,保底 2 位小数)', () => {
    const wrapper = mountTable([model()])
    const text = wrapper.text()
    expect(text).toContain('Claude Sonnet 4.5')
    expect(text).toContain('claude-sonnet-4-5')
    expect(text).toContain('$3.00')
    expect(text).toContain('$15.00')
    // 缓存写 / 读(超过 2 位小数原样保留)
    expect(text).toContain('$3.75')
    expect(text).toContain('$0.30')
  })

  it('协议行包含协议名/计费模式/直连徽章;direct=false 展示中转', () => {
    const direct = mountTable([model()])
    expect(direct.text()).toContain('anthropic')
    expect(direct.text()).toContain('modelPlaza.table.billingToken')
    // 表头也有 Direct 列名,断言行内末列徽章
    const lastCell = direct.findAll('tbody tr td').at(-1)!
    expect(lastCell.text()).toBe('modelPlaza.table.direct')

    const relay = mountTable([model({ protocols: [protocol({ direct: false })] })])
    const relayCell = relay.findAll('tbody tr td').at(-1)!
    expect(relayCell.text()).toBe('modelPlaza.table.relay')
  })

  it('多协议模型:每个协议各占一行,模型单元格跨协议行合并', () => {
    const m = model({
      protocols: [protocol({ protocol: 'openai', direct: true }), protocol({ protocol: 'anthropic', direct: false })]
    })
    const wrapper = mountTable([m])
    const rows = wrapper.findAll('tbody tr')
    expect(rows).toHaveLength(2)
    // 协议行按协议名排序:anthropic 在前;模型名 + 标识只在首行(合并单元格)
    expect(rows[0].text()).toContain('Claude Sonnet 4.5')
    expect(rows[0].text()).toContain('anthropic')
    expect(rows[1].text()).not.toContain('Claude Sonnet 4.5')
    expect(rows[1].text()).toContain('openai')
    expect(rows[0].find('td').attributes('rowspan')).toBe('2')
  })

  it('token 协议行排前,按次/按图沉底;同组按输出价降序,同价按展示名降序', () => {
    const tokenExpensive = model({
      id: 'gpt-5.6',
      display_name: 'GPT-5.6',
      protocols: [protocol({ protocol: 'openai', pricing: { ...protocol().pricing, input_price: 1e-5, output_price: 7.5e-5 } })]
    })
    const tokenCheap = model({
      id: 'gpt-5.6-luna',
      display_name: 'GPT-5.6 Luna',
      protocols: [protocol({ protocol: 'openai', pricing: { ...protocol().pricing, input_price: 1e-6, output_price: 5e-6 } })]
    })
    const perRequest = model({
      id: 'search-tool',
      display_name: 'Search Tool',
      protocols: [protocol({
        protocol: 'openai',
        billing_mode: 'per_request',
        pricing: { ...protocol().pricing, input_price: null, output_price: null, cache_write_price: null, cache_read_price: null, per_request_price: 0.04 }
      })]
    })

    const wrapper = mountTable([perRequest, tokenCheap, tokenExpensive])
    const rows = wrapper.findAll('tbody tr')
    expect(rows[0].find('td').text()).toContain('GPT-5.6')
    expect(rows[1].find('td').text()).toContain('GPT-5.6 Luna')
    expect(rows[2].find('td').text()).toContain('Search Tool')
  })

  it('按次计费协议行展示单次价 + / 次后缀,缓存列为 -', () => {
    const m = model({
      protocols: [protocol({
        protocol: 'openai',
        billing_mode: 'per_request',
        pricing: { ...protocol().pricing, input_price: null, output_price: null, cache_write_price: null, cache_read_price: null, per_request_price: 0.04 }
      })]
    })
    const text = mountTable([m]).text()
    expect(text).toContain('$0.04')
    expect(text).toContain('modelPlaza.table.perRequest')
    expect(text).toContain('modelPlaza.table.perUnitRequest')
  })

  it('token 阶梯定价内联进输入/输出列;按次阶梯以芯片展示', () => {
    const tiered = model({
      protocols: [protocol({
        pricing: {
          ...protocol().pricing,
          cache_write_price: null,
          cache_read_price: null,
          intervals: [
            { min_tokens: 0, max_tokens: 200000, tier_label: '', input_price: 3e-6, output_price: 1.5e-5, cache_write_price: null, cache_read_price: null, per_request_price: null },
            { min_tokens: 200000, max_tokens: null, tier_label: '', input_price: 6e-6, output_price: 3e-5, cache_write_price: null, cache_read_price: null, per_request_price: null }
          ]
        }
      })]
    })
    const text = mountTable([tiered]).text()
    expect(text).toContain('≤200K')
    expect(text).toContain('>200K')
    expect(text).toContain('$3.00')
    expect(text).toContain('$6.00')
    expect(text).toContain('$30.00')

    const chip = model({
      id: 'gpt-image-2',
      display_name: 'GPT Image 2',
      protocols: [protocol({
        protocol: 'openai',
        billing_mode: 'image',
        pricing: {
          ...protocol().pricing,
          input_price: null,
          output_price: null,
          per_request_price: null,
          intervals: [
            { min_tokens: 0, max_tokens: null, tier_label: '1K', input_price: null, output_price: null, cache_write_price: null, cache_read_price: null, per_request_price: 0.01 }
          ]
        }
      })]
    })
    const chipText = mountTable([chip]).text()
    expect(chipText).toContain('modelPlaza.table.perImage')
    expect(chipText).toContain('1K')
    expect(chipText).toContain('$0.01')
    expect(chipText).toContain('modelPlaza.table.perUnitImage')
    // 非 token:缓存列无价
    expect(chipText).not.toContain('modelPlaza.table.cacheWrite')
  })

  it('无协议的模型不渲染任何行', () => {
    const wrapper = mountTable([model({ protocols: [] })])
    expect(wrapper.findAll('tbody tr')).toHaveLength(0)
  })

  it('无输入/输出价时展示 -', () => {
    const m = model({
      protocols: [protocol({
        pricing: { ...protocol().pricing, input_price: null, output_price: null, cache_write_price: null, cache_read_price: null }
      })]
    })
    const wrapper = mountTable([m])
    const priceCell = wrapper.findAll('tbody tr td')[3]
    expect(priceCell.text()).toContain('modelPlaza.table.input')
    expect(priceCell.text()).toContain('modelPlaza.table.output')
    expect(priceCell.text()).toContain('-')
    // 缓存列同样为 -
    const cacheCell = wrapper.findAll('tbody tr td')[4]
    expect(cacheCell.text()).toBe('-')
  })

  it('多档乱序时按下限升序展示(兜底排序)', () => {
    const m = model({
      protocols: [protocol({
        pricing: {
          ...protocol().pricing,
          cache_write_price: null,
          cache_read_price: null,
          intervals: [
            { min_tokens: 200000, max_tokens: null, tier_label: '', input_price: 6e-6, output_price: 3e-5, cache_write_price: null, cache_read_price: null, per_request_price: null },
            { min_tokens: 0, max_tokens: 200000, tier_label: '', input_price: 3e-6, output_price: 1.5e-5, cache_write_price: null, cache_read_price: null, per_request_price: null }
          ]
        }
      })]
    })
    const text = mountTable([m]).text()
    const firstTier = text.indexOf('≤200K')
    const secondTier = text.indexOf('>200K')
    expect(firstTier).toBeGreaterThanOrEqual(0)
    expect(secondTier).toBeGreaterThan(firstTier)
  })

  it('多档缓存价按档分行,每档一行与输入/输出列对齐;无档缓存价时沿用平价两行', () => {
    const tiered = model({
      protocols: [protocol({
        pricing: {
          ...protocol().pricing,
          cache_write_price: null,
          cache_read_price: null,
          intervals: [
            { min_tokens: 0, max_tokens: 200000, tier_label: '', input_price: 3e-6, output_price: 1.5e-5, cache_write_price: 3.75e-6, cache_read_price: 3e-7, per_request_price: null },
            { min_tokens: 200000, max_tokens: null, tier_label: '', input_price: 6e-6, output_price: 3e-5, cache_write_price: 7.5e-6, cache_read_price: 6e-7, per_request_price: null }
          ]
        }
      })]
    })
    const cacheCell = mountTable([tiered]).findAll('tbody tr td')[4]
    const tierRows = cacheCell.findAll('.leading-5')
    expect(tierRows).toHaveLength(2)
    expect(tierRows[0].text()).toContain('modelPlaza.table.cacheWriteShort')
    expect(tierRows[0].text()).toContain('$3.75')
    expect(tierRows[0].text()).toContain('$0.30')
    expect(tierRows[1].text()).toContain('$7.50')
    expect(tierRows[1].text()).toContain('$0.60')
    // 档位无缓存价的档渲染 -,输入/输出列仍按档
    const partial = model({
      protocols: [protocol({
        pricing: {
          ...protocol().pricing,
          cache_write_price: null,
          cache_read_price: null,
          intervals: [
            { min_tokens: 0, max_tokens: 200000, tier_label: '', input_price: 3e-6, output_price: 1.5e-5, cache_write_price: null, cache_read_price: null, per_request_price: null },
            { min_tokens: 200000, max_tokens: null, tier_label: '', input_price: 6e-6, output_price: 3e-5, cache_write_price: 7.5e-6, cache_read_price: 6e-7, per_request_price: null }
          ]
        }
      })]
    })
    const partialRows = mountTable([partial]).findAll('tbody tr td')[4].findAll('.leading-5')
    expect(partialRows).toHaveLength(2)
    expect(partialRows[0].text()).toBe('-')
    expect(partialRows[1].text()).toContain('$7.50')
  })

  it('无阶梯但有平价缓存价时沿用写入/读取两行', () => {
    const wrapper = mountTable([model()])
    const cacheCell = wrapper.findAll('tbody tr td')[4]
    expect(cacheCell.findAll('.leading-5')).toHaveLength(0)
    expect(cacheCell.text()).toContain('modelPlaza.table.cacheWrite')
    expect(cacheCell.text()).toContain('modelPlaza.table.cacheRead')
    expect(cacheCell.text()).toContain('$3.75')
  })
})