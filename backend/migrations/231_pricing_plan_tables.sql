-- 定价套餐基础表 + api_keys.pricing_plan_id
-- 包含三张表：pricing_plans（套餐数据与公开产品标记）、
-- pricing_plan_models（套餐内「模型 -> 协议」条目：直连/兼容回退/定价）、
-- pricing_plan_routes（套餐内内部池层：每组一层，按 priority 升序裁决）。
-- api_keys 增加可空的 pricing_plan_id（ON DELETE SET NULL），group_id 保持原样。

CREATE TABLE IF NOT EXISTS pricing_plans (
    id BIGSERIAL PRIMARY KEY,
    name VARCHAR(100) NOT NULL,
    title VARCHAR(100) NOT NULL DEFAULT '',
    description TEXT,
    status VARCHAR(20) NOT NULL DEFAULT 'active',
    is_public BOOLEAN NOT NULL DEFAULT FALSE,
    sort_order INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ NULL,
    CONSTRAINT pricing_plans_status_check CHECK (status IN ('active', 'disabled'))
);

-- 软删除后允许以同名重建：唯一约束只在非删除行上生效
CREATE UNIQUE INDEX IF NOT EXISTS idx_pricing_plans_unique_active_name
    ON pricing_plans (name)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_pricing_plans_status
    ON pricing_plans (status)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_pricing_plans_public_status
    ON pricing_plans (is_public, status)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_pricing_plans_sort_order
    ON pricing_plans (sort_order, id)
    WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS pricing_plan_models (
    id BIGSERIAL PRIMARY KEY,
    plan_id BIGINT NOT NULL REFERENCES pricing_plans(id) ON DELETE CASCADE,
    public_model VARCHAR(200) NOT NULL,
    protocol VARCHAR(20) NOT NULL DEFAULT 'chat_completions',
    upstream_model VARCHAR(200) NOT NULL DEFAULT '',
    direct BOOLEAN NOT NULL DEFAULT TRUE,
    allow_compatibility_fallback BOOLEAN NOT NULL DEFAULT FALSE,
    priority INTEGER NOT NULL DEFAULT 100,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    notes TEXT,
    pricing JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ NULL,
    CONSTRAINT pricing_plan_models_protocol_check CHECK (protocol IN ('chat_completions', 'messages', 'responses'))
);

-- 一个公开模型可走多个协议：唯一约束含 protocol
CREATE UNIQUE INDEX IF NOT EXISTS idx_pricing_plan_models_unique_active
    ON pricing_plan_models (plan_id, public_model, protocol)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_pricing_plan_models_plan_enabled
    ON pricing_plan_models (plan_id, enabled)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_pricing_plan_models_plan_priority
    ON pricing_plan_models (plan_id, priority, id)
    WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS pricing_plan_routes (
    id BIGSERIAL PRIMARY KEY,
    plan_id BIGINT NOT NULL REFERENCES pricing_plans(id) ON DELETE CASCADE,
    group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE RESTRICT,
    priority INTEGER NOT NULL DEFAULT 100,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ NULL
);

-- 每个内部池层在套餐内唯一：每组一层、每层一个优先级
CREATE UNIQUE INDEX IF NOT EXISTS idx_pricing_plan_routes_unique_active_group
    ON pricing_plan_routes (plan_id, group_id)
    WHERE deleted_at IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_pricing_plan_routes_unique_active_priority
    ON pricing_plan_routes (plan_id, priority)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_pricing_plan_routes_plan_enabled
    ON pricing_plan_routes (plan_id, enabled)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_pricing_plan_routes_plan_priority
    ON pricing_plan_routes (plan_id, priority, id)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_pricing_plan_routes_group_id
    ON pricing_plan_routes (group_id)
    WHERE deleted_at IS NULL;

-- API key 可选绑定定价套餐；group_id 保留不变
ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS pricing_plan_id BIGINT REFERENCES pricing_plans(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_api_keys_pricing_plan_id
    ON api_keys (pricing_plan_id);