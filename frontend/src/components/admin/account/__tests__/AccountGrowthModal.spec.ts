import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountGrowthModal from '../AccountGrowthModal.vue'
import type { CodeBuddyGrowthChannel } from '@/api/admin/codebuddy'

const {
  growthChannelsMock,
  growthRunChannelMock,
  growthRunAllMock,
  growthActivityRunMock,
  showSuccessMock,
  showErrorMock,
} = vi.hoisted(() => ({
  growthChannelsMock: vi.fn(),
  growthRunChannelMock: vi.fn(),
  growthRunAllMock: vi.fn(),
  growthActivityRunMock: vi.fn(),
  showSuccessMock: vi.fn(),
  showErrorMock: vi.fn(),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    codebuddy: {
      growthChannels: growthChannelsMock,
      growthRunChannel: growthRunChannelMock,
      growthRunAll: growthRunAllMock,
      growthActivityRun: growthActivityRunMock,
    },
  },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showSuccess: showSuccessMock, showError: showErrorMock }),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      // 直接回 key，断言时看 key 即可（不引入真实 locale 文件）。
      t: (key: string) => key,
    }),
  }
})

/** 构造一条通道（默认：已授权的 full 通道）。 */
function makeChannel(overrides: Partial<CodeBuddyGrowthChannel> = {}): CodeBuddyGrowthChannel {
  return {
    key: 'adopt',
    tier: 'full',
    auto_runnable: true,
    auto_authorized: true,
    auto_authorization: '用户 2026-09-29 裁定',
    rationale: '前置 chat_5 只能靠伪造活跃上报满足',
    ...overrides,
  }
}

function mountModal(channels: CodeBuddyGrowthChannel[] = [makeChannel()]) {
  growthChannelsMock.mockResolvedValue({ channels })
  return mount(AccountGrowthModal, {
    props: { show: true, accountId: 11 },
    global: {
      stubs: {
        // 桩掉 BaseDialog 的 teleport/过渡，只渲染内容便于断言。
        BaseDialog: { template: '<div><slot /></div>' },
        Icon: true,
      },
    },
  })
}

describe('AccountGrowthModal', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  it('列出通道并逐条呈现分级与授权状态', async () => {
    const wrapper = mountModal([
      makeChannel({ key: 'adopt', tier: 'full', auto_authorized: true }),
      makeChannel({
        key: 'streak',
        tier: 'claim',
        auto_authorized: false,
        auto_authorization: undefined,
      }),
      makeChannel({
        key: 'lottery',
        tier: 'full',
        auto_runnable: false,
        auto_authorized: false,
        auto_authorization: undefined,
      }),
    ])
    await flushPromises()

    expect(growthChannelsMock).toHaveBeenCalledTimes(1)
    const text = wrapper.text()
    // 三个通道键都应出现。
    expect(text).toContain('adopt')
    expect(text).toContain('streak')
    expect(text).toContain('lottery')

    // 已授权的 full 通道：标"已授权自动"，并显示授权依据。
    expect(wrapper.find('[data-testid="growth-auto-authorized"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="growth-authorization"]').exists()).toBe(true)

    // 未授权的 full 通道（lottery）：标"仅手动"，且**不得**出现授权徽标。
    expect(wrapper.find('[data-testid="growth-manual-only"]').exists()).toBe(true)
    // 全表只有 streak 一条是"性质上可自动"。
    expect(wrapper.findAll('[data-testid="growth-auto-runnable"]').length).toBe(1)
  })

  it('授权依据逐通道显示（可追溯，不靠记忆）', async () => {
    const wrapper = mountModal([
      makeChannel({ key: 'adopt', auto_authorization: '用户 2026-09-29 裁定：开领养' }),
    ])
    await flushPromises()

    expect(wrapper.text()).toContain('用户 2026-09-29 裁定：开领养')
  })

  it('单通道执行：调用 API 并带账号 id 与通道键', async () => {
    growthRunChannelMock.mockResolvedValue({ channel: 'adopt', tier: 'full', auto_runnable: true })
    const wrapper = mountModal()
    await flushPromises()

    await wrapper.find('[data-testid="growth-run-adopt"]').trigger('click')
    await flushPromises()

    expect(growthRunChannelMock).toHaveBeenCalledWith(11, 'adopt')
    expect(showSuccessMock).toHaveBeenCalled()
  })

  it('单通道失败：把错误原因显示出来（不静默当成功）', async () => {
    growthRunChannelMock.mockResolvedValue({
      channel: 'adopt',
      tier: 'full',
      auto_runnable: true,
      error: '未达 chat_5 门槛',
    })
    const wrapper = mountModal()
    await flushPromises()

    await wrapper.find('[data-testid="growth-run-adopt"]').trigger('click')
    await flushPromises()

    // 后端把"业务上没做成"写在 error 字段里（HTTP 200）——必须提示失败。
    expect(showErrorMock).toHaveBeenCalled()
    expect(showSuccessMock).not.toHaveBeenCalled()
  })

  it('跑一轮：汇总成功/跳过/失败计数', async () => {
    growthRunAllMock.mockResolvedValue({ attempted: 3, succeeded: 2, skipped: 1, failed: 0 })
    const wrapper = mountModal()
    await flushPromises()

    await wrapper.find('[data-testid="growth-run-all"]').trigger('click')
    await flushPromises()

    expect(growthRunAllMock).toHaveBeenCalledTimes(1)
    expect(showSuccessMock).toHaveBeenCalled()
  })

  it('触发活跃上报：展示上报计数', async () => {
    growthActivityRunMock.mockResolvedValue({
      attempted: 3,
      reported: 2,
      skipped: 1,
      failed: 0,
      self_check_suspicious: 0,
    })
    const wrapper = mountModal()
    await flushPromises()

    await wrapper.find('[data-testid="growth-activity-run"]').trigger('click')
    await flushPromises()

    expect(growthActivityRunMock).toHaveBeenCalledTimes(1)
    expect(showSuccessMock).toHaveBeenCalled()
  })

  it('加载失败：提示错误而不是显示空列表当成功', async () => {
    growthChannelsMock.mockRejectedValue(new Error('boom'))
    const wrapper = mount(AccountGrowthModal, {
      props: { show: true, accountId: 11 },
      global: {
        stubs: { BaseDialog: { template: '<div><slot /></div>' }, Icon: true },
      },
    })
    await flushPromises()

    expect(showErrorMock).toHaveBeenCalled()
  })
})
