import { beforeEach, describe, expect, it, vi } from 'vitest'

const { showError } = vi.hoisted(() => ({ showError: vi.fn() }))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError,
  }),
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => {
      const messages: Record<string, string> = {
        'common.unknownError': 'Unknown error occurred',
      }
      return messages[key] ?? key
    },
  }),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    zhipu: {
      generateLoginUrl: vi.fn(),
      exchange: vi.fn(),
      relogin: vi.fn(),
      getResetCardStatus: vi.fn(),
    },
  },
}))

import { useZhipuOAuth } from '@/composables/useZhipuOAuth'
import { adminAPI } from '@/api/admin'

const LOGIN_URL_RESPONSE = {
  login_url: 'https://bigmodel.cn/login?appId=zcode&state=state-1',
  session_id: 'session-1',
  state: 'state-1',
}

const LOGIN_CREDENTIAL = {
  api_key: 'key-id.key-secret',
  access_token: 'access-token',
  zcodejwttoken: 'zcode-jwt-token',
  plan_level: 'coding',
}

describe('useZhipuOAuth login flow', () => {
  beforeEach(() => {
    vi.mocked(adminAPI.zhipu.generateLoginUrl).mockReset()
    vi.mocked(adminAPI.zhipu.exchange).mockReset()
    vi.mocked(adminAPI.zhipu.relogin).mockReset()
    showError.mockReset()
  })

  it('walks idle → generating → awaiting_code → exchanging → ready', async () => {
    const oauth = useZhipuOAuth()
    let statusWhileGenerating: string = ''
    let statusWhileExchanging: string = ''

    vi.mocked(adminAPI.zhipu.generateLoginUrl).mockImplementationOnce(async () => {
      statusWhileGenerating = oauth.status.value
      return LOGIN_URL_RESPONSE
    })
    vi.mocked(adminAPI.zhipu.exchange).mockImplementationOnce(async () => {
      statusWhileExchanging = oauth.status.value
      return LOGIN_CREDENTIAL
    })

    expect(oauth.status.value).toBe('idle')

    const generated = await oauth.generateLoginUrl({ proxyId: 7 })
    expect(generated).toBe(true)
    expect(statusWhileGenerating).toBe('generating')
    expect(adminAPI.zhipu.generateLoginUrl).toHaveBeenCalledWith({ proxy_id: 7 })
    expect(oauth.status.value).toBe('awaiting_code')
    expect(oauth.loginUrl.value).toBe(LOGIN_URL_RESPONSE.login_url)
    expect(oauth.sessionId.value).toBe('session-1')
    expect(oauth.state.value).toBe('state-1')
    expect(oauth.errorMessage.value).toBe('')

    const credential = await oauth.exchangeAuthCode({ code: '  auth-code-1  ' })
    expect(statusWhileExchanging).toBe('exchanging')
    expect(adminAPI.zhipu.exchange).toHaveBeenCalledWith({
      session_id: 'session-1',
      state: 'state-1',
      auth_code: 'auth-code-1',
    })
    expect(credential).toEqual(LOGIN_CREDENTIAL)
    expect(oauth.credential.value).toEqual(LOGIN_CREDENTIAL)
    expect(oauth.status.value).toBe('ready')
    expect(oauth.errorMessage.value).toBe('')
  })

  it('reports a generate failure as error and recovers on the next attempt', async () => {
    const oauth = useZhipuOAuth()
    vi.mocked(adminAPI.zhipu.generateLoginUrl).mockRejectedValueOnce({
      status: 502,
      message: 'upstream risk control rejected the request',
    })

    const generated = await oauth.generateLoginUrl()

    expect(generated).toBe(false)
    expect(oauth.status.value).toBe('error')
    expect(oauth.errorMessage.value).toBe('upstream risk control rejected the request')
    expect(oauth.loginUrl.value).toBe('')
    expect(showError).toHaveBeenCalledWith('upstream risk control rejected the request')

    vi.mocked(adminAPI.zhipu.generateLoginUrl).mockResolvedValueOnce(LOGIN_URL_RESPONSE)

    await expect(oauth.generateLoginUrl()).resolves.toBe(true)
    expect(oauth.status.value).toBe('awaiting_code')
    expect(oauth.errorMessage.value).toBe('')
    expect(oauth.loginUrl.value).toBe(LOGIN_URL_RESPONSE.login_url)
  })

  it('surfaces an expired-session exchange failure as error with no credential', async () => {
    const oauth = useZhipuOAuth()
    vi.mocked(adminAPI.zhipu.generateLoginUrl).mockResolvedValueOnce(LOGIN_URL_RESPONSE)
    await oauth.generateLoginUrl()
    vi.mocked(adminAPI.zhipu.exchange).mockRejectedValueOnce({
      status: 400,
      reason: 'ZHIPU_OAUTH_SESSION_NOT_FOUND',
      message: 'Zhipu OAuth session not found or expired. Generate a new login URL.',
    })

    const credential = await oauth.exchangeAuthCode({ code: 'auth-code-1' })

    expect(credential).toBeNull()
    expect(oauth.credential.value).toBeNull()
    expect(oauth.status.value).toBe('error')
    expect(oauth.errorMessage.value).toBe(
      'Zhipu OAuth session not found or expired. Generate a new login URL.'
    )
    expect(showError).toHaveBeenCalledWith(
      'Zhipu OAuth session not found or expired. Generate a new login URL.'
    )
  })

  it('relogins an existing account with the current session and reports failure states', async () => {
    const oauth = useZhipuOAuth()
    vi.mocked(adminAPI.zhipu.generateLoginUrl).mockResolvedValueOnce(LOGIN_URL_RESPONSE)
    await oauth.generateLoginUrl()
    let statusWhileRelogining = ''
    vi.mocked(adminAPI.zhipu.relogin).mockImplementationOnce(async () => {
      statusWhileRelogining = oauth.status.value
    })

    const relogined = await oauth.relogin(42, { code: '  auth-code-1  ' })

    expect(statusWhileRelogining).toBe('exchanging')
    expect(adminAPI.zhipu.relogin).toHaveBeenCalledWith(42, {
      session_id: 'session-1',
      state: 'state-1',
      auth_code: 'auth-code-1',
    })
    expect(relogined).toBe(true)
    expect(oauth.status.value).toBe('ready')
    expect(oauth.errorMessage.value).toBe('')

    vi.mocked(adminAPI.zhipu.relogin).mockRejectedValueOnce({
      status: 400,
      reason: 'ZHIPU_OAUTH_INVALID_STATE',
      message: 'Zhipu OAuth state does not match the current session.',
    })

    const failed = await oauth.relogin(42, { code: 'auth-code-2' })

    expect(failed).toBe(false)
    expect(oauth.status.value).toBe('error')
    expect(oauth.errorMessage.value).toBe(
      'Zhipu OAuth state does not match the current session.'
    )
  })

  it('resets repeatedly back to idle and starts the next flow clean', async () => {
    const oauth = useZhipuOAuth()
    vi.mocked(adminAPI.zhipu.generateLoginUrl).mockResolvedValueOnce(LOGIN_URL_RESPONSE)
    vi.mocked(adminAPI.zhipu.exchange).mockResolvedValueOnce(LOGIN_CREDENTIAL)
    await oauth.generateLoginUrl()
    await oauth.exchangeAuthCode({ code: 'auth-code-1' })
    expect(oauth.status.value).toBe('ready')

    oauth.reset()
    oauth.reset()

    expect(oauth.status.value).toBe('idle')
    expect(oauth.loginUrl.value).toBe('')
    expect(oauth.sessionId.value).toBe('')
    expect(oauth.state.value).toBe('')
    expect(oauth.credential.value).toBeNull()
    expect(oauth.errorMessage.value).toBe('')

    vi.mocked(adminAPI.zhipu.generateLoginUrl).mockRejectedValueOnce({
      status: 500,
      message: 'boom',
    })
    await oauth.generateLoginUrl()
    expect(oauth.status.value).toBe('error')

    oauth.reset()

    expect(oauth.status.value).toBe('idle')
    expect(oauth.errorMessage.value).toBe('')

    const secondResponse = {
      login_url: 'https://bigmodel.cn/login?appId=zcode&state=state-2',
      session_id: 'session-2',
      state: 'state-2',
    }
    vi.mocked(adminAPI.zhipu.generateLoginUrl).mockResolvedValueOnce(secondResponse)
    await expect(oauth.generateLoginUrl()).resolves.toBe(true)

    expect(oauth.status.value).toBe('awaiting_code')
    expect(oauth.sessionId.value).toBe('session-2')
    expect(oauth.state.value).toBe('state-2')
  })
})

describe('useZhipuOAuth credential handling', () => {
  const dumpConsoleCalls = (calls: unknown[][]): string =>
    calls
      .flat()
      .map((value) => {
        try {
          return JSON.stringify(value) ?? String(value)
        } catch {
          return String(value)
        }
      })
      .join('\n')

  it('never writes api_key / access_token / zcodejwttoken to console or to errorMessage', async () => {
    const consoleSpies = (['log', 'info', 'warn', 'error', 'debug'] as const).map((method) =>
      vi.spyOn(console, method).mockImplementation(() => {})
    )

    try {
      const oauth = useZhipuOAuth()
      vi.mocked(adminAPI.zhipu.generateLoginUrl).mockResolvedValueOnce(LOGIN_URL_RESPONSE)
      vi.mocked(adminAPI.zhipu.exchange).mockResolvedValueOnce(LOGIN_CREDENTIAL)

      await oauth.generateLoginUrl()
      await oauth.exchangeAuthCode({ code: 'auth-code-1' })
      expect(oauth.credential.value).toEqual(LOGIN_CREDENTIAL)

      vi.mocked(adminAPI.zhipu.exchange).mockRejectedValueOnce({
        status: 400,
        reason: 'ZHIPU_OAUTH_INVALID_STATE',
        message: 'Zhipu OAuth state does not match the current session.',
      })
      await oauth.exchangeAuthCode({ code: 'auth-code-2' })

      const consoleOutput = consoleSpies
        .map((spy) => dumpConsoleCalls(spy.mock.calls))
        .join('\n')
      for (const secret of [
        LOGIN_CREDENTIAL.api_key,
        LOGIN_CREDENTIAL.access_token,
        LOGIN_CREDENTIAL.zcodejwttoken,
      ]) {
        expect(consoleOutput).not.toContain(secret)
        expect(oauth.errorMessage.value).not.toContain(secret)
      }
    } finally {
      consoleSpies.forEach((spy) => spy.mockRestore())
    }
  })
})
