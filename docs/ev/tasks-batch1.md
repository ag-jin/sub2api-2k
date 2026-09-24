# 批1 任务清单（tasks，2026-09-24）

顺序即依赖序；每项含写入范围与验收。TDD：先测后码。

## T1 临时停用双状态位（A9/M7）— owner: wb2api
- 写入：backend/internal/service/（账号状态+调度排除）、handler/admin/、面板账号操作
- 验收：调度排除单测；保活/签到不受影响单测；端点测试；重启保留（extra 持久化）

## T2 分级冷却（A7）— owner: wb2api
- 写入：backend/internal/service/codebuddy_account_cooldown.go（新）
- 验收：402→次日04:00 / 404→10m / 连败→熔断退避封顶6h 的映射单测；参数可配

## T3 token 保活+自动续期（D4/M3）— owner: wb2api（wbmanager 共享成果）
- 写入：backend/internal/service/codebuddy_token_keepalive.go（新）+ 排程注册 + settings
- 依赖：T1（manual_disabled 语义）、T2（失败禁用走 disabled 位）
- 验收：mock 上游——成功成对回写；失效3次禁用；手动停用号仍保活；灰度开关

## T4 面板开关（A9/M7 前端）— owner: wbmanager
- 写入：frontend/src/views/admin/（账号操作菜单+确认）
- 验收：组件测试——停用/启用/恢复三态与原因输入

## T5 dev 部署+门禁+路由记录（收尾）— owner: wb2api
- 写入：docs/route-records/active/、发布 tag
- 验收：健康/根路由/特性A/B 三项 + 记录落档

## 未覆盖声明
本批只做批1 五任务；批2-6 与 deferred/rejected 项不在本批（coverage 已记录去向）。
