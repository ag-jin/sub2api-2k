-- 站点图床（票 #36）：API 调用方上传的图片落在对象存储的 bed/ 前缀下，
-- 这张表只做 TTL 记账 —— 清理任务按 expires_at 扫描，先删对象再删行。
--
-- key 是 PostgreSQL 的非保留关键字，可直接作列名：它是不透明文件名（URL 路径里的那一段），
-- storage_key 才是对象存储的完整键（bed/ 前缀 + key），两者刻意分开，避免清理时误改前缀。
CREATE TABLE IF NOT EXISTS image_bed_uploads (
    id BIGSERIAL PRIMARY KEY,
    key VARCHAR(128) NOT NULL UNIQUE,
    storage_key VARCHAR(512) NOT NULL,
    user_id BIGINT NOT NULL,
    content_type VARCHAR(64) NOT NULL,
    size_bytes BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL
);

-- 清理扫描：WHERE expires_at <= now() ORDER BY expires_at LIMIT n
CREATE INDEX IF NOT EXISTS image_bed_uploads_expires_at_idx ON image_bed_uploads (expires_at);
-- 按上传者排查/统计
CREATE INDEX IF NOT EXISTS image_bed_uploads_user_id_idx ON image_bed_uploads (user_id);
