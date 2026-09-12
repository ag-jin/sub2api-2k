import { describe, expect, it } from 'vitest'

import en from '../locales/en'
import zh from '../locales/zh'

describe('CodeBuddy account locale copy', () => {
  it('exposes the credential acquisition guide in zh and en', () => {
    for (const locale of [zh, en]) {
      const copy = locale.admin.accounts.codebuddy
      expect(copy.credentialGuide.summary).toBeTruthy()
      expect(copy.credentialGuide.intro).toBeTruthy()
      expect(copy.credentialGuide.macos).toContain('CodeBuddyExtension/Data/Public/auth')
      expect(copy.credentialGuide.windows).toContain('CodeBuddyExtension\\Data\\Public\\auth')
      expect(copy.credentialGuide.linux).toContain('CodeBuddyExtension/Data/Public/auth')
      expect(copy.credentialGuide.openAndPaste).toBeTruthy()
    }
  })

  it('keeps the desktop sign-out paste tip in zh and en', () => {
    expect(zh.admin.accounts.codebuddy.pasteTip).toContain('凭证互踢')
    expect(en.admin.accounts.codebuddy.pasteTip).toContain('kick each other')
  })

  it('exposes edit-state credential copy in zh and en', () => {
    for (const locale of [zh, en]) {
      const copy = locale.admin.accounts.codebuddy
      expect(copy.credentialsConfigured).toBeTruthy()
      expect(copy.credentialsRequired).toContain('auth.accessToken')
      expect(copy.authJsonEditPlaceholder).toBeTruthy()
    }
  })

  it('exposes the QR binding copy in zh and en', () => {
    for (const locale of [zh, en]) {
      const qr = locale.admin.accounts.codebuddy.qr
      expect(qr.tabLabel).toBeTruthy()
      expect(qr.startButton).toBeTruthy()
      expect(qr.waiting).toBeTruthy()
      expect(qr.expired).toBeTruthy()
      expect(qr.pollError).toBeTruthy()
      expect(qr.retry).toBeTruthy()
    }
    // success 携带 uid 插值占位
    expect(zh.admin.accounts.codebuddy.qr.success).toContain('{uid}')
    expect(en.admin.accounts.codebuddy.qr.success).toContain('{uid}')
  })

  it('exposes the CodeBuddy balance label in zh and en', () => {
    expect(zh.admin.accounts.codebuddy.usage.balanceLabel).toBeTruthy()
    expect(en.admin.accounts.codebuddy.usage.balanceLabel).toBeTruthy()
  })

  it('exposes the CodeBuddy credits value copy in zh and en with the value placeholder', () => {
    // 积分形态（unit=credits），不套货币格式（$）
    expect(zh.admin.accounts.codebuddy.usage.creditsValue).toBe('{value} 积分')
    expect(en.admin.accounts.codebuddy.usage.creditsValue).toBe('{value} credits')
    expect(String(zh.admin.accounts.codebuddy.usage.creditsValue)).not.toContain('$')
    expect(String(en.admin.accounts.codebuddy.usage.creditsValue)).not.toContain('$')
  })

  it('exposes the daily check-in copy in zh and en', () => {
    for (const locale of [zh, en]) {
      const checkin = locale.admin.accounts.codebuddy.checkin
      expect(checkin.action).toBeTruthy()
      expect(checkin.success).toContain('{credit}')
      expect(checkin.already).toBeTruthy()
      expect(checkin.streak).toContain('{days}')
      expect(checkin.failed).toBeTruthy()
    }
  })
})
