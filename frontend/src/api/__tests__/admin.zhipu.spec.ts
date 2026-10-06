import { beforeEach, describe, expect, it, vi } from 'vitest'

const { get, post, put } = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
  put: vi.fn(),
}))

vi.mock('@/api/client', () => ({
  apiClient: { get, post, put },
}))

import zhipuAPI, {
  createFromLogin,
  exchange,
  generateLoginUrl,
  getResetCardStatus,
  getSignConfig,
  getSignStatus,
  relogin,
  updateSignConfig,
} from '@/api/admin/zhipu'
import type { CreateAccountRequest } from '@/types'

describe('admin Zhipu OAuth API', () => {
  beforeEach(() => {
    get.mockReset()
    post.mockReset()
    put.mockReset()
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

  // ---- 票 28 契约：签名 V4 配置读写 + 状态只读（前端 29 的消费面） ----

  it('reads the effective sign config (28 contract field names) over GET', async () => {
    const view = {
      sign_v4_enabled: true,
      sign_client_version: '0.16.9',
      sign_pow_bits: 8,
      sign_key_ttl_minutes: 1440,
      sign_handshake_backoff_seconds: 30,
      sign_account_circuit_break_threshold: 10,
      sign_fail_policy: 'open',
      sign_alert_enabled: true,
      sign_reconcile_interval_hours: 6,
      sign_reconcile_deviation_threshold: 0.67,
      overridden_keys: ['gateway.zhipu.sign_pow_bits'],
    }
    get.mockResolvedValueOnce({ data: view })

    const result = await getSignConfig()

    expect(get).toHaveBeenCalledWith('/admin/zhipu/sign/config')
    expect(result).toEqual(view)
    expect(result.overridden_keys).toEqual(['gateway.zhipu.sign_pow_bits'])
  })

  it('sends a partial update over PUT so untouched keys keep their effective value', async () => {
    const updated = {
      sign_v4_enabled: true,
      sign_client_version: '0.16.9',
      sign_pow_bits: 8,
      sign_key_ttl_minutes: 1440,
      sign_handshake_backoff_seconds: 30,
      sign_account_circuit_break_threshold: 10,
      sign_fail_policy: 'closed',
      sign_alert_enabled: true,
      sign_reconcile_interval_hours: 6,
      sign_reconcile_deviation_threshold: 0.67,
      overridden_keys: ['gateway.zhipu.sign_fail_policy'],
    }
    put.mockResolvedValueOnce({ data: updated })

    const result = await updateSignConfig({ sign_fail_policy: 'closed' })

    expect(put).toHaveBeenCalledWith('/admin/zhipu/sign/config', { sign_fail_policy: 'closed' })
    expect(result).toEqual(updated)
    expect(get).not.toHaveBeenCalled()
  })

  it('reads per-account handshake and circuit-break status over GET', async () => {
    const status = {
      sign_v4_enabled: true,
      sign_fail_policy: 'open',
      accounts: [
        {
          account_id: 7,
          account_name: 'zhipu-managed-a',
          key_cached: true,
          last_handshake_at: '2026-10-06T02:30:00Z',
          key_expires_at: '2026-10-07T02:30:00Z',
          consecutive_failures: 0,
          circuit_break_tripped: false,
          circuit_break_reason: '',
          runtime_state_available: true,
        },
        {
          account_id: 9,
          account_name: 'zhipu-managed-b',
          key_cached: false,
          last_handshake_at: null,
          key_expires_at: null,
          consecutive_failures: 3,
          circuit_break_tripped: true,
          circuit_break_reason: 'verify_invalid_over_threshold',
          runtime_state_available: true,
        },
      ],
    }
    get.mockResolvedValueOnce({ data: status })

    const result = await getSignStatus()

    expect(get).toHaveBeenCalledWith('/admin/zhipu/sign/status')
    expect(result.accounts).toHaveLength(2)
    expect(result.accounts[0].key_cached).toBe(true)
    expect(result.accounts[1].last_handshake_at).toBeNull()
    expect(result.accounts[1].consecutive_failures).toBe(3)
    expect(result.accounts[1].circuit_break_reason).toBe('verify_invalid_over_threshold')
  })
})
