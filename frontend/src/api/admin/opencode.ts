/**
 * Admin OpenCode GO API endpoints
 * Half-automatic OpenAuth login: generate auth URL → admin logs in via
 * GitHub/Google in browser → paste back the code → exchange.
 *
 * Note: the OpenAuth token only captures identity (email). The GO API key
 * (sk-...) is still entered manually because OpenAuth tokens cannot read usage
 * or act as the API key (see anomalyco/opencode#8911).
 */

import { apiClient } from '../client'
import type { Account } from '@/types'

export interface OpenCodeAuthUrlResponse {
  auth_url: string
  session_id: string
}

export interface OpenCodeAuthUrlRequest {
  provider?: 'github' | 'google'
  redirect_uri?: string
}

export interface OpenCodeTokenInfo {
  access_token: string
  refresh_token?: string
  id_token?: string
  expires_in?: number
  expires_at?: number
  email?: string
}

export interface OpenCodeExchangeCodeRequest {
  session_id: string
  code: string
  state: string
  redirect_uri?: string
}

export interface OpenCodeCreateFromOAuthRequest extends OpenCodeExchangeCodeRequest {
  api_key: string
  base_url?: string
  proxy_url?: string
  name?: string
  proxy_id?: number
  concurrency?: number
  priority?: number
  group_ids?: number[]
}

export async function generateAuthUrl(
  payload: OpenCodeAuthUrlRequest
): Promise<OpenCodeAuthUrlResponse> {
  const { data } = await apiClient.post<OpenCodeAuthUrlResponse>(
    '/admin/opencode/generate-auth-url',
    payload
  )
  return data
}

export async function exchangeCode(
  payload: OpenCodeExchangeCodeRequest
): Promise<OpenCodeTokenInfo> {
  const { data } = await apiClient.post<OpenCodeTokenInfo>(
    '/admin/opencode/exchange-code',
    payload
  )
  return data
}

export async function createFromOAuth(
  payload: OpenCodeCreateFromOAuthRequest
): Promise<Account> {
  const { data } = await apiClient.post<Account>(
    '/admin/opencode/create-from-oauth',
    payload
  )
  return data
}

export default { generateAuthUrl, exchangeCode, createFromOAuth }
