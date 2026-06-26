-- 为 groups 表添加 intra_group_balance 列（组内负载均衡 / 企业号组）。
--
-- 开启后：会话粘性绑定到本组而非组内单个账号，每次请求在组内成员账号间做
-- 负载均衡选号。用于把多个 Kiro 企业账号编成一个组，会话粘组、组内成员分发、
-- 遭 429 即时换号（Kiro 不讲缓存命中，打破组内粘性零代价）。
--
-- 默认 false：其他所有分组行为完全不变（个人号维持现有会话粘性）。

ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS intra_group_balance BOOLEAN NOT NULL DEFAULT FALSE;
