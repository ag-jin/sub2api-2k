import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'

import AccountsView from '../AccountsView.vue'
import { ACCOUNTS_PATH, PLATFORM_QUERY_PARAM } from '../accountPlatformFilter'

const {
  listAccounts,
  listWithEtag,
  getBatchTodayStats,
  getAllProxies,
  getAllGroups,
  showError
} = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  listWithEtag: vi.fn(),
  getBatchTodayStats: vi.fn(),
  getAllProxies: vi.fn(),
  getAllGroups: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      listWithEtag,
      getBatchTodayStats,
      getUpstreamBillingProbeSettings: vi.fn().mockResolvedValue({ enabled: true, interval_minutes: 30 }),
      delete: vi.fn(),
      batchClearError: vi.fn(),
      batchRefresh: vi.fn(),
      toggleSchedulable: vi.fn()
    },
    proxies: { getAll: getAllProxies },
    groups: { getAll: getAllGroups }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showSuccess: vi.fn(), showInfo: vi.fn(), showWarning: vi.fn() })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ token: 'test-token', isSimpleMode: false })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

// 只桩掉取数入口：AccountTableFilters 被替换成一个能透传「页内手动选平台」的桩，
// 用来验证 URL 与页内筛选之间的优先级。
const AccountTableFiltersStub = {
  props: ['searchQuery', 'filters'],
  emits: ['update:filters', 'change'],
  methods: {
    // 复刻真实筛选栏的行为：先更新 filters，再发 change 触发（防抖）重查。
    pickPlatform(value: string) {
      this.$emit('update:filters', { platform: value })
      this.$emit('change')
    }
  },
  template: '<button data-test="pick-platform-openai" @click="pickPlatform(\'openai\')">pick</button>'
}

function mountView(router: Router) {
  return mount(AccountsView, {
    global: {
      plugins: [router],
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: {
          template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>'
        },
        DataTable: { props: ['columns', 'data'], template: '<div data-test="data-table"></div>' },
        HelpTooltip: true,
        Pagination: true,
        ConfirmDialog: true,
        AccountTableActions: { template: '<div><slot name="beforeCreate" /><slot name="after" /></div>' },
        AccountTableFilters: AccountTableFiltersStub,
        AccountBulkActionsBar: true,
        AccountActionMenu: true,
        ImportDataModal: true,
        ReAuthAccountModal: true,
        AccountTestModal: true,
        AccountStatsModal: true,
        ScheduledTestsPanel: true,
        SyncFromCrsModal: true,
        TempUnschedStatusModal: true,
        ErrorPassthroughRulesModal: true,
        TLSFingerprintProfilesModal: true,
        CreateAccountModal: true,
        EditAccountModal: true,
        BulkEditAccountModal: true,
        PlatformTypeBadge: true,
        AccountCapacityCell: true,
        AccountStatusIndicator: true,
        AccountTodayStatsCell: true,
        AccountGroupsCell: true,
        AccountUsageCell: true,
        UpstreamBillingRateCell: true,
        Icon: true
      }
    }
  })
}

async function mountAt(initialUrl: string) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: ACCOUNTS_PATH, name: 'AdminAccounts', component: { template: '<div />' } },
      { path: '/:pathMatch(.*)*', component: { template: '<div />' } }
    ]
  })
  await router.push(initialUrl)
  await router.isReady()

  const wrapper = mountView(router)
  await flushPromises()
  return { wrapper, router }
}

/** 列表请求实际带上的 platform 筛选值（第 3 个参数是 params）。 */
function requestedPlatforms(): string[] {
  return listAccounts.mock.calls.map(call => call[2]?.platform)
}

describe('admin AccountsView platform filter from URL', () => {
  beforeEach(() => {
    localStorage.clear()

    listAccounts.mockReset()
    listWithEtag.mockReset()
    getBatchTodayStats.mockReset()
    getAllProxies.mockReset()
    getAllGroups.mockReset()
    showError.mockReset()

    listAccounts.mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20, pages: 0 })
    listWithEtag.mockResolvedValue({ notModified: true, etag: null, data: null })
    getBatchTodayStats.mockResolvedValue({ stats: {} })
    getAllProxies.mockResolvedValue([])
    getAllGroups.mockResolvedValue([])
  })

  it('seeds the platform filter from ?platform= on first load', async () => {
    const wrapper = (await mountAt(`${ACCOUNTS_PATH}?${PLATFORM_QUERY_PARAM}=anthropic`)).wrapper
    await flushPromises()

    expect(requestedPlatforms()[0]).toBe('anthropic')
    wrapper.unmount()
  })

  it('starts unfiltered when the URL carries no platform', async () => {
    const wrapper = (await mountAt(ACCOUNTS_PATH)).wrapper
    await flushPromises()

    expect(requestedPlatforms()[0]).toBe('')
    wrapper.unmount()
  })

  // 本任务的核心风险：从侧边栏切平台时组件不会重新挂载，只是 query 变了。
  // initialParams 只读一次，漏掉这个 watch 就会出现"点第二个平台没反应"。
  it('re-filters the list when the URL platform changes without remounting', async () => {
    const { wrapper, router } = await mountAt(`${ACCOUNTS_PATH}?${PLATFORM_QUERY_PARAM}=anthropic`)
    await flushPromises()
    expect(requestedPlatforms()).toEqual(['anthropic'])

    await router.push(`${ACCOUNTS_PATH}?${PLATFORM_QUERY_PARAM}=gemini`)
    await flushPromises()

    expect(wrapper.vm).toBeTruthy() // 组件实例未重建（同一 wrapper 仍然有效）
    expect(requestedPlatforms().at(-1)).toBe('gemini')
    wrapper.unmount()
  })

  it('keeps the existing results when the URL platform is unchanged', async () => {
    const { wrapper, router } = await mountAt(`${ACCOUNTS_PATH}?${PLATFORM_QUERY_PARAM}=gemini`)
    await flushPromises()
    const callsBefore = listAccounts.mock.calls.length

    // 同一个平台重复进入（例如点当前已高亮的子项）不应触发重复请求。
    await router.push(`${ACCOUNTS_PATH}?${PLATFORM_QUERY_PARAM}=gemini`)
    await flushPromises()

    expect(listAccounts.mock.calls.length).toBe(callsBefore)
    wrapper.unmount()
  })

  it('clears the filter back to all platforms when the query is dropped', async () => {
    const { wrapper, router } = await mountAt(`${ACCOUNTS_PATH}?${PLATFORM_QUERY_PARAM}=kimi`)
    await flushPromises()

    await router.push(ACCOUNTS_PATH)
    await flushPromises()

    expect(requestedPlatforms().at(-1)).toBe('')
    wrapper.unmount()
  })

  it('leaves an in-page platform choice alone while the URL has no platform', async () => {
    const { wrapper } = await mountAt(ACCOUNTS_PATH)
    await flushPromises()
    const callsBefore = listAccounts.mock.calls.length

    // URL 未携带平台时不与页内筛选争夺：用户手动选的平台不会被清掉。
    await wrapper.get('[data-test="pick-platform-openai"]').trigger('click')
    // 页内筛选走 useTableLoader 的 300ms 防抖重查。
    await new Promise(resolve => setTimeout(resolve, 350))
    await flushPromises()

    expect(listAccounts.mock.calls.length).toBeGreaterThan(callsBefore)
    expect(requestedPlatforms().at(-1)).toBe('openai')
    wrapper.unmount()
  })
})
