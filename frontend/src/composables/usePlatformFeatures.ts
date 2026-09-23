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

  return { groups, loading, saving, saved, load, save }
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
