/**
 * Admin CodeBuddy API endpoints
 * QR 吸收绑定（A1）与每日签到（A3）。
 * 路由为 absorption-design v2 冻结契约：
 *   POST /admin/codebuddy/qr/start
 *   GET  /admin/codebuddy/qr/poll?state=
 *   POST /admin/codebuddy/accounts/:id/checkin
 */

import { apiClient } from '../client'

export interface CodeBuddyQrStartResponse {
  state: string
  authUrl: string
}

export type CodeBuddyQrPollStatus = 'waiting' | 'ok' | 'expired' | 'error'

/** 白名单字段，永不含凭据 */
export interface CodeBuddyQrPollAccount {
  id: number
  uid: string
  nickname?: string
  created?: boolean
  updated?: boolean
}

export interface CodeBuddyQrPollResponse {
  status: CodeBuddyQrPollStatus
  account?: CodeBuddyQrPollAccount
}

export interface CodeBuddyCheckinResponse {
  already_checked_in: boolean
  credit?: number
  streak_days?: number
}

export async function qrStart(): Promise<CodeBuddyQrStartResponse> {
  const { data } = await apiClient.post<CodeBuddyQrStartResponse>('/admin/codebuddy/qr/start')
  return data
}

export async function qrPoll(state: string): Promise<CodeBuddyQrPollResponse> {
  const { data } = await apiClient.get<CodeBuddyQrPollResponse>('/admin/codebuddy/qr/poll', {
    params: { state }
  })
  return data
}

export async function checkin(id: number): Promise<CodeBuddyCheckinResponse> {
  const { data } = await apiClient.post<CodeBuddyCheckinResponse>(
    `/admin/codebuddy/accounts/${id}/checkin`
  )
  return data
}

export default {
  qrStart,
  qrPoll,
  checkin,
}
