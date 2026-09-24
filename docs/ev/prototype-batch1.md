# 原型验证记录（批1：token 保活/自动续期 + 临时停用双状态位，2026-09-24）

## 验证范围（六问）
- 吸收对象：wb2api 的 token 刷新流（D4/M3 依赖）、临时停用语义（A9/M7）
- 验证什么：可行性——上游 API 形状是否可对接、我们的数据是否具备前提
- 不验证什么：不做完整实现、不对生产账号真实调用刷新（会轮换 refresh_token）

## P1 token 刷新（D4 保活 / M3 自动续期的前提）
证据：
- 上游 API：`POST {chatBase}/v2/plugin/auth/token/refresh`，鉴权头携带
  access/refresh/domain/uid/deviceToken，响应 `{accessToken, refreshToken, expiresIn, domain}`
  （workbuddy2api internal/upstream/client.go:957-990）
- 关键语义：**响应会轮换 refreshToken**，必须成对持久化；并发模型为两段式
  （锁内取快照→锁外 I/O 30s→写回前快照一致性校验）
- 我方前提：生产四个 codebuddy 账号 `credentials` 均含 `refresh_token`（snake；
  已探明 `refreshToken` camel 不存在——解析时两种键都要兼容）
- 头族：RefreshHeaders 所需字段我们已在 codebuddy_upstream_identity.go 有对应物

结论：**可行**。实现要点：刷新后 `access_token`/`refresh_token`/过期时间成对回写；
session 失效连续 3 次才禁用（对齐上游语义）。

## P2 临时停用双状态位（A9/M7）
设计原型：
- `manual_disabled`（运营手动摘出）与 `disabled`（系统自动禁用）为两个独立位，
  存账号 extra；**两者都清除才回调度池**
- 调度侧：选号排除 `manual_disabled=true`；**签到/保活/6004 排程不检查该位**
  （「摘对话流量」而非「冻结账号」——对齐上游 issue #138/#118 语义）
- 入口：管理端点 disable/enable/revive + 面板按钮；状态随账号持久化，重启保留

结论：**可行**。与我们现有 schedulable 位正交，无迁移（存 extra）。

## 问题集（带入 design-final）
1. 刷新在 dev 用 mock 上游验证；生产首刷选一个账号灰度（轮换失败风险）
2. manual_disabled 是否要在面板账号列表单独一列展示（M4 一并做）
3. 6004 模型级停调（现有）与账号级 manual_disabled 的优先级展示语义
