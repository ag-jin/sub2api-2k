import { beforeEach, describe, expect, it, vi } from 'vitest'

const { get, post } = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
}))

vi.mock('@/api/client', () => ({
  apiClient: { get, post },
}))

import zhipuAPI, {
  createFromLogin,
  exchange,
  generateLoginUrl,
  getResetCardStatus,
  relogin,
} from '@/api/admin/zhipu'
import type { CreateAccountRequest } from '@/types'

describe('admin Zhipu OAuth API', () => {
  beforeEach(() => {
    get.mockReset()
    post.mockReset()
  })

  it('posts the login-url request to the frozen admin path with 04 field names', async () => {
    const response = {
      login_url: 'https://bigmodel.cn/login?appId=zcode&state=state-1',
      session_id: 'session-1',
      state: 'state-1',
    }
    post.mockResolvedValueOnce({ data: response })

    const result = await generateLoginUrl({
      proxy_id: 7,
      redirect_uri: 'http://localhost:8085/callback',
    })

    expect(post).toHaveBeenCalledWith('/admin/zhipu/oauth/login-url', {
      proxy_id: 7,
      redirect_uri: 'http://localhost:8085/callback',
    })
    expect(result).toEqual(response)
  })

  it('sends the exchange body as session_id / state / auth_code (04 contract)', async () => {
    const credential = {
      api_key: 'zcode-api-key-id.secret',
      access_token: 'access-token',
      zcodejwttoken: 'zcode-jwt-token',
      refresh_token: 'refresh-token',
      plan_level: 'coding',
    }
    post.mockResolvedValueOnce({ data: credential })

    const result = await exchange({
      session_id: 'session-1',
      state: 'state-1',
      auth_code: 'auth-code-1',
    })

    expect(post).toHaveBeenCalledWith('/admin/zhipu/oauth/exchange', {
      session_id: 'session-1',
      state: 'state-1',
      auth_code: 'auth-code-1',
    })
    expect(result).toEqual(credential)
  })

  it('creates an account from login through the frozen admin path', async () => {
    const payload: CreateAccountRequest = {
      name: 'zhipu-login',
      platform: 'zhipu',
      type: 'apikey',
      credentials: {
        auth_flow: 'bigmodel_oauth',
        api_key: 'key-id.key-secret',
        access_token: 'access-token',
        zcodejwttoken: 'zcode-jwt-token',
        account_mode: 'coding',
        api_protocol: 'anthropic',
      },
      group_ids: [3],
    }
    const created = { id: 42, name: 'zhipu-login', platform: 'zhipu', type: 'apikey' }
    post.mockResolvedValueOnce({ data: created })

    const result = await createFromLogin(payload)

    expect(post).toHaveBeenCalledWith('/admin/zhipu/oauth/create-from-login', payload)
    expect(result).toEqual(created)
  })

  it('relogins an existing account with the same session_id / state / auth_code contract', async () => {
    post.mockResolvedValueOnce({ data: { message: 'ok' } })

    await relogin(42, { session_id: 'session-1', state: 'state-1', auth_code: 'auth-code-1' })

    expect(post).toHaveBeenCalledWith('/admin/zhipu/accounts/42/relogin', {
      session_id: 'session-1',
      state: 'state-1',
      auth_code: 'auth-code-1',
    })
  })

  it('reads the reset-card status over GET only (R0: observation, never consumption)', async () => {
    const status = {
      available_five_hour_resets: [{ type: 'five_hour', expire_at: '2026-10-07T00:00:00Z' }],
      available_week_resets: [],
    }
    get.mockResolvedValueOnce({ data: status })

    const result = await getResetCardStatus(42)

    expect(get).toHaveBeenCalledWith('/admin/zhipu/accounts/42/reset-card')
    expect(post).not.toHaveBeenCalled()
    expect(result).toEqual(status)
  })

  it('exposes no reset-card consumption entry point (R0 invariant)', () => {
    const exportNames = Object.keys(zhipuAPI)

    expect(exportNames.filter((name) => /use/i.test(name))).toEqual([])
    expect(exportNames).toContain('getResetCardStatus')
  })

  it('is reachable as adminAPI.zhipu for the create/relogin modals', async () => {
    const { adminAPI } = await import('@/api/admin')

    expect(adminAPI.zhipu).toBe(zhipuAPI)
    expect(typeof adminAPI.zhipu.relogin).toBe('function')
    expect(typeof adminAPI.zhipu.createFromLogin).toBe('function')
  })
})
