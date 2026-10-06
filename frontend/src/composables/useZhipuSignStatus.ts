/**
 * 智谱签名运行态（账号级熔断）的共享只读缓存。
 *
 * 数据源：票 28 的 `GET /admin/zhipu/sign/status`（后端返回全局生效值 + 每个
 * 「已启用签名」账号的握手私钥/熔断状态，票 24 落地的运行时状态投影）。
 *
 * 账号列表每行一个配额单元格，逐个请求会把一次列表渲染放大成 N 次 HTTP：
 * 这里做模块级 60s 缓存（同一时刻只有一个在途请求），读取失败静默降级为 null，
 * 由调用方决定「无徽标」而不是虚构状态。
 */

import { getSignStatus } from '@/api/admin/zhipu'
import type { ZhipuSignAccountStatus, ZhipuSignStatus } from '@/api/admin/zhipu'

/** 缓存有效期：签名运行态分钟级变化，60s 足以支撑一次列表浏览且不形成请求风暴。 */
const SIGN_STATUS_TTL_MS = 60 * 1000

let cache: { at: number; request: Promise<ZhipuSignStatus | null> } | null = null

/** 读取签名状态（命中缓存或在途请求时复用；失败返回 null）。 */
export function loadZhipuSignStatus(): Promise<ZhipuSignStatus | null> {
  const now = Date.now()
  if (cache && now - cache.at < SIGN_STATUS_TTL_MS) {
    return cache.request
  }
  const request = getSignStatus().catch(() => null)
  cache = { at: now, request }
  return request
}

/**
 * 取出某账号的熔断状态：仅当运行时状态源可用且确已熔断时返回条目，
 * 其余情况（未接线 / 查不到 / 未熔断）返回 null —— 前端只展示真实状态。
 */
export function resolveZhipuSignDegraded(
  status: ZhipuSignStatus | null | undefined,
  accountId: number
): ZhipuSignAccountStatus | null {
  if (!status || !status.sign_v4_enabled) return null
  const entry = status.accounts?.find((item) => item?.account_id === accountId)
  if (!entry || !entry.runtime_state_available || !entry.circuit_break_tripped) return null
  return entry
}

/** 仅测试使用：清空模块级缓存，保证用例之间互不串数据。 */
export function resetZhipuSignStatusCache(): void {
  cache = null
}
