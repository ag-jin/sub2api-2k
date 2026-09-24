# 吸收设计初稿（wb2api + wbmanager，2026-09-24）

64 项 disposition 已全部写入 workflow-state.json coverage[]（COVERAGE_OK 双线 rc=0）。
汇总：adopted 20 / adapted 23 / deferred 12 / rejected 8 / 对齐已adopt含已有 20。

## 实现批次（adopted+adapted 共 43 项）
- 批1 保号与可控性：D4 token保活、M3 令牌自动续期、A9/M7 临时停用双状态位、A7 分级冷却
- 批2 选号均衡：A2 三因子加权、A3 防惊群、C3 概率倾斜、A4 租约语义
- 批3 领积分可见性：M8 积分流水、M9 任务记录页、M10 收益明细+采集落库、F2 批量签到、D1 余额解冻
- 批4 面板完善：M2 有效期预警、M4+F6 6004台账展示、M19 首字延迟与credit扣费列、M16 模型中心
- 批5 设置面：M20/M22/M23/M24/M25/M26、M29/M30
- 批6 对齐修补：B2 思维链、B5 脱敏、D5/D6 补测、M13 密钥哈希、M12 密钥管控
- 6004（A8）已于 v0.1.200 完成

## 关键裁定
- 合规红线：D2 的兑换/抽奖保持手动（用户 full 级禁自动裁定），仅自动部分吸收
- deferred 12 项集中于：账本分层(A5/A6/C1)、双域(E1/M15)、提示词(B3/M21)、测试台(M17)、IP管控(M18)
- rejected 8 项均有现状替代或无场景，理由逐条在 coverage 记录
