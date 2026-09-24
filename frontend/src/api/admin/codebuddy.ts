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

/** 单账号的非成功明细（失败原因 / 跳过原因）。 */
export interface CodeBuddyCheckinAccountNote {
  account_id: number
  account_name?: string
  message: string
}

/**
 * 批量签到汇总。四态互斥：succeeded / already_checked_in / failed / skipped，
 * total = 四态之和（即本轮考虑过的全部候选账号）。
 */
export interface CodeBuddyCheckinBatchResponse {
  total: number
  succeeded: number
  already_checked_in: number
  failed: number
  skipped: number
  credit_earned?: number
  errors?: CodeBuddyCheckinAccountNote[]
  skipped_notes?: CodeBuddyCheckinAccountNote[]
}

/**
 * 批量签到全部 CodeBuddy 账号（服务端并发执行，上限 5）。
 * 逐账号汇总四态，不因单账号失败中断。
 */
export async function checkinAll(): Promise<CodeBuddyCheckinBatchResponse> {
  const { data } = await apiClient.post<CodeBuddyCheckinBatchResponse>(
    '/admin/codebuddy/accounts/checkin-all'
  )
  return data
}

/** A9/M7 临时停用：只摘出对话流量选号，签到/保活照常（与系统禁用位独立）。 */
export async function manualDisable(id: number, reason?: string): Promise<void> {
  await apiClient.post(`/admin/codebuddy/accounts/${id}/manual-disable`, { reason: reason ?? '' })
}

/** 恢复：清除 manual_disabled 位；系统级禁用仍在则仍不可选。 */
export async function manualEnable(id: number): Promise<void> {
  await apiClient.post(`/admin/codebuddy/accounts/${id}/manual-enable`)
}

export default {
  qrStart,
  qrPoll,
  checkin,
  checkinAll,
  manualDisable,
  manualEnable,
}
