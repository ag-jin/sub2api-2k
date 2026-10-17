import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { mount } from '@vue/test-utils'

const {
  updateAccountMock,
  checkMixedChannelRiskMock,
  syncUpstreamModelsMock,
  showErrorMock,
  showInfoMock,
  authIsSimpleMode,
} = vi.hoisted(() => ({
  updateAccountMock: vi.fn(),
  checkMixedChannelRiskMock: vi.fn(),
  syncUpstreamModelsMock: vi.fn(),
  showErrorMock: vi.fn(),
  showInfoMock: vi.fn(),
  authIsSimpleMode: { value: true },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: showErrorMock,
    showSuccess: vi.fn(),
    showInfo: showInfoMock,
    showWarning: vi.fn(),
  }),
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    get isSimpleMode() {
      return authIsSimpleMode.value
    },
  }),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      update: updateAccountMock,
      checkMixedChannelRisk: checkMixedChannelRiskMock,
      syncUpstreamModels: syncUpstreamModelsMock,
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({}),
    },
    tlsFingerprintProfiles: {
      list: vi.fn().mockResolvedValue([]),
    },
  },
}))

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn(),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

import EditAccountModal from '../EditAccountModal.vue'

// 冻结 props 契约（coordinator 2026-10-16）：v-model config + modelOptions/candidates/
// candidatesState/disabled/error + emits update:modelValue、sync-targets。
const QuotaFallbackEditorStub = defineComponent({
  name: 'OpenCodeQuotaFallbackEditor',
  props: {
    modelValue: { type: Object, required: true },
    modelOptions: { type: Array, default: () => [] },
    candidates: { type: Array, default: () => [] },
    candidatesState: { type: String, default: 'idle' },
    disabled: { type: Boolean, default: false },
    error: { type: String, default: null },
  },
  emits: ['update:modelValue', 'sync-targets'],
  template: '<div data-testid="quota-fallback-editor-stub" />',
})

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
})

function buildOpenCodeGoAccount(
  credentials: Record<string, unknown> = {},
  platform = 'opencode_go'
) {
  return {
    id: 7,
    name: 'OpenCode GO',
    notes: '',
    platform,
    type: 'apikey',
    credentials: {
      account_mode: 'go',
      api_protocol: 'adaptive',
      base_url: 'https://opencode.ai/zen/go/v1',
      api_base_urls: {
        chat_completions: 'https://opencode.ai/zen/go/v1',
        anthropic: 'https://opencode.ai/zen/go',
        responses: 'https://opencode.ai/zen/go/v1',
      },
      model_mapping: { 'claude-sonnet-4-5': 'deepseek-v4.1-flash' },
      ...credentials,
    },
    credentials_status: { has_api_key: true },
    extra: {},
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    rate_multiplier: 1,
    status: 'active',
    group_ids: [],
    expires_at: null,
    auto_pause_on_expired: false,
  } as any
}

function mountModal(account: any) {
  return mount(EditAccountModal, {
    props: { show: true, account, proxies: [], groups: [] },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        Select: true,
        Icon: true,
        ProxySelector: true,
        GroupSelector: true,
        ModelWhitelistSelector: true,
        OpenCodeQuotaFallbackEditor: QuotaFallbackEditorStub,
      },
    },
  })
}

function findEditor(wrapper: ReturnType<typeof mountModal>) {
  return wrapper.findComponent(QuotaFallbackEditorStub)
}

async function selectButtonByText(wrapper: ReturnType<typeof mountModal>, text: string) {
  const button = wrapper.findAll('button').find((candidate) => candidate.text().includes(text))
  expect(button).toBeDefined()
  await button?.trigger('click')
}

async function save(wrapper: ReturnType<typeof mountModal>) {
  await wrapper.get('form#edit-account-form').trigger('submit.prevent')
  await vi.waitFor(() => expect(updateAccountMock).toHaveBeenCalledTimes(1))
  return updateAccountMock.mock.calls[0]?.[1]
}

describe('EditAccountModal OpenCode quota fallback', () => {
  beforeEach(() => {
    authIsSimpleMode.value = true
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })
    syncUpstreamModelsMock.mockReset().mockResolvedValue({ models: [], metadata: {} })
    showErrorMock.mockReset()
    showInfoMock.mockReset()
  })

  it('renders the editor for opencode_go go accounts and echoes the stored config', async () => {
    const account = buildOpenCodeGoAccount({
      quota_fallback: {
        enabled: true,
        threshold_percent: 95,
        model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' },
      },
    })
    updateAccountMock.mockResolvedValue(account)
    const wrapper = mountModal(account)

    const editor = findEditor(wrapper)
    expect(editor.exists()).toBe(true)
    expect(editor.props('modelValue')).toEqual({
      enabled: true,
      threshold_percent: 95,
      model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' },
    })
    expect(editor.props('modelOptions')).toEqual(['deepseek-v4.1-flash'])
  })

  it('hides the editor for opencode_go zen accounts', async () => {
    const account = buildOpenCodeGoAccount({ account_mode: 'zen' })
    updateAccountMock.mockResolvedValue(account)
    const wrapper = mountModal(account)

    expect(findEditor(wrapper).exists()).toBe(false)
  })

  it('hides the editor for non OpenCode platforms', async () => {
    const account = buildOpenCodeGoAccount(
      { account_mode: 'coding', model_mapping: {} },
      'kimi'
    )
    updateAccountMock.mockResolvedValue(account)
    const wrapper = mountModal(account)

    expect(findEditor(wrapper).exists()).toBe(false)
  })

  it('renders the editor for the legacy opencode platform with a default disabled config', async () => {
    const account = buildOpenCodeGoAccount({ account_mode: undefined, model_mapping: {} }, 'opencode')
    updateAccountMock.mockResolvedValue(account)
    const wrapper = mountModal(account)

    const editor = findEditor(wrapper)
    expect(editor.exists()).toBe(true)
    expect(editor.props('modelValue')).toEqual({
      enabled: false,
      threshold_percent: 98,
      model_mapping: {},
    })
  })

  it('saves a valid enabled config with the frozen schema', async () => {
    const account = buildOpenCodeGoAccount({
      quota_fallback: {
        enabled: false,
        threshold_percent: 98,
        model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' },
      },
    })
    updateAccountMock.mockResolvedValue(account)
    const wrapper = mountModal(account)

    await findEditor(wrapper).vm.$emit('update:modelValue', {
      enabled: true,
      threshold_percent: 90,
      model_mapping: { ' deepseek-v4.1-flash ': ' step-5-preview ' },
    })

    const payload = await save(wrapper)
    expect(payload?.credentials?.quota_fallback).toEqual({
      enabled: true,
      threshold_percent: 90,
      model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' },
    })
  })

  it('blocks saving with a translated error when the enabled config is invalid', async () => {
    const account = buildOpenCodeGoAccount()
    updateAccountMock.mockResolvedValue(account)
    const wrapper = mountModal(account)

    await findEditor(wrapper).vm.$emit('update:modelValue', {
      enabled: true,
      threshold_percent: 98,
      model_mapping: {},
    })

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await vi.waitFor(() => {
      expect(showErrorMock).toHaveBeenCalledWith(
        'admin.accounts.openCodeQuotaFallback.errors.mappingInvalid'
      )
    })
    expect(updateAccountMock).not.toHaveBeenCalled()
  })

  it('does not leak the enabled policy into zen when the mode is switched before saving', async () => {
    const account = buildOpenCodeGoAccount({
      quota_fallback: {
        enabled: true,
        threshold_percent: 98,
        model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' },
      },
    })
    updateAccountMock.mockResolvedValue(account)
    const wrapper = mountModal(account)

    await selectButtonByText(wrapper, 'admin.accounts.opencodeGo.accountMode.zen')
    const payload = await save(wrapper)

    expect(payload?.credentials?.account_mode).toBe('zen')
    expect(payload?.credentials?.quota_fallback).toEqual({
      enabled: false,
      threshold_percent: 98,
      model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' },
    })
  })

  it('keeps the stored disabled config in disabled form on save', async () => {
    const account = buildOpenCodeGoAccount({
      quota_fallback: {
        enabled: false,
        threshold_percent: 98,
        model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' },
      },
    })
    updateAccountMock.mockResolvedValue(account)
    const wrapper = mountModal(account)

    const payload = await save(wrapper)
    expect(payload?.credentials?.quota_fallback).toEqual({
      enabled: false,
      threshold_percent: 98,
      model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' },
    })
  })

  it('does not write a quota_fallback field for a fieldless legacy account that leaves the editor off', async () => {
    const account = buildOpenCodeGoAccount()
    updateAccountMock.mockResolvedValue(account)
    const wrapper = mountModal(account)

    const payload = await save(wrapper)
    expect('quota_fallback' in (payload?.credentials ?? {})).toBe(false)
  })

  it('preserves unknown credentials fields on save', async () => {
    const account = buildOpenCodeGoAccount({
      custom_local_field: 'keep-me',
      quota_fallback: {
        enabled: false,
        threshold_percent: 98,
        model_mapping: {},
      },
    })
    updateAccountMock.mockResolvedValue(account)
    const wrapper = mountModal(account)

    const payload = await save(wrapper)
    expect(payload?.credentials?.custom_local_field).toBe('keep-me')
  })

  it('loads upstream candidates on sync-targets for the saved account', async () => {
    syncUpstreamModelsMock.mockResolvedValue({
      models: ['step-5-preview', 'free-model'],
      metadata: { 'step-5-preview': { id: 'step-5-preview', display_name: 'Step 5 Preview' } },
    })
    const account = buildOpenCodeGoAccount()
    updateAccountMock.mockResolvedValue(account)
    const wrapper = mountModal(account)

    await findEditor(wrapper).vm.$emit('sync-targets')
    await vi.waitFor(() => expect(syncUpstreamModelsMock).toHaveBeenCalledWith(7))
    await wrapper.vm.$nextTick()

    expect(findEditor(wrapper).props('candidates')).toEqual([
      { id: 'step-5-preview', label: 'Step 5 Preview', capability: 'unknown' },
      { id: 'free-model', capability: 'unknown' },
    ])
    expect(findEditor(wrapper).props('candidatesState')).toBe('ready')
  })

  it('drops in-flight candidates when the account changes (no cross-account leak)', async () => {
    let resolveFetch: (value: unknown) => void = () => {}
    syncUpstreamModelsMock.mockReturnValue(
      new Promise((resolve) => {
        resolveFetch = resolve
      })
    )
    const account = buildOpenCodeGoAccount()
    updateAccountMock.mockResolvedValue(account)
    const wrapper = mountModal(account)

    await findEditor(wrapper).vm.$emit('sync-targets')
    expect(findEditor(wrapper).props('candidatesState')).toBe('loading')

    const otherAccount = { ...buildOpenCodeGoAccount(), id: 8, name: 'Other OpenCode' }
    await wrapper.setProps({ account: otherAccount })
    resolveFetch({ models: ['stale-model'], metadata: {} })
    await vi.waitFor(() => expect(wrapper.findComponent(QuotaFallbackEditorStub).exists()).toBe(true))

    expect(findEditor(wrapper).props('candidates')).toEqual([])
    expect(findEditor(wrapper).props('candidatesState')).toBe('idle')
  })
})
