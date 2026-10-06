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

/**
 * 签名 V4 管理端契约（backend ticket 28, `zhipu_sign_config.go` / `zhipu_sign_status.go`）。
 *
 * 字段名与 `gateway.zhipu.sign_*` 配置键逐字一致（后端 JSON tag 同源），前端不做任何
 * 本地默认值推断：C1 的默认口径冲突（M3 默认关 vs M6 写作 on）以后端返回的生效值为准。
 */
export type ZhipuSignFailPolicy = 'open' | 'closed'

/** 生效值快照：运行层覆盖 > 部署层 > 协议默认（后端 `ZhipuSignConfig`）。 */
export interface ZhipuSignConfig {
  sign_v4_enabled: boolean
  sign_client_version: string
  sign_pow_bits: number
  sign_key_ttl_minutes: number
  sign_handshake_backoff_seconds: number
  sign_account_circuit_break_threshold: number
  sign_fail_policy: ZhipuSignFailPolicy
  sign_alert_enabled: boolean
  sign_reconcile_interval_hours: number
  sign_reconcile_deviation_threshold: number
}

/** 管理面读取视图：生效值 + 被运行期覆盖的 system_settings 键（已排序）。 */
export interface ZhipuSignConfigView extends ZhipuSignConfig {
  overridden_keys: string[]
}

/**
 * 管理面更新请求：部分更新——只有传了的键才改，未传的键保持当前生效值。
 * 非法值由后端强校验拒绝（reason=ZHIPU_SIGN_CONFIG_INVALID），且不会写入任何键。
 */
export type ZhipuSignConfigUpdate = Partial<ZhipuSignConfig>

/** 单个账号的握手私钥与熔断状态（不含任何凭据或签名材料）。 */
export interface ZhipuSignAccountStatus {
  account_id: number
  account_name: string
  key_cached: boolean
  last_handshake_at: string | null
  key_expires_at: string | null
  consecutive_failures: number
  circuit_break_tripped: boolean
  circuit_break_reason: string
  /** false 表示运行时状态源未接线，UI 必须渲染「未知」而不是「未熔断」。 */
  runtime_state_available: boolean
}

/** 状态读取响应：全局生效值 + 仅「启用签名」的账号（按 account_id 升序）。 */
export interface ZhipuSignStatus {
  sign_v4_enabled: boolean
  sign_fail_policy: ZhipuSignFailPolicy
  accounts: ZhipuSignAccountStatus[]
}

/** 读取签名生效配置（管理端 GET，audit 只记录写操作）。 */
export async function getSignConfig(): Promise<ZhipuSignConfigView> {
  const { data } = await apiClient.get<ZhipuSignConfigView>('/admin/zhipu/sign/config')
  return data
}

/** 写入签名配置修改（管理端 PUT；后端强校验 + 热更新 + 审计中间件留痕）。 */
export async function updateSignConfig(
  update: ZhipuSignConfigUpdate
): Promise<ZhipuSignConfigView> {
  const { data } = await apiClient.put<ZhipuSignConfigView>('/admin/zhipu/sign/config', update)
  return data
}

/** 读取签名状态（全局生效值 + 每个启用账号的握手私钥/熔断状态）。 */
export async function getSignStatus(): Promise<ZhipuSignStatus> {
  const { data } = await apiClient.get<ZhipuSignStatus>('/admin/zhipu/sign/status')
  return data
}

export default {
  createFromLogin,
  exchange,
  generateLoginUrl,
  getResetCardStatus,
  getSignConfig,
  getSignStatus,
  relogin,
  updateSignConfig,
}
