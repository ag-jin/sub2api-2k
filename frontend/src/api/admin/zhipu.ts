/**
 * Admin Zhipu (bigmodel) OAuth API endpoints
 *
 * Mirrors the frozen HTTP contract of backend ticket 04
 * (`registerZhipuOAuthRoutes`): request field names are `session_id` / `state` / `auth_code`.
 *
 * R0 (hard requirement): this system only ever READS Zhipu reset-card status.
 * There is intentionally no "use reset card" call in this module.
 */

import { apiClient } from '../client'
import type { Account, CreateAccountRequest } from '@/types'

export interface ZhipuLoginUrlRequest {
  proxy_id?: number
  redirect_uri?: string
}

export interface ZhipuLoginUrlResponse {
  login_url: string
  session_id: string
  state: string
}

export interface ZhipuExchangeRequest {
  session_id: string
  state: string
  auth_code: string
}

export interface ZhipuLoginCredential {
  api_key: string
  access_token: string
  zcodejwttoken: string
  refresh_token?: string
  plan_level?: string
}

/**
 * Create a Zhipu account from a completed login.
 *
 * The body is the standard admin account-create payload; `credentials` carries the
 * frozen `bigmodel_oauth` key set built by the caller (ticket 03 whitelist).
 */
export async function createFromLogin(payload: CreateAccountRequest): Promise<Account> {
  const { data } = await apiClient.post<Account>('/admin/zhipu/oauth/create-from-login', payload)
  return data
}

export async function exchange(payload: ZhipuExchangeRequest): Promise<ZhipuLoginCredential> {
  const { data } = await apiClient.post<ZhipuLoginCredential>(
    '/admin/zhipu/oauth/exchange',
    payload
  )
  return data
}

/** Re-login an existing managed account: exchange → re-resolve api_key → merge credentials. */
export async function relogin(accountId: number, payload: ZhipuExchangeRequest): Promise<void> {
  await apiClient.post(`/admin/zhipu/accounts/${accountId}/relogin`, payload)
}

export interface ZhipuResetCard {
  type: 'five_hour' | 'week'
  expire_at?: string | null
}

/**
 * Read-only reset-card status (backend ticket 12, `GET /admin/zhipu/accounts/:id/reset-card`).
 *
 * R0: reset cards are observed only — never consumed. This module must not gain a
 * reset-card "use" call, and this shape is read-only by construction.
 */
export interface ZhipuResetCardStatus {
  available_five_hour_resets?: ZhipuResetCard[]
  available_week_resets?: ZhipuResetCard[]
}

/** R0 read-only observation of an account's remaining reset cards. */
export async function getResetCardStatus(accountId: number): Promise<ZhipuResetCardStatus> {
  const { data } = await apiClient.get<ZhipuResetCardStatus>(
    `/admin/zhipu/accounts/${accountId}/reset-card`
  )
  return data
}

export async function generateLoginUrl(
  payload: ZhipuLoginUrlRequest
): Promise<ZhipuLoginUrlResponse> {
  const { data } = await apiClient.post<ZhipuLoginUrlResponse>(
    '/admin/zhipu/oauth/login-url',
    payload
  )
  return data
}

export default {
  createFromLogin,
  exchange,
  generateLoginUrl,
  getResetCardStatus,
  relogin,
}
