import { describe, expect, it } from 'vitest'

import en from '../locales/en'
import zh from '../locales/zh'
import { CONCRETE_PLATFORM_OPTIONS } from '@/constants/platforms'

/**
 * 侧边栏「账号管理」的平台子项文案取自 `admin.accounts.platforms.<platform>`。
 * 这是动态拼出来的 key，静态 key 扫描器（localeKeyCompleteness）看不到，
 * 新增平台时很容易只加目录项、漏掉文案 —— 那时子项会退化成显示原始 key。
 * 这里把"目录里的每个平台都必须有中英文案"固化为失败用例。
 */
describe('accounts platform navigation locale copy', () => {
  it('labels every catalog platform in both locales', () => {
    for (const [locale, messages] of Object.entries({ en, zh })) {
      for (const option of CONCRETE_PLATFORM_OPTIONS) {
        const label = messages.admin.accounts.platforms[option.value]
        expect(label, `${locale} is missing admin.accounts.platforms.${option.value}`).toBeTruthy()
      }
    }
  })

  it('keeps zh platform labels free of untranslated catalog values', () => {
    // 目录里的 label 是英文兜底；zh 缺文案时子项会退回这些英文串，这里挡住这种退化。
    const zhLabels = CONCRETE_PLATFORM_OPTIONS.map(option => zh.admin.accounts.platforms[option.value])
    expect(zhLabels).not.toContain(undefined)
    for (const label of zhLabels) {
      expect(String(label).trim()).not.toBe('')
    }
  })
})
