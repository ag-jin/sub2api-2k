import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { ref } from 'vue'

import AppSidebar from '../AppSidebar.vue'
import { CONCRETE_PLATFORM_OPTIONS } from '@/constants/platforms'
import { ACCOUNTS_PATH, PLATFORM_QUERY_PARAM } from '@/views/admin/accountPlatformFilter'
import zh from '@/i18n/locales/zh'

const { setMobileOpen } = vi.hoisted(() => ({ setMobileOpen: vi.fn() }))

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

// 用真实的中文文案渲染：既验证子项可见文案，也顺带证明
// `admin.accounts.platforms.<platform>` 这套动态 key 在 zh 里确实存在。
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
      { path: '/admin/channels/pricing', component: { template: '<div />' } },
      { path: '/admin/channels/monitor', component: { template: '<div />' } },
      { path: '/admin/risk-control', component: { template: '<div />' } },
      { path: '/admin/prompt-audit', component: { template: '<div />' } },
      { path: '/admin/groups', component: { template: '<div />' } },
      { path: '/admin/redeem', component: { template: '<div />' } },
      { path: '/keys', component: { template: '<div />' } },
      // 侧边栏其余条目在测试里不需要真实页面，用兜底路由避免噪音警告。
      { path: '/:pathMatch(.*)*', component: { template: '<div />' } }
    ]
  })
}

async function mountSidebar(initialUrl = '/admin/dashboard') {
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

/** 账号管理分组当前渲染出来的平台子项（按 href 上的筛选参数识别，不依赖 DOM 层级）。 */
function platformLinks(wrapper: VueWrapper) {
  return wrapper
    .findAll('a.sidebar-link')
    .filter(link => (link.attributes('href') ?? '').includes(`${PLATFORM_QUERY_PARAM}=`))
}

function linkHrefs(wrapper: VueWrapper): (string | undefined)[] {
  return platformLinks(wrapper).map(link => link.attributes('href'))
}

async function expandAccountsGroup(wrapper: VueWrapper) {
  await wrapper.get('#sidebar-channel-manage').trigger('click')
  await flushPromises()
}

describe('AppSidebar accounts platform children', () => {
  beforeEach(() => {
    setMobileOpen.mockReset()
  })

  it('renders one child per catalog platform, each linking to the filtered accounts URL', async () => {
    const { wrapper } = await mountSidebar()
    await expandAccountsGroup(wrapper)

    const links = platformLinks(wrapper)
    expect(links).toHaveLength(CONCRETE_PLATFORM_OPTIONS.length)

    expect(linkHrefs(wrapper)).toEqual(
      CONCRETE_PLATFORM_OPTIONS.map(
        option => `${ACCOUNTS_PATH}?${PLATFORM_QUERY_PARAM}=${option.value}`
      )
    )

    // 子项不带数量（用户明确选择的形态）。
    expect(links.map(link => link.text())).toEqual(
      CONCRETE_PLATFORM_OPTIONS.map(option => zh.admin.accounts.platforms[option.value])
    )
    expect(links.map(link => link.text()).join('')).not.toMatch(/\d/)
  })

  it('keeps the accounts group collapsed until the user expands it', async () => {
    const { wrapper } = await mountSidebar()
    expect(platformLinks(wrapper)).toHaveLength(0)

    await expandAccountsGroup(wrapper)
    expect(platformLinks(wrapper)).toHaveLength(CONCRETE_PLATFORM_OPTIONS.length)

    await wrapper.get('#sidebar-channel-manage').trigger('click')
    await flushPromises()
    expect(platformLinks(wrapper)).toHaveLength(0)
  })

  it('highlights only the child matching ?platform= and leaves the others inactive', async () => {
    const { wrapper } = await mountSidebar(`${ACCOUNTS_PATH}?${PLATFORM_QUERY_PARAM}=codebuddy`)

    const links = platformLinks(wrapper)
    expect(links).toHaveLength(CONCRETE_PLATFORM_OPTIONS.length)

    const activeHrefs = links
      .filter(link => link.classes().includes('sidebar-link-active'))
      .map(link => link.attributes('href'))
    expect(activeHrefs).toEqual([`${ACCOUNTS_PATH}?${PLATFORM_QUERY_PARAM}=codebuddy`])
  })

  it('keeps every child inactive when the accounts list has no platform filter', async () => {
    const { wrapper } = await mountSidebar(ACCOUNTS_PATH)

    const activeHrefs = platformLinks(wrapper)
      .filter(link => link.classes().includes('sidebar-link-active'))
      .map(link => link.attributes('href'))
    expect(activeHrefs).toEqual([])
  })

  it('clears the platform filter when the accounts group is collapsed', async () => {
    const { wrapper, router } = await mountSidebar(`${ACCOUNTS_PATH}?${PLATFORM_QUERY_PARAM}=gemini`)
    expect(router.currentRoute.value.query[PLATFORM_QUERY_PARAM]).toBe('gemini')

    await wrapper.get('#sidebar-channel-manage').trigger('click')
    await flushPromises()

    expect(router.currentRoute.value.query[PLATFORM_QUERY_PARAM]).toBeUndefined()
  })

  it('keeps every onboarding sidebar anchor on a real element', async () => {
    const { wrapper } = await mountSidebar()

    // 账号管理的锚点随条目一起变成了可展开组，driver.js 需要它仍能定位到元素。
    const accountsAnchor = wrapper.get('#sidebar-channel-manage')
    expect(accountsAnchor.text()).toContain(zh.nav.accounts)

    // 另外两个锚点走的是同一套 id 映射，一并回归，避免重构时漏掉。
    expect(wrapper.get('#sidebar-group-manage').text()).toContain(zh.nav.groups)
    expect(wrapper.get('#sidebar-wallet').text()).toContain(zh.nav.redeemCodes)
  })

  it('does not let the accounts parent navigate away from the list', async () => {
    const { wrapper, router } = await mountSidebar('/admin/dashboard')

    await wrapper.get('#sidebar-channel-manage').trigger('click')
    await flushPromises()

    expect(router.currentRoute.value.path).toBe('/admin/dashboard')
  })
})
