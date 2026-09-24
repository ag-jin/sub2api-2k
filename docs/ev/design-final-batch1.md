# 设计终稿：批1 保号与可控性（2026-09-24）

范围：D4 token保活 · M3 令牌自动续期 · A9/M7 临时停用双状态位 · A7 分级冷却
（前端部分仅含 A9/M7 的面板开关，M2/M4 展示留在批4）

## 1. D4+M3 token 保活与自动续期
- 新组件 `internal/service/codebuddy_token_keepalive.go`
- 触发：每日 22:00 全号刷新（对齐上游）；另每次 tick 检查 `expires_at<3天` 即提前刷（M3）
- 刷新：`POST {chatBase}/v2/plugin/auth/token/refresh`，头族复用 identity 模块；
  响应 accessToken/refreshToken/expiresIn **成对回写** credentials；两段式快照防并发双写
- 失败语义：session 失效连续 3 次才置 `disabled`；单次失败仅记任务日志
- 账号范围：active 且 manual_disabled 不排除（停用期保活照常）；系统 disabled 跳过
- 配置：settings 开关 + 时刻（默认 22:00），复用排程框架
- **验收**：mock 上游单测——成功成对回写/失败3次禁用/手动停用号仍被保活/灰度开关

## 2. A9/M7 临时停用双状态位
- 存储：`extra.manual_disabled` {bool, reason, at}，与系统 `disabled` 位独立；两者都清才回池
- 调度：选号排除 manual_disabled；签到/保活/6004 排程**不检查**该位
- 入口：管理端点 `POST /admin/accounts/{id}/(codebuddy-disable|codebuddy-enable)` + 面板开关
- **验收**：调度单测（停用号不被选中）+ 排程单测（保活照常）+ 端点测试 + 重启保留

## 3. A7 分级冷却对齐
- 402/余额耗尽 → 账号硬冷却至**次日 04:00**（本地时区）；404 → 浅冷却 10m；
- 连续失败达阈值（默认 5）→ 熔断指数退避封顶 6h
- 实现：复用 temp_unschedulable 通道（SetTempUnschedulable），6004 既有路径不变
- **验收**：状态码→冷却映射单测；阈值参数可配

## 4. 灰度与风险
- 生产首刷选 1 个号灰度（刷新会轮换 refreshToken，失败即回滚凭据不覆盖）
- 全部实现走 dev 门禁（健康/根路由/特性A/B），路由记录留档

## 实施顺序
1. A9/M7 双状态位（纯内部+端点，风险最低）
2. A7 分级冷却（复用既有通道）
3. D4/M3 保活与续期（依赖 1 的状态位语义）
