/**
 * Pricing Plans API endpoints (user-facing)
 * Handles selectable binding plans for API keys (whitelisted id/name/title/description only)
 */

import { apiClient } from './client'
import type { PricingPlanOption } from '@/types'

/**
 * Get active pricing plans the current user can bind to API keys.
 * Backend only exposes id/name/title/description (no routes/groups/模型协议/成本).
 * @returns List of selectable pricing plans
 */
export async function getAvailable(): Promise<PricingPlanOption[]> {
  const { data } = await apiClient.get<PricingPlanOption[]>('/pricing-plans/available')
  return data
}

export const pricingPlansAPI = {
  getAvailable
}

export default pricingPlansAPI