import { createMemoryHistory, createRouter, type Router } from 'vue-router'

/**
 * AccountsView 现在会读 `route.query.platform`（侧边栏平台子项靠 URL 驱动列表筛选），
 * 挂载它的测试必须注入一个 vue-router 实例，否则 useRoute() 取不到注入、setup 直接抛错。
 *
 * 返回的路由停在初始位置（无 query），等价于"全部平台"，因此原有用例的断言不受影响。
 * 需要验证平台筛选行为的用例见 `AccountsView.platformFilter.spec.ts`，它自己建路由并 push query。
 */
export function createAccountsViewTestRouter(): Router {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/admin/accounts', name: 'AdminAccounts', component: { template: '<div />' } },
      { path: '/:pathMatch(.*)*', component: { template: '<div />' } }
    ]
  })
}
