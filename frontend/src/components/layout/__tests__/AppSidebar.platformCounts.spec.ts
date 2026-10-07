import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { ref } from 'vue'

import { CONCRETE_PLATFORM_OPTIONS } from '@/constants/platforms'
import { ACCOUNTS_PATH, PLATFORM_QUERY_PARAM } from '@/views/admin/accountPlatformFilter'
import zh from '@/i18n/locales/zh'

/**
 * 侧边栏「账号管理」子项的默认可见性：
 * 只显示有账号的平台，底部提供「显示全部平台」开关（状态存 localStorage）。
 */
const SHOW_ALL_PLATFORMS_STORAGE_KEY = 'sidebar.showAllPlatforms'
const TOGGLE_SELECTOR = '[data-testid="sidebar-accounts-platforms-toggle"]'

const { platformCounts } = vi.hoisted(() => ({ platformCounts: vi.fn() }))

vi.mock('@/api/admin/accounts', () => ({ platformCounts, default: { platformCounts } }))

const setMobileOpen = vi.fn()

const appStoreMock = {
  sidebarCollapsed: false,
  mobileOpen: false,
  siteName: 'Test Site',
  siteLogo: '',
  siteVersion: '0.0.0',
  publicSettingsLoaded: true,
  cachedPublicSettings: null,
  backendModeEnabled: false,
  sidebarScrollTop: 0,
  toggleSidebar: vi.fn(),
  setMobileOpen
}

vi.mock('@/stores/app', () => ({
  useAppStore: () => appStoreMock
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ isAdmin: true, isSimpleMode: false })
}))

vi.mock('@/stores/adminSettings', () => ({
  useAdminSettingsStore: () => ({
    opsMonitoringEnabled: true,
    paymentEnabled: true,
    customMenuItems: [],
    fetch: vi.fn()
  })
}))

vi.mock('@/stores/onboarding', () => ({
  useOnboardingStore: () => ({ isCurrentStep: () => false, nextStep: vi.fn() })
}))

vi.mock('@/composables/useBatchImageAccess', () => ({
  useBatchImageAccess: () => ({ canUseBatchImage: ref(false), refreshBatchImageAccess: vi.fn() })
}))

// 用真实中文文案渲染，顺带证明新增的 nav.showAllPlatforms / nav.collapseAllPlatforms 确实存在。
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  const lookup = (key: string): string =>
    key.split('.').reduce<any>((node, segment) => node?.[segment], zh) ?? key
  return { ...actual, useI18n: () => ({ t: lookup, locale: { value: 'zh' } }) }
})

function createTestRouter(): Router {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: ACCOUNTS_PATH, name: 'AdminAccounts', component: { template: '<div />' } },
      { path: '/admin/dashboard', component: { template: '<div />' } },
      // 侧边栏其余条目在测试里不需要真实页面，用兜底路由避免噪音警告。
      { path: '/:pathMatch(.*)*', component: { template: '<div />' } }
    ]
  })
}

// 组件内的平台计数带模块级缓存，用 resetModules + 动态 import 保证每个用例
// 都从"干净挂载"开始（否则先成功的用例会把计数缓存带给后面的用例）。
async function mountSidebar(initialUrl = '/admin/dashboard') {
  vi.resetModules()
  const { default: AppSidebar } = await import('../AppSidebar.vue')

  const router = createTestRouter()
  await router.push(initialUrl)
  await router.isReady()

  const wrapper = mount(AppSidebar, {
    global: {
      plugins: [router],
      stubs: { VersionBadge: true, Icon: true }
    }
  })
  await flushPromises()
  return { wrapper, router }
}

/** 账号管理分组当前渲染出来的平台子项（按 href 上的筛选参数识别）。 */
function platformLinks(wrapper: VueWrapper) {
  return wrapper
    .findAll('a.sidebar-link')
    .filter(link => (link.attributes('href') ?? '').includes(`${PLATFORM_QUERY_PARAM}=`))
}

function linkHrefs(wrapper: VueWrapper): (string | undefined)[] {
  return platformLinks(wrapper).map(link => link.attributes('href'))
}

function allPlatformHrefs(): string[] {
  return CONCRETE_PLATFORM_OPTIONS.map(
    option => `${ACCOUNTS_PATH}?${PLATFORM_QUERY_PARAM}=${option.value}`
  )
}

function platformHrefs(...platforms: string[]): string[] {
  return platforms.map(platform => `${ACCOUNTS_PATH}?${PLATFORM_QUERY_PARAM}=${platform}`)
}

function showAllToggle(wrapper: VueWrapper) {
  return wrapper.get(TOGGLE_SELECTOR)
}

async function expandAccountsGroup(wrapper: VueWrapper) {
  await wrapper.get('#sidebar-channel-manage').trigger('click')
  await flushPromises()
}

describe('AppSidebar accounts platform counts', () => {
  beforeEach(() => {
    platformCounts.mockReset()
    localStorage.clear()
    setMobileOpen.mockReset()
  })

  it('renders only platforms that have accounts, keeping the catalog order', async () => {
    platformCounts.mockResolvedValue({ zhipu: 1, anthropic: 3 })

    const { wrapper } = await mountSidebar()
    await expandAccountsGroup(wrapper)

    expect(linkHrefs(wrapper)).toEqual(platformHrefs('anthropic', 'zhipu'))
  })

  it('shows every catalog platform again after the show-all toggle is switched on and back off', async () => {
    platformCounts.mockResolvedValue({ zhipu: 1, anthropic: 3 })

    const { wrapper } = await mountSidebar()
    await expandAccountsGroup(wrapper)
    expect(linkHrefs(wrapper)).toEqual(platformHrefs('anthropic', 'zhipu'))

    await showAllToggle(wrapper).trigger('click')
    await flushPromises()
    expect(linkHrefs(wrapper)).toEqual(allPlatformHrefs())

    await showAllToggle(wrapper).trigger('click')
    await flushPromises()
    expect(linkHrefs(wrapper)).toEqual(platformHrefs('anthropic', 'zhipu'))
  })

  it('renders the toggle as the last row of the platform child list', async () => {
    platformCounts.mockResolvedValue({ zhipu: 1, anthropic: 3 })

    const { wrapper } = await mountSidebar()
    await expandAccountsGroup(wrapper)

    const container = platformLinks(wrapper)[0].element.parentElement
    const toggle = showAllToggle(wrapper)
    expect(container?.contains(toggle.element)).toBe(true)
    expect(container?.lastElementChild).toBe(toggle.element)
    expect(toggle.element.textContent).toContain(zh.nav.showAllPlatforms)
  })

  it('remembers the show-all preference in localStorage and restores it on the next mount', async () => {
    platformCounts.mockResolvedValue({ anthropic: 2 })

    const { wrapper } = await mountSidebar()
    await expandAccountsGroup(wrapper)
    expect(localStorage.getItem(SHOW_ALL_PLATFORMS_STORAGE_KEY)).toBeNull()

    await showAllToggle(wrapper).trigger('click')
    await flushPromises()
    expect(localStorage.getItem(SHOW_ALL_PLATFORMS_STORAGE_KEY)).toBe('true')
    expect(showAllToggle(wrapper).element.textContent).toContain(zh.nav.collapseAllPlatforms)

    // 下一次挂载：localStorage 里的偏好生效，默认就展开全部平台。
    const { wrapper: remounted } = await mountSidebar()
    await expandAccountsGroup(remounted)
    expect(linkHrefs(remounted)).toEqual(allPlatformHrefs())
  })

  it('restores an expanded platform list when the stored preference is true', async () => {
    localStorage.setItem(SHOW_ALL_PLATFORMS_STORAGE_KEY, 'true')
    platformCounts.mockResolvedValue({ anthropic: 2 })

    const { wrapper } = await mountSidebar()
    await expandAccountsGroup(wrapper)

    expect(linkHrefs(wrapper)).toEqual(allPlatformHrefs())
  })

  it('falls back to every platform when the counts request fails', async () => {
    platformCounts.mockRejectedValue(new Error('platform-counts unavailable'))

    const { wrapper } = await mountSidebar()
    await expandAccountsGroup(wrapper)

    expect(linkHrefs(wrapper)).toEqual(allPlatformHrefs())
    // 没有计数就没有可筛选的名单，开关会变成空按钮，故不渲染。
    expect(wrapper.find(TOGGLE_SELECTOR).exists()).toBe(false)
  })

  it('falls back to every platform when the endpoint returns no usable payload', async () => {
    platformCounts.mockResolvedValue(undefined)

    const { wrapper } = await mountSidebar()
    await expandAccountsGroup(wrapper)

    expect(linkHrefs(wrapper)).toEqual(allPlatformHrefs())
    expect(wrapper.find(TOGGLE_SELECTOR).exists()).toBe(false)
  })

  it('keeps the accounts group (and its toggle) when no platform owns an account', async () => {
    platformCounts.mockResolvedValue({})

    const { wrapper, router } = await mountSidebar('/admin/dashboard')
    await expandAccountsGroup(wrapper)

    // 可见子项为零，但父项仍必须是"只展开不跳转"的分组，否则开关无从点起。
    expect(linkHrefs(wrapper)).toEqual([])
    expect(router.currentRoute.value.path).toBe('/admin/dashboard')

    await showAllToggle(wrapper).trigger('click')
    await flushPromises()
    expect(linkHrefs(wrapper)).toEqual(allPlatformHrefs())
  })

  it('does not navigate away when the toggle is used while the selected platform is hidden', async () => {
    platformCounts.mockResolvedValue({ anthropic: 2 })

    const selectedUrl = `${ACCOUNTS_PATH}?${PLATFORM_QUERY_PARAM}=minimax`
    const { wrapper, router } = await mountSidebar(selectedUrl)
    await expandAccountsGroup(wrapper)

    // 选中的 minimax 没有账号，默认不出现；但 URL 仍是用户当前所在的位置。
    expect(linkHrefs(wrapper)).toEqual(platformHrefs('anthropic'))

    await showAllToggle(wrapper).trigger('click')
    await flushPromises()

    expect(router.currentRoute.value.fullPath).toBe(selectedUrl)
    expect(linkHrefs(wrapper)).toEqual(allPlatformHrefs())
  })

  it('only requests the counts once per mount', async () => {
    platformCounts.mockResolvedValue({ anthropic: 2 })

    const { wrapper } = await mountSidebar()
    await expandAccountsGroup(wrapper)
    await wrapper.get('#sidebar-channel-manage').trigger('click')
    await expandAccountsGroup(wrapper)

    expect(platformCounts).toHaveBeenCalledTimes(1)
  })
})
