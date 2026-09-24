-- M19: 记录上游实际积分扣费（workbuddy2api 吸收）
-- codebuddy 上游末帧 usage.credit（字符串数字）是本次调用的真实积分消耗，
-- 与平台内计算的成本(total_cost)是两个口径；持久化供运营对账。
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS upstream_credit numeric(20,10) NOT NULL DEFAULT 0;
