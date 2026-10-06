import { flushPromises, mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

// useClipboard 在 setup 里取 app store（本文件不挂 pinia，注入桩）。
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showWarning: vi.fn(),
  }),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    grok: { getCapabilities: vi.fn().mockResolvedValue({ password_auth_enabled: false }) },
  },
}))

// 保留 vue-i18n 真实导出：useClipboard → @/i18n 的模块级 createI18n 需要它存在。
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

import OAuthAuthorizationFlow from '../OAuthAuthorizationFlow.vue'

type FlowPlatform = 'zhipu' | 'openai'

/** 授权码输入框在模板里以 placeholder（i18n 桩返回键名）区分。 */
function authCodePlaceholder(platform: FlowPlatform): string {
  return platform === 'openai'
    ? 'admin.accounts.oauth.openai.authCodePlaceholder'
    : 'admin.accounts.oauth.authCodePlaceholder'
}

function mountFlow(platform: FlowPlatform) {
  return mount(OAuthAuthorizationFlow, {
    props: { addMethod: 'oauth', platform },
  })
}

/** 模拟管理员把回调地址整条粘进授权码输入框，返回输入框里的可见文本。 */
async function pasteCallback(
  wrapper: ReturnType<typeof mountFlow>,
  platform: FlowPlatform,
  pasted: string
): Promise<string> {
  const field = wrapper.get(`textarea[placeholder="${authCodePlaceholder(platform)}"]`)
  await field.setValue(pasted)
  await flushPromises()
  return (field.element as HTMLTextAreaElement).value
}

// 票 34：智谱回调参数名是 authCode=（http://127.0.0.1:53699/oauth/callback/bigmodel?authCode=xxx&state=yyy），
// 整条 URL 粘进去时自动抽取 authCode 回填，否则会把整条 URL 当授权码发给后端而必失败。
describe('OAuthAuthorizationFlow zhipu authCode auto-extraction', () => {
  it('extracts authCode from a pasted bigmodel callback URL', async () => {
    const wrapper = mountFlow('zhipu')
    const value = await pasteCallback(
      wrapper,
      'zhipu',
      'http://127.0.0.1:53699/oauth/callback/bigmodel?authCode=abc123&state=st-1'
    )

    expect(value).toBe('abc123')
    wrapper.unmount()
  })

  it('falls back to regex extraction when the pasted callback is not a parsable URL', async () => {
    const wrapper = mountFlow('zhipu')
    const value = await pasteCallback(
      wrapper,
      'zhipu',
      'bigmodel/callback?authCode=regex-code&state=st-2'
    )

    expect(value).toBe('regex-code')
    wrapper.unmount()
  })

  it('leaves a bare authorization code untouched', async () => {
    const wrapper = mountFlow('zhipu')
    const value = await pasteCallback(wrapper, 'zhipu', 'bare-code-1')

    expect(value).toBe('bare-code-1')
    wrapper.unmount()
  })
})

// 回归：既有平台（code= 参数）的抽取逻辑保持不变。
describe('OAuthAuthorizationFlow callback code auto-extraction (regression)', () => {
  it('still extracts code= for openai callbacks', async () => {
    const wrapper = mountFlow('openai')
    const value = await pasteCallback(
      wrapper,
      'openai',
      'http://localhost:1455/auth/callback?code=code-xyz&state=st-3'
    )

    expect(value).toBe('code-xyz')
    wrapper.unmount()
  })
})
