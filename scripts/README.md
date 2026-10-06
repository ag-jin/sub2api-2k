# scripts/ —— 仓库级校验脚本

## check_r0_invariant.sh（票 13）

R0 是硬性要求（`spec.md`）：**永不使用账户的重置卡**。本系统对重置卡只做只读观测
（`reset/status` 读取 → 快照 → 展示），任何形式的「使用」都是违规：调用封装、端点路由、
管理端按钮、自动策略、注释掉/环境开关后置的死代码，一律不接受。

本脚本把这条要求固化为**可重复执行、可举证**的代码层不变量：在整仓代码里检索使用路径的
各种命名形态，命中即非零退出。它不新增任何构建/CI 基础设施，只是一条可复跑的检查命令；
票 18（P2 验证）、票 31（P3 验证）必须复跑并把输出记入验证记录。

### 用法

```sh
# 整仓（默认扫描根 = 仓库根）
scripts/check_r0_invariant.sh

# 只扫指定根（相对仓库根）
scripts/check_r0_invariant.sh backend frontend/src

# 自检：干净样本必须 0 违规、违规样本必须被捕获（模式失效或误报时自检失败）
scripts/check_r0_invariant.sh --selftest
```

退出码：`0` = 不变量成立；`1` = 违规（含缺少 R0 只读语义注释）；`3` = 用法/环境错误。

扫描器：优先 `rg`（ripgrep），缺失时退回 `grep -rE`；模式均为 POSIX ERE，两种引擎结果一致。
排除面：`.git`、`node_modules`、`dist`、`build`、`coverage`、`vendor`、`.scratch`、`docs`、
`openspec`、`*.md`（文档描述协议不构成可执行路径；R0 的强制对象是可执行代码与配置）。

### 判定口径

- **模式清单**（脚本头部 `PATTERN_LABELS` / `PATTERN_REGEXES`，票面清单 + 一条同义动词扩展）：
  `reset/use`、`reset_use`、`reset-use`、`UseResetCard`/`use_reset_card`、
  `resetCardUse`/`reset_card_use`、`idempotency_key` 与重置卡同行组合、
  `consume|redeem|apply|claim|burn` 与重置卡组合。每条模式带边界面守卫
  `([^A-Za-z0-9]|$)`，避免命中的是 `ResetUserAffCode`、`ResetIdempotencyKey` 这类无关标识符。
- **豁免清单**（脚本头部 `EXEMPT_PATHS` / `EXEMPT_CONTENT_REGEXES` / `EXEMPT_REASONS`）：
  三字段一一对应，只有「路径精确匹配 **且** 该行内容匹配豁免正则」才豁免，不做整文件放水；
  **理由必填**（为空即退出码 3，视为配置错误）。当前两条：
  1. 检查器自身（模式字面量 + 自检固件，均非调用）；
  2. `frontend/src/components/common/__tests__/MonitorQuotaView.spec.ts` 里的 R0 负向守卫
     ——仅豁免 `not.toContain(...)` 否定断言行与 `forbidden` 列表中的纯字符串字面量项，
     该文件其余行照常判定。
- **正向检查**：`backend/internal/domain/channel_monitor_quota.go` 与
  `backend/internal/service/zhipu_account_monitor_service.go` 必须含只读语义注释
  `R0：仅观测，永不使用`（票 13 验收项）。

### 新增豁免的纪律

1. 先问「这是不是使用路径」。是 → 删除，不得豁免（注释保留、开关后置同样不允许）。
2. 只有「字面量本身是『不存在』的证据」（负向断言、检查器自身）才可豁免，并且：
   优先按行内容豁免而非整文件；理由写清票号与为什么不是使用路径。
3. 若脚本提示某个豁免规则 0 命中（代码已删除/行内容已变），及时回到脚本头部同步或删除。

### 挂接方式（本票只交脚本与说明，未接线）

R0 检查随时可挂到提交或 CI 上；下面两段是**示例**，接入属后续票的事，本票不新建 CI 配置：

```sh
# .git/hooks/pre-commit （或 pre-commit 框架的 local hook）
#!/bin/sh
exec "$(git rev-parse --show-toplevel)/scripts/check_r0_invariant.sh" || {
  echo "R0 违规：禁止提交任何重置卡使用路径（见 scripts/README.md）" >&2
  exit 1
}
```

```yaml
# .github/workflows/*.yml（示例步骤，本票未添加）
      - name: R0 invariant (no reset-card usage path)
        run: scripts/check_r0_invariant.sh
```

> 注意：仓库 `.gitignore` 原有 `scripts` 规则会忽略本目录，本票为此加了三条锚定例外
> （`!/scripts/`、`!/scripts/check_r0_invariant.sh`、`!/scripts/README.md`），
> 保证脚本随仓库版本化、CI checkout 后可用。
