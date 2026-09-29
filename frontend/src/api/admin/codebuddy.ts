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

// --- 成长链（A6）---

/**
 * 一条成长通道及其合规分级与授权状态。
 *
 * `tier` 是动作的**性质**（preview/claim/full），不随政策变；
 * `autoAuthorized` 表示它是**因用户授权**才可自动（即 full 级被放开）。
 * 两者分开呈现：性质需人担责、但已授权放开的通道，风险画像与
 * "本来就无需人在场"的通道不同，界面不该显示成同一种状态。
 */
export interface CodeBuddyGrowthChannel {
  key: string
  tier: 'preview' | 'claim' | 'full'
  auto_runnable: boolean
  auto_authorized: boolean
  /** 授权依据（谁/何时/为什么）；未授权时为空。 */
  auto_authorization?: string
  rationale: string
}

/** 通道列表（含合规分级与授权依据）。 */
export interface CodeBuddyGrowthChannelsResponse {
  channels: CodeBuddyGrowthChannel[]
}

/** 列出全部成长通道及其分级/授权状态（只读，供面板渲染与运维核对）。 */
export async function growthChannels(): Promise<CodeBuddyGrowthChannelsResponse> {
  const { data } = await apiClient.get<CodeBuddyGrowthChannelsResponse>(
    '/admin/codebuddy/growth/channels'
  )
  return data
}

/** 单通道运行结果（结构随通道而异，用 unknown 承载，由界面按需呈现）。 */
export interface CodeBuddyGrowthRunChannelResponse {
  channel: string
  tier: string
  auto_runnable: boolean
  detail?: unknown
  error?: string
}

/** 手动执行**单个**成长通道（人显式点名，故不受自动排程的开关限制）。 */
export async function growthRunChannel(
  id: number,
  channel: string
): Promise<CodeBuddyGrowthRunChannelResponse> {
  const { data } = await apiClient.post<CodeBuddyGrowthRunChannelResponse>(
    `/admin/codebuddy/accounts/${id}/growth/run`,
    { channel }
  )
  return data
}

/** 一轮成长链汇总（对全部候选账号跑已授权自动的通道）。 */
export interface CodeBuddyGrowthRunSummary {
  attempted: number
  succeeded: number
  skipped: number
  failed: number
  /** 本轮实际跑的通道键。 */
  channels?: string[]
  error?: string
}

/** 手动跑一轮**已授权自动**的通道（未授权的不会被带上）。 */
export async function growthRunAll(): Promise<CodeBuddyGrowthRunSummary> {
  const { data } = await apiClient.post<CodeBuddyGrowthRunSummary>('/admin/codebuddy/growth/run-all')
  return data
}

/** 手动触发一轮活跃上报（人明确要求，故绕开窗口与当日去重）。 */
export interface CodeBuddyGrowthActivityRunResponse {
  attempted: number
  reported: number
  skipped: number
  failed: number
  self_check_suspicious: number
  error?: string
}

export async function growthActivityRun(): Promise<CodeBuddyGrowthActivityRunResponse> {
  const { data } = await apiClient.post<CodeBuddyGrowthActivityRunResponse>(
    '/admin/codebuddy/growth/activity-run'
  )
  return data
}

export default {
  qrStart,
  qrPoll,
  checkin,
  checkinAll,
  manualDisable,
  manualEnable,
  growthChannels,
  growthRunChannel,
  growthRunAll,
  growthActivityRun,
}
