import { beforeEach, describe, expect, it, vi } from 'vitest'

const { get } = vi.hoisted(() => ({ get: vi.fn() }))

vi.mock('@/api/client', () => ({ apiClient: { get } }))

import { platformCounts } from '@/api/admin/accounts'

// 侧边栏「账号管理」子项靠这个接口判断哪些平台有账号。
describe('admin accounts platform counts endpoint', () => {
  beforeEach(() => {
    get.mockReset()
  })

  it('reads the per-platform counts endpoint and passes the payload through', async () => {
    const data = { anthropic: 3, zhipu: 1 }
    get.mockResolvedValue({ data })

    await expect(platformCounts()).resolves.toBe(data)
    expect(get).toHaveBeenCalledWith('/admin/accounts/platform-counts')
  })

  it('keeps an empty payload as an empty map', async () => {
    get.mockResolvedValue({ data: {} })

    await expect(platformCounts()).resolves.toEqual({})
    expect(get).toHaveBeenCalledTimes(1)
  })
})
