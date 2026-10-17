import { defineComponent, nextTick } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  BUILTIN_PLATFORM_CATALOG,
  resetPlatformCatalog,
  setPlatformCatalog,
} from '@/constants/platformCatalog'

const {
  createAccountMock,
  probeUpstreamBillingMock,
  syncUpstreamModelsPreviewMock,
  syncUpstreamModelsMock,
  showErrorMock,
  showWarningMock,
  authIsSimpleMode,
} = vi.hoisted(() => ({
  createAccountMock: vi.fn(),
  probeUpstreamBillingMock: vi.fn(),
  syncUpstreamModelsPreviewMock: vi.fn(),
  syncUpstreamModelsMock: vi.fn(),
  showErrorMock: vi.fn(),
  showWarningMock: vi.fn(),
  authIsSimpleMode: { value: true },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: showErrorMock,
    showSuccess: vi.fn(),
    showWarning: showWarningMock,
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
      create: createAccountMock,
      probeUpstreamBilling: probeUpstreamBillingMock,
      syncUpstreamModels: syncUpstreamModelsMock,
      syncUpstreamModelsPreview: syncUpstreamModelsPreviewMock,
      checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false }),
      importCodexSession: vi.fn(),
      createOpenAICodexPAT: vi.fn(),
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({}),
    },
    zhipu: {
      generateLoginUrl: vi.fn(),
      exchange: vi.fn(),
      createFromLogin: vi.fn(),
    },
    tlsFingerprintProfiles: {
      list: vi.fn().mockResolvedValue([]),
    },
  },
}))

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn().mockResolvedValue([]),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

import CreateAccountModal from '../CreateAccountModal.vue'
import OpenCodeQuotaFallbackEditor from '../OpenCodeQuotaFallbackEditor.vue'

// 冻结 props 契约（coordinator 2026-10-16）：v-model config + modelOptions/candidates/
// candidatesState/disabled/error + emits update:modelValue、sync-targets。
// 用显式 stub 断言接线，不依赖 UI owner 组件内部结构。
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

const OAuthAuthorizationFlowStub = defineComponent({
  name: 'OAuthAuthorizationFlow',
  props: {
    showManualOption: Boolean,
    showCodexSessionImportOption: Boolean,
    showAgentIdentityOption: Boolean,
    showCodexPatOption: Boolean,
    initialInputMethod: String,
  },
  template: '<div />',
})

const GroupSelectorStub = defineComponent({
  name: 'GroupSelector',
  props: { modelValue: { type: Array, default: () => [] } },
  emits: ['update:modelValue'],
  template: '<button type="button" data-testid="select-pricing-groups" @click="$emit(\'update:modelValue\', [1])">groups</button>',
})

const ModelWhitelistSelectorStub = defineComponent({
  name: 'ModelWhitelistSelector',
  props: {
    modelValue: { type: Array, default: () => [] },
    platform: String,
    syncCredentials: Object,
  },
  emits: ['update:modelValue', 'upstream-synced'],
  template: '<button type="button" data-testid="model-whitelist-selector" @click="$emit(\'update:modelValue\', [\'public-glm\'])">models</button>',
})

function mountModal(groups: any[] = []) {
  return mount(CreateAccountModal, {
    props: { show: true, proxies: [], groups },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        OAuthAuthorizationFlow: OAuthAuthorizationFlowStub,
        ConfirmDialog: true,
        Select: true,
        Icon: true,
        PlatformIcon: true,
        ProxySelector: true,
        ProxyAdBanner: true,
        GroupSelector: GroupSelectorStub,
        ModelWhitelistSelector: ModelWhitelistSelectorStub,
        QuotaLimitCard: true,
        OpenCodeQuotaFallbackEditor: QuotaFallbackEditorStub,
      },
    },
  })
}

// 真实 Editor 需要可交互的 Select 才能构造“只选 target、source 为空”的草稿行。
const RealEditorSelectStub = defineComponent({
  name: 'Select',
  props: ['modelValue', 'options', 'disabled', 'error'],
  emits: ['update:modelValue'],
  template: `
    <div>
      <button type="button" data-testid="select-custom" @click="$emit('update:modelValue', 'free-choice')">custom</button>
    </div>
  `,
})

function mountModalWithRealEditor(groups: any[] = []) {
  return mount(CreateAccountModal, {
    props: { show: true, proxies: [], groups },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        OAuthAuthorizationFlow: OAuthAuthorizationFlowStub,
        ConfirmDialog: true,
        Select: RealEditorSelectStub,
        Icon: true,
        PlatformIcon: true,
        ProxySelector: true,
        ProxyAdBanner: true,
        GroupSelector: GroupSelectorStub,
        ModelWhitelistSelector: ModelWhitelistSelectorStub,
        QuotaLimitCard: true,
      },
    },
  })
}

async function selectButtonByText(wrapper: ReturnType<typeof mountModal>, text: string) {
  const button = wrapper.findAll('button').find((candidate) => candidate.text().includes(text))
  expect(button).toBeDefined()
  await button?.trigger('click')
}

function findEditor(wrapper: ReturnType<typeof mountModal>) {
  return wrapper.findComponent(QuotaFallbackEditorStub)
}

async function openOpenCodeGoForm(wrapper: ReturnType<typeof mountModal>) {
  await wrapper.get('[data-testid="platform-button-opencode_go"]').trigger('click')
  await selectButtonByText(wrapper, 'admin.accounts.opencodeGo.accountMode.go')
  await wrapper.get('form#create-account-form input[type="text"]').setValue('oc-go')
  await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-opencode-go')
}

async function submit(wrapper: ReturnType<typeof mountModal>) {
  await wrapper.get('form#create-account-form').trigger('submit.prevent')
  await flushPromises()
}

describe('CreateAccountModal OpenCode quota fallback', () => {
  beforeEach(() => {
    authIsSimpleMode.value = true
    resetPlatformCatalog()
    setPlatformCatalog(BUILTIN_PLATFORM_CATALOG)
    createAccountMock.mockReset().mockResolvedValue({ id: 42, platform: 'opencode_go', type: 'apikey' })
    probeUpstreamBillingMock.mockReset().mockResolvedValue({})
    syncUpstreamModelsMock.mockReset().mockResolvedValue({ models: [], metadata: {} })
    syncUpstreamModelsPreviewMock.mockReset().mockResolvedValue({ models: [], metadata: {} })
    showErrorMock.mockReset()
    showWarningMock.mockReset()
  })

  afterEach(() => {
    resetPlatformCatalog()
  })

  it('renders the editor for legacy opencode apikey accounts, disabled by default', async () => {
    const wrapper = mountModal()
    await wrapper.get('[data-testid="platform-button-opencode"]').trigger('click')

    const editor = findEditor(wrapper)
    expect(editor.exists()).toBe(true)
    expect(editor.props('modelValue')).toEqual({
      enabled: false,
      threshold_percent: 98,
      model_mapping: {},
    })
    // 白名单默认按平台模型列表预填（identity 映射），其 targets 即 source 候选
    const options = editor.props('modelOptions') as string[]
    expect(options.length).toBeGreaterThan(0)
    expect(options).toContain('deepseek-v4-flash')
    expect(editor.props('candidatesState')).toBe('idle')
  })

  it('hides the editor on opencode_go zen mode and shows it after switching to go', async () => {
    const wrapper = mountModal()
    await wrapper.get('[data-testid="platform-button-opencode_go"]').trigger('click')

    // opencode_go 默认 zen：无 5h 窗口，不展示
    expect(findEditor(wrapper).exists()).toBe(false)

    await selectButtonByText(wrapper, 'admin.accounts.opencodeGo.accountMode.go')
    expect(findEditor(wrapper).exists()).toBe(true)
  })

  it('never renders the editor for other platforms', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    expect(findEditor(wrapper).exists()).toBe(false)

    await selectButtonByText(wrapper, 'Kimi')
    expect(findEditor(wrapper).exists()).toBe(false)

    await selectButtonByText(wrapper, 'admin.accounts.cnProviders.accountMode.coding')
    expect(findEditor(wrapper).exists()).toBe(false)
  })

  it('feeds modelOptions from the normal model mapping actual upstream targets', async () => {
    const wrapper = mountModal()
    await wrapper.get('[data-testid="platform-button-opencode_go"]').trigger('click')
    await selectButtonByText(wrapper, 'admin.accounts.opencodeGo.accountMode.go')

    await selectButtonByText(wrapper, 'admin.accounts.modelMapping')
    await selectButtonByText(wrapper, 'admin.accounts.addMapping')
    await wrapper.get('input[placeholder="admin.accounts.requestModel"]').setValue('deepseek-flash')
    await wrapper.get('input[placeholder="admin.accounts.actualModel"]').setValue('deepseek-v4.1-flash')

    expect(findEditor(wrapper).props('modelOptions')).toEqual(['deepseek-v4.1-flash'])
  })

  it('submits the frozen schema when the editor enables the policy', async () => {
    const wrapper = mountModal()
    await openOpenCodeGoForm(wrapper)

    await findEditor(wrapper).vm.$emit('update:modelValue', {
      enabled: true,
      threshold_percent: 95,
      model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' },
    })
    await flushPromises()
    await submit(wrapper)

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials?.quota_fallback).toEqual({
      enabled: true,
      threshold_percent: 95,
      model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' },
    })
  })

  it('does not add quota_fallback when the editor stays untouched (legacy behavior)', async () => {
    const wrapper = mountModal()
    await openOpenCodeGoForm(wrapper)
    await submit(wrapper)

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials?.quota_fallback).toBeUndefined()
  })

  it('keeps a disabled config carrying the chosen mapping instead of dropping it', async () => {
    const wrapper = mountModal()
    await openOpenCodeGoForm(wrapper)

    await findEditor(wrapper).vm.$emit('update:modelValue', {
      enabled: false,
      threshold_percent: 98,
      model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' },
    })
    await flushPromises()
    await submit(wrapper)

    expect(createAccountMock.mock.calls[0]?.[0]?.credentials?.quota_fallback).toEqual({
      enabled: false,
      threshold_percent: 98,
      model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' },
    })
  })

  it('blocks submit with a translated threshold error instead of writing an invalid schema', async () => {
    const wrapper = mountModal()
    await openOpenCodeGoForm(wrapper)

    await findEditor(wrapper).vm.$emit('update:modelValue', {
      enabled: true,
      threshold_percent: 150,
      model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' },
    })
    await flushPromises()
    await submit(wrapper)

    expect(createAccountMock).not.toHaveBeenCalled()
    expect(showErrorMock).toHaveBeenCalledWith('admin.accounts.openCodeQuotaFallback.errors.thresholdInvalid')
  })

  it('blocks submit when enabled without a complete substitute mapping', async () => {
    const wrapper = mountModal()
    await openOpenCodeGoForm(wrapper)

    await findEditor(wrapper).vm.$emit('update:modelValue', {
      enabled: true,
      threshold_percent: 98,
      model_mapping: {},
    })
    await flushPromises()
    await submit(wrapper)

    expect(createAccountMock).not.toHaveBeenCalled()
    expect(showErrorMock).toHaveBeenCalledWith('admin.accounts.openCodeQuotaFallback.errors.mappingInvalid')
  })

  it('keeps the edited policy as disabled config when the account is switched back to zen before submit', async () => {
    const wrapper = mountModal()
    await openOpenCodeGoForm(wrapper)

    await findEditor(wrapper).vm.$emit('update:modelValue', {
      enabled: true,
      threshold_percent: 98,
      model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' },
    })
    await flushPromises()
    await selectButtonByText(wrapper, 'admin.accounts.opencodeGo.accountMode.zen')
    await submit(wrapper)

    const credentials = createAccountMock.mock.calls[0]?.[0]?.credentials
    expect(credentials?.account_mode).toBe('zen')
    // 与 Edit 一致：保留已编辑映射但强制 enabled=false（不静默丢弃、不泄漏启用策略）
    expect(credentials?.quota_fallback).toEqual({
      enabled: false,
      threshold_percent: 98,
      model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' },
    })
  })

  it('does not write an untouched quota fallback field on a zen account', async () => {
    const wrapper = mountModal()
    await wrapper.get('[data-testid="platform-button-opencode_go"]').trigger('click')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('oc-zen')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-opencode-zen')
    await submit(wrapper)

    expect(createAccountMock.mock.calls[0]?.[0]?.credentials?.quota_fallback).toBeUndefined()
  })

  it('loads upstream candidates on sync-targets and maps them to the frozen candidate shape', async () => {
    syncUpstreamModelsPreviewMock.mockResolvedValue({
      models: ['step-5-preview', 'free-model'],
      metadata: { 'step-5-preview': { id: 'step-5-preview', display_name: 'Step 5 Preview' } },
    })
    const wrapper = mountModal()
    await openOpenCodeGoForm(wrapper)
    await findEditor(wrapper).vm.$emit('update:modelValue', {
      enabled: true,
      threshold_percent: 98,
      model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' },
    })
    await flushPromises()

    await findEditor(wrapper).vm.$emit('sync-targets')
    await flushPromises()

    expect(syncUpstreamModelsPreviewMock).toHaveBeenCalledWith(
      expect.objectContaining({ platform: 'opencode_go', type: 'apikey', api_key: 'sk-opencode-go' })
    )
    expect(findEditor(wrapper).props('candidates')).toEqual([
      { id: 'step-5-preview', label: 'Step 5 Preview', capability: 'unknown' },
      { id: 'free-model', capability: 'unknown' },
    ])
    expect(findEditor(wrapper).props('candidatesState')).toBe('ready')
  })

  it('marks candidatesState error when the upstream catalog request fails', async () => {
    syncUpstreamModelsPreviewMock.mockRejectedValue(new Error('boom'))
    const wrapper = mountModal()
    await openOpenCodeGoForm(wrapper)

    await findEditor(wrapper).vm.$emit('sync-targets')
    await flushPromises()

    expect(findEditor(wrapper).props('candidatesState')).toBe('error')
  })

  it('drops in-flight candidates when the platform context changes (no cross-context leak)', async () => {
    let resolveFetch: (value: unknown) => void = () => {}
    syncUpstreamModelsPreviewMock.mockReturnValue(
      new Promise((resolve) => {
        resolveFetch = resolve
      })
    )
    const wrapper = mountModal()
    await openOpenCodeGoForm(wrapper)

    await findEditor(wrapper).vm.$emit('sync-targets')
    await flushPromises()
    expect(findEditor(wrapper).props('candidatesState')).toBe('loading')

    await wrapper.get('[data-testid="platform-button-opencode"]').trigger('click')
    resolveFetch({ models: ['stale-model'], metadata: {} })
    await flushPromises()

    expect(findEditor(wrapper).props('candidates')).toEqual([])
    expect(findEditor(wrapper).props('candidatesState')).toBe('idle')
  })

  it('blocks the real form submit when a target-only draft exists next to a valid row', async () => {
    const wrapper = mountModalWithRealEditor()
    await openOpenCodeGoForm(wrapper)

    const editor = wrapper.findComponent(OpenCodeQuotaFallbackEditor)
    editor.vm.$emit('update:modelValue', {
      enabled: true,
      threshold_percent: 98,
      model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' },
    })
    await flushPromises()

    await wrapper.get('[data-testid="opencode-quota-fallback-add"]').trigger('click')
    await wrapper
      .get('[data-testid^="opencode-quota-fallback-target-draft-"] [data-testid="select-custom"]')
      .trigger('click')

    await submit(wrapper)

    expect(createAccountMock).not.toHaveBeenCalled()
    expect(showErrorMock).toHaveBeenCalledWith(
      'admin.accounts.openCodeQuotaFallback.errors.sourceRequired'
    )
    expect(wrapper.text()).toContain('admin.accounts.openCodeQuotaFallback.errors.sourceRequired')
    // 合法行仍在编辑器内，整个保存被阻断，而不是丢弃草稿后照常创建。
    expect(
      wrapper.find('[data-testid="opencode-quota-fallback-source-deepseek-v4.1-flash"]').exists()
    ).toBe(true)
  })

  it('allows switching protection off with a target-only draft and saves without it', async () => {
    const wrapper = mountModalWithRealEditor()
    await openOpenCodeGoForm(wrapper)

    const editor = wrapper.findComponent(OpenCodeQuotaFallbackEditor)
    editor.vm.$emit('update:modelValue', {
      enabled: true,
      threshold_percent: 98,
      model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' },
    })
    await flushPromises()

    await wrapper.get('[data-testid="opencode-quota-fallback-add"]').trigger('click')
    await wrapper
      .get('[data-testid^="opencode-quota-fallback-target-draft-"] [data-testid="select-custom"]')
      .trigger('click')
    // 关闭是显式回退草稿，不应强迫用户回头补完空行。
    await wrapper.get('[data-testid="opencode-quota-fallback-toggle"]').trigger('click')

    await submit(wrapper)

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(showErrorMock).not.toHaveBeenCalled()
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials?.quota_fallback).toEqual({
      enabled: false,
      threshold_percent: 98,
      model_mapping: { 'deepseek-v4.1-flash': 'step-5-preview' },
    })
  })

  it('commits the current source on Enter in the real form without creating an account', async () => {
    const wrapper = mountModalWithRealEditor()
    await openOpenCodeGoForm(wrapper)

    await wrapper.get('[data-testid="opencode-quota-fallback-toggle"]').trigger('click')
    await wrapper.get('[data-testid="opencode-quota-fallback-add"]').trigger('click')
    const input = wrapper.get<HTMLInputElement>(
      '[data-testid^="opencode-quota-fallback-source-draft-"]'
    )
    await input.setValue('typed-in-modal')

    const event = new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true })
    input.element.dispatchEvent(event)
    await nextTick()
    await flushPromises()

    expect(event.defaultPrevented).toBe(true)
    expect(createAccountMock).not.toHaveBeenCalled()
    expect(wrapper.emitted('created')).toBeUndefined()
    expect(wrapper.find('[data-testid="opencode-quota-fallback-source-typed-in-modal"]').exists()).toBe(true)
  })
})
