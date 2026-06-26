-- 为 groups 表添加「账号池输出整理」两列。
--
-- 1) enforce_models_list：把已有的 models_list_config 从「仅影响 /v1/models 展示」
--    升级为「展示 + 调度拦截」。开启后，请求一个不在 models_list_config.Models 清单
--    内的模型将被直接拒绝（400），而不仅是列表里不显示。默认 false：存量分组维持
--    原有「仅展示」行为，逐字节不变。
--
-- 2) model_alias_mappings：对外统一模型名 → 池内真实模型名 的映射表。调度匹配账号
--    前先把对外名归一化为该池真实模型名。为后续「key 绑接口跨池」铺路：同一对外名
--    在不同池可映射到各自真实模型。默认空 map：不配则不做任何别名替换。

ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS enforce_models_list  BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS model_alias_mappings JSONB   NOT NULL DEFAULT '{}';
