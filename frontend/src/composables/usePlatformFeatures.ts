import { ref, type Ref } from 'vue'
import { adminAPI } from '@/api/admin'
import type { PlatformFeatureGroup } from '@/api/admin/settings'

/**
 * 平台功能设置的状态逻辑（A4 批 4.0）。
 *
 * 抽成 composable 而不是写在 SettingsView 里：SettingsView 是上万行的巨型 SFC，
 * 内嵌的逻辑无法单独测试，只能靠"重新实现一遍再测"——那种测试测的是替身而不是
 * 真实代码，是自欺（本项目已有教训：抄一份清单挡不住漂移）。
 * 抽出来之后这里的每条契约都是可执行断言。
 *
 * 数据形状完全由服务端注册表驱动：各平台注册功能项后这里自动出现，
 * 前端不为任何具体平台/功能写死。
 */
export function usePlatformFeatures(options?: {
  /** 错误提示回调（默认静默，由调用方注入 appStore.showError）。 */
  onError?: (message: string, key: 'loadFailed' | 'saveFailed') => void
  /** 立即执行成功回调（默认静默，由调用方注入 appStore.showSuccess）。 */
  onRunSuccess?: (summary: string, platform: string, key: string) => void
  /** 立即执行失败回调（默认静默）。 */
  onRunError?: (platform: string, key: string, error: unknown) => void
}) {
  const groups: Ref<PlatformFeatureGroup[]> = ref([])
  const loading = ref(false)
  const saving = ref(false)
  const saved = ref(false)
  let savedTimer: ReturnType<typeof setTimeout> | undefined

  function reportError(key: 'loadFailed' | 'saveFailed', error: unknown): void {
    console.error(`platform features ${key}:`, error)
    options?.onError?.(key, key)
  }

  async function load(): Promise<void> {
    loading.value = true
    try {
      const data = await adminAPI.settings.getPlatformFeatures()
      groups.value = data.platforms ?? []
    } catch (error) {
      reportError('loadFailed', error)
    } finally {
      loading.value = false
    }
  }

  /**
   * 保存：把当前全部功能项提交（接口按 platform+key 稀疏覆盖，未提交项服务端保持原值）。
   *
   * 用**服务端返回值**回填而不是沿用本地输入：服务端会做归一化
   * （越界时间点退回默认、零长窗口修正），回填本地值会让界面显示一个
   * 并未真正生效的时间段。
   */
  async function save(): Promise<void> {
    if (saving.value) return
    saving.value = true
    saved.value = false
    try {
      const features = groups.value.flatMap((group) =>
        group.features.map((feature) => ({
          platform: group.platform,
          key: feature.key,
          value: feature.value
        }))
      )
      const data = await adminAPI.settings.updatePlatformFeatures(features)
      groups.value = data.platforms ?? []
      saved.value = true
      if (savedTimer !== undefined) clearTimeout(savedTimer)
      savedTimer = setTimeout(() => {
        saved.value = false
      }, 3000)
    } catch (error) {
      reportError('saveFailed', error)
    } finally {
      saving.value = false
    }
  }

  // --- 立即执行（设置页"立即执行"按钮）---
  //
  // 放在 composable 而不是 SettingsView 里，理由与上面「抽成 composable」那段相同：
  // SettingsView 是上万行的巨型 SFC，内嵌逻辑无法单独测试。
  //
  // 语义上它与"配置读写"不同层：立即执行是**一次性动作**，既不读也不写配置
  // （服务端同样不读开关、不读窗口）。但两者共享同一份"功能清单"状态，
  // 放在一起才能让按钮知道该渲染在哪条 feature 上。

  /** 正在执行的功能（`platform:key`）；空串表示空闲。 */
  const runningId = ref('')

  /**
   * 各功能最近一次执行结果（`platform:key` → 服务端生成的一句话）。
   *
   * 保留在界面上而不是 toast 后消失：这类动作的结果有信息量
   * （成功几个 / 跳过几个 / 失败几个），一闪而过的 toast 会逼用户反复点。
   */
  const runResults: Ref<Record<string, string>> = ref({})

  /**
   * 立即执行某功能。
   *
   * 为什么需要它：功能只有"开关 + 时段"时，唯一触发手段是**等窗口**。
   * 窗口设得晚、或当天已经错过（在窗口外才配置），按定时就只能等第二天
   * ——用户原话「设置的时间有时候太晚，会到第二天去」。
   * 本方法把那次执行提前到现在。
   *
   * 不读开关也不读窗口（服务端同口径）：用户点它就是"现在要跑"，
   * 被自动排程的开关或时段挡住会让人点了没反应且找不到原因。
   */
  async function runNow(platform: string, key: string): Promise<void> {
    const id = `${platform}:${key}`
    // 串行化：同一时间只允许一个执行请求（这些动作都会打上游，并发点会放大风控暴露）。
    if (runningId.value !== '') return
    runningId.value = id
    try {
      const result = await adminAPI.settings.runPlatformFeature(platform, key)
      const summary = result?.summary ?? ''
      runResults.value = { ...runResults.value, [id]: summary }
      options?.onRunSuccess?.(summary, platform, key)
    } catch (error) {
      console.error(`platform feature run ${platform}/${key}:`, error)
      options?.onRunError?.(platform, key, error)
    } finally {
      runningId.value = ''
    }
  }

  return {
    groups,
    loading,
    saving,
    saved,
    load,
    save,
    runningId,
    runResults,
    runNow,
  }
}

/** TimeOfDay → "HH:MM"（time 输入框取值）。 */
export function timeOfDayToHHMM(value?: { hour: number; minute: number }): string {
  if (!value) return ''
  const two = (n: number) => String(n).padStart(2, '0')
  return `${two(value.hour)}:${two(value.minute)}`
}

/**
 * "HH:MM" → TimeOfDay，写回目标字段；非法输入返回 false 且**不修改目标**
 * （避免把已配置的时段清成 00:00 或半截值）。
 *
 * 解析必须严格——`<input type="time">` 通常给规范值，但它也接受部分输入的
 * 中间态与自由文本：
 *   - 必须恰好两段（`"1:2:3"` 若只取前两段会被静默读成 01:02）；
 *   - 每段必须纯数字（`Number.parseInt("9abc")` 会得到 9，不能用它来校验）；
 *   - 小时 0–23、分钟 0–59。
 */
export function applyHHMMToTimeOfDay(
  target: { start?: { hour: number; minute: number }; end?: { hour: number; minute: number } },
  field: 'start' | 'end',
  raw: string
): boolean {
  const parts = raw.trim().split(':')
  if (parts.length !== 2) return false
  const parsed = parts.map(parseStrictInt)
  if (parsed.some((value) => value === null)) return false
  const [hour, minute] = parsed as [number, number]
  if (hour < 0 || hour > 23 || minute < 0 || minute > 59) return false
  target[field] = { hour, minute }
  return true
}

/** 纯十进制整数解析；含非数字字符返回 null（不静默截断）。 */
function parseStrictInt(raw: string): number | null {
  const trimmed = raw.trim()
  if (trimmed === '' || !/^\d+$/.test(trimmed)) return null
  return Number.parseInt(trimmed, 10)
}
