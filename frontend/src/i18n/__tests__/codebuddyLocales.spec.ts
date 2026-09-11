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
})
