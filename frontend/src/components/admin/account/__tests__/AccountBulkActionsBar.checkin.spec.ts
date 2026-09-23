import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AccountBulkActionsBar from '../AccountBulkActionsBar.vue'

// 批量签到按钮（A4 批 4.3 的前端入口）。
//
// 关键契约：这是个**全量**动作——候选账号由服务端按平台 + 可调度性决定，
// 与"当前选中了哪些账号"无关。所以按钮必须在**没有选中任何账号**时也可见，
// 否则管理员得先随便勾一个账号才能签到，语义是错的。
// 这条断言正是防"顺手把它塞进 selectedIds>0 分支"的回归。

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

const baseProps = {
  selectedIds: [] as number[],
  totalResults: 0,
  selectingAll: false,
  allResultsSelected: false
}

function mountBar(props: Record<string, unknown> = {}) {
  return mount(AccountBulkActionsBar, { props: { ...baseProps, ...props } })
}

describe('AccountBulkActionsBar — 批量签到 CodeBuddy', () => {
  it('未选中任何账号时按钮仍可见（全量动作，与选择无关）', () => {
    const wrapper = mountBar({ selectedIds: [] })
    const button = wrapper.find('[data-test="codebuddy-checkin"]')
    expect(button.exists()).toBe(true)
    expect(button.text()).toContain('admin.accounts.bulkActions.checkinCodeBuddy')
  })

  it('点击时 emit checkin-codebuddy', async () => {
    const wrapper = mountBar()
    await wrapper.find('[data-test="codebuddy-checkin"]').trigger('click')
    expect(wrapper.emitted('checkin-codebuddy')).toHaveLength(1)
  })

  it('运行中禁用并显示"签到中…"，防重复提交', async () => {
    const wrapper = mountBar({ checkinRunning: true })
    const button = wrapper.find('[data-test="codebuddy-checkin"]')
    expect(button.attributes('disabled')).toBeDefined()
    expect(button.text()).toContain('admin.accounts.bulkActions.checkinRunning')
  })

  it('运行中点击不 emit（禁用态不重复触发）', async () => {
    const wrapper = mountBar({ checkinRunning: true })
    await wrapper.find('[data-test="codebuddy-checkin"]').trigger('click')
    expect(wrapper.emitted('checkin-codebuddy')).toBeUndefined()
  })
})
