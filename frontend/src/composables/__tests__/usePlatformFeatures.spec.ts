import { describe, expect, it, vi, beforeEach } from 'vitest'
import {
  usePlatformFeatures,
  timeOfDayToHHMM,
  applyHHMMToTimeOfDay
} from '../usePlatformFeatures'

// 平台功能设置的状态逻辑（A4 批 4.0 前端侧）。
//
// 这些是 SettingsView 里真实调用的逻辑（已抽成 composable），不是替身实现——
// 测的是会被期望同样跑的代码。
//
// 关键契约：
//   1. 形状由服务端驱动（不写死任何平台）；
//   2. 保存用服务端**归一化后的返回值**回填，而不是沿用本地输入；
//   3. 加载/保存失败不抛穿，走 onError 回调；
//   4. 时段字符串 ↔ TimeOfDay 的解析要保守（非法输入不破坏已配置值）。

const { getPlatformFeaturesMock, updatePlatformFeaturesMock, runPlatformFeatureMock } = vi.hoisted(
  () => ({
    getPlatformFeaturesMock: vi.fn(),
    updatePlatformFeaturesMock: vi.fn(),
    runPlatformFeatureMock: vi.fn()
  })
)

vi.mock('@/api/admin', () => ({
  adminAPI: {
    settings: {
      getPlatformFeatures: getPlatformFeaturesMock,
      updatePlatformFeatures: updatePlatformFeaturesMock,
      runPlatformFeature: runPlatformFeatureMock
    }
  }
}))

const codeBuddyPayload = {
  platforms: [
    {
      platform: 'codebuddy',
      features: [
        {
          key: 'checkin',
          kind: 'time_range',
          title: '自动签到',
          description: '',
          value: { enabled: true, start: { hour: 9, minute: 0 }, end: { hour: 11, minute: 0 } }
        }
      ]
    }
  ]
}

beforeEach(() => {
  getPlatformFeaturesMock.mockReset()
  updatePlatformFeaturesMock.mockReset()
})

describe('usePlatformFeatures', () => {
  it('load 把服务端返回的平台分组原样装入（不写死平台）', async () => {
    getPlatformFeaturesMock.mockResolvedValue({
      platforms: [
        ...codeBuddyPayload.platforms,
        {
          platform: 'future-platform',
          features: [
            { key: 'warmup', kind: 'bool', title: '保活', description: '', value: { enabled: false } }
          ]
        }
      ]
    })

    const { groups, loading, load } = usePlatformFeatures()
    expect(loading.value).toBe(false)
    const promise = load()
    expect(loading.value).toBe(true, 'load 期间应处于 loading')
    await promise

    expect(loading.value).toBe(false)
    expect(groups.value.map((g) => g.platform)).toEqual(['codebuddy', 'future-platform'])
  })

  it('load 失败走 onError 回调且不抛穿，loading 复位', async () => {
    getPlatformFeaturesMock.mockRejectedValue(new Error('boom'))
    const onError = vi.fn()

    const { loading, load, groups } = usePlatformFeatures({ onError })
    await expect(load()).resolves.toBeUndefined()
    expect(onError).toHaveBeenCalledWith('loadFailed', 'loadFailed')
    expect(loading.value).toBe(false)
    expect(groups.value).toEqual([])
  })

  it('save 提交全部功能项，并用服务端归一化值回填', async () => {
    // 本地是零长窗口（10:00–10:00，服务端会判为无意义并退回默认）
    getPlatformFeaturesMock.mockResolvedValue({
      platforms: [
        {
          platform: 'codebuddy',
          features: [
            {
              key: 'checkin',
              kind: 'time_range',
              title: '自动签到',
              description: '',
              value: { enabled: true, start: { hour: 10, minute: 0 }, end: { hour: 10, minute: 0 } }
            }
          ]
        }
      ]
    })
    updatePlatformFeaturesMock.mockResolvedValue(codeBuddyPayload) // 服务端修正为 09:00–11:00

    const { groups, saved, save, load } = usePlatformFeatures()
    await load()
    await save()

    expect(updatePlatformFeaturesMock).toHaveBeenCalledTimes(1)
    expect(updatePlatformFeaturesMock.mock.calls[0][0]).toEqual([
      {
        platform: 'codebuddy',
        key: 'checkin',
        value: { enabled: true, start: { hour: 10, minute: 0 }, end: { hour: 10, minute: 0 } }
      }
    ])

    // 回填的是服务端值，不是本地提交的 10:00 —— 否则界面会显示未生效的时段。
    expect(groups.value[0].features[0].value.start).toEqual({ hour: 9, minute: 0 })
    expect(groups.value[0].features[0].value.end).toEqual({ hour: 11, minute: 0 })
    expect(saved.value).toBe(true)
  })

  it('save 失败走 onError 回调且不抛穿', async () => {
    getPlatformFeaturesMock.mockResolvedValue(codeBuddyPayload)
    updatePlatformFeaturesMock.mockRejectedValue(new Error('boom'))
    const onError = vi.fn()

    const { load, save, saving } = usePlatformFeatures({ onError })
    await load()
    await expect(save()).resolves.toBeUndefined()

    expect(onError).toHaveBeenCalledWith('saveFailed', 'saveFailed')
    expect(saving.value).toBe(false)
  })

  it('并发 save 只提交一次（防重复点击打两次上游）', async () => {
    getPlatformFeaturesMock.mockResolvedValue(codeBuddyPayload)
    let resolveUpdate: (value: unknown) => void = () => {}
    updatePlatformFeaturesMock.mockImplementation(
      () => new Promise((resolve) => (resolveUpdate = resolve))
    )

    const { load, save } = usePlatformFeatures()
    await load()

    const first = save()
    const second = save() // 应被 saving 门挡掉
    resolveUpdate(codeBuddyPayload)
    await Promise.all([first, second])

    expect(updatePlatformFeaturesMock).toHaveBeenCalledTimes(1)
  })

  it('空注册表是合法状态（不报错、groups 为空）', async () => {
    getPlatformFeaturesMock.mockResolvedValue({ platforms: [] })
    const onError = vi.fn()

    const { groups, load } = usePlatformFeatures({ onError })
    await load()

    expect(groups.value).toEqual([])
    expect(onError).not.toHaveBeenCalled()
  })
})

describe('timeOfDayToHHMM', () => {
  it('补零渲染', () => {
    expect(timeOfDayToHHMM({ hour: 9, minute: 0 })).toBe('09:00')
    expect(timeOfDayToHHMM({ hour: 23, minute: 59 })).toBe('23:59')
    expect(timeOfDayToHHMM({ hour: 0, minute: 5 })).toBe('00:05')
  })

  it('缺值返回空串（time 输入框的空态）', () => {
    expect(timeOfDayToHHMM(undefined)).toBe('')
  })
})

describe('applyHHMMToTimeOfDay', () => {
  it('合法 HH:MM 写入目标字段', () => {
    const target: { start?: { hour: number; minute: number }; end?: { hour: number; minute: number } } = {}
    expect(applyHHMMToTimeOfDay(target, 'start', '14:30')).toBe(true)
    expect(target.start).toEqual({ hour: 14, minute: 30 })
  })

  it('非法输入不修改目标（不清掉已配置的时段）', () => {
    const target = { start: { hour: 9, minute: 0 } }
    for (const bad of ['', 'abc', '24:00', '12:60', '9', '1:2:3', '09:0x', '   ', '::', '-1:00']) {
      expect(applyHHMMToTimeOfDay(target, 'start', bad)).toBe(false)
    }
    expect(target.start).toEqual({ hour: 9, minute: 0 })
  })
})

// --- 立即执行（设置页"立即执行"按钮的逻辑）---
//
// 这条能力解决：功能只有"开关+时段"时，唯一触发手段是等窗口；
// 窗口设得晚、或当天已错过（在窗口外才配置），按定时就只能等第二天。
// 用户原话：「设置的时间有时候太晚，会到第二天去」。

describe('usePlatformFeatures 立即执行', () => {
  // ⚠️ 必须在这里单独 reset：文件顶部的 beforeEach 只重置它那两个 mock，
  // 不管 runPlatformFeature —— 漏掉会让调用计数在用例间累积
  // （实测表现：断言"只调一次"却得到 4 次）。
  beforeEach(() => {
    runPlatformFeatureMock.mockReset()
  })

  it('调用服务端并把 platform/key 原样传出（映射不在前端）', async () => {
    runPlatformFeatureMock.mockResolvedValue({ summary: '签到完成（共 3 个账号）：成功 3' })
    const f = usePlatformFeatures()

    await f.runNow('codebuddy', 'checkin')

    expect(runPlatformFeatureMock).toHaveBeenCalledWith('codebuddy', 'checkin')
  })

  it('结果按 platform:key 保留（界面可长期展示，不随 toast 消失）', async () => {
    runPlatformFeatureMock.mockResolvedValue({ summary: '成功 3' })
    const f = usePlatformFeatures()

    await f.runNow('codebuddy', 'checkin')

    expect(f.runResults.value['codebuddy:checkin']).toBe('成功 3')
  })

  it('执行完成后 runningId 复位（按钮可再次点击）', async () => {
    runPlatformFeatureMock.mockResolvedValue({ summary: 'ok' })
    const f = usePlatformFeatures()

    await f.runNow('codebuddy', 'checkin')

    expect(f.runningId.value).toBe('')
  })

  it('并发点击只发一次请求（这些动作都会打上游，并发会放大风控暴露）', async () => {
    // 关键：第一个请求**悬而不决**（模拟上游慢），期间发第二次点击。
    // 注意不能 `await` 第一次调用——那会等到解决后才执行第二句，
    // 就不是"并发"了，测不到串行化。
    let resolveFn: ((v: { summary: string }) => void) | undefined
    runPlatformFeatureMock.mockImplementation(
      () => new Promise((resolve) => { resolveFn = resolve })
    )
    const f = usePlatformFeatures()

    const first = f.runNow('codebuddy', 'checkin') // 不 await
    // 此刻 runningId 已是 checkin，第二次点击应被挡掉。
    const second = f.runNow('codebuddy', 'growth')

    expect(runPlatformFeatureMock).toHaveBeenCalledTimes(1)
    expect(runPlatformFeatureMock).toHaveBeenCalledWith('codebuddy', 'checkin')
    expect(f.runningId.value).toBe('codebuddy:checkin')

    resolveFn?.({ summary: 'ok' })
    await Promise.all([first, second])
    expect(f.runningId.value).toBe('')
  })

  it('失败走 onRunError 回调且不抛穿，runningId 仍复位', async () => {
    runPlatformFeatureMock.mockRejectedValue(new Error('boom'))
    const onRunError = vi.fn()
    const f = usePlatformFeatures({ onRunError })

    await expect(f.runNow('codebuddy', 'checkin')).resolves.toBeUndefined()

    expect(onRunError).toHaveBeenCalledWith('codebuddy', 'checkin', expect.any(Error))
    expect(f.runningId.value).toBe('')
  })

  it('成功走 onRunSuccess 回调并带上服务端文案', async () => {
    runPlatformFeatureMock.mockResolvedValue({ summary: '已跑一轮' })
    const onRunSuccess = vi.fn()
    const f = usePlatformFeatures({ onRunSuccess })

    await f.runNow('codebuddy', 'growth')

    expect(onRunSuccess).toHaveBeenCalledWith('已跑一轮', 'codebuddy', 'growth')
  })
})
