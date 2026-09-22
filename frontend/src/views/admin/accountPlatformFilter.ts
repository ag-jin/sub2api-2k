/**
 * 「账号管理」按平台筛选（侧边栏平台子项 ↔ 列表筛选参数）的共享契约。
 *
 * 侧边栏（`components/layout/AppSidebar.vue`）与列表页（`views/admin/AccountsView.vue`）
 * 必须对这个 URL 参数、这个路径、以及"URL 里的筛选算不算数"用同一套判断，
 * 否则会出现"点了子项但列表没筛"或"筛了但子项不高亮"这类分裂状态。
 * 放在独立模块里也避免了 组件 → 页面 → 组件 的循环 import。
 */

/** 账号管理列表路径。 */
export const ACCOUNTS_PATH = '/admin/accounts'

/** 承载平台筛选的 URL 查询参数名。 */
export const PLATFORM_QUERY_PARAM = 'platform'

/**
 * 从 route.query 读取平台筛选值。
 *
 * 只接受字符串：`?platform=a&platform=b` 会被 vue-router 解析成数组，
 * 这种歧义输入按"未筛选"处理，避免把数组塞进列表请求参数。
 */
export function readPlatformFilterFromQuery(query: Record<string, unknown> | undefined): string {
  const raw = query?.[PLATFORM_QUERY_PARAM]
  return typeof raw === 'string' ? raw : ''
}
