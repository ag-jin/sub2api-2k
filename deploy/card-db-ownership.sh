#!/bin/sh
# Card 部署包：数据库对象归属对齐（幂等，可重复执行）
#
# 为什么需要：本仓 dev / 生产库最初由 SQL dump 以 postgres 角色恢复，
# 表/序列/视图归 postgres 所有，而应用以 sub2api 连接。上游自 v0.2.x 起把 SQL
# 迁移改为「服务启动时必跑」（删除了 DATABASE_RUN_MIGRATIONS_ON_STARTUP 开关），
# 迁移中的 COMMENT/ALTER 需要对象归属权，否则启动直接失败：
#   apply migration 229_plugins.sql: pq: must be owner of table sub2api_plugin_bindings
#
# 用法（必须以对象当前属主或 superuser 身份执行，例如 OS 用户 postgres）：
#   sudo -u postgres sh card-db-ownership.sh [dbname] [app_role]
# 默认 dbname=sub2api、app_role=sub2api。
#
# 幂等：已对齐时 0 行改动；只动 public schema 下属主 != app_role 的表/序列/视图。
# 可选自愈：把 deploy/systemd/card-db-ownership.conf 装成服务 drop-in，
# 每次启动前自动跑本脚本（ExecStartPre，失败不影响启动决策由 operator 选择）。
set -eu

DB="${1:-sub2api}"
APP_ROLE="${2:-sub2api}"

case "$APP_ROLE" in
  *[!A-Za-z0-9_]*) echo "card-db-ownership: 非法角色名: $APP_ROLE" >&2; exit 2 ;;
esac
case "$DB" in
  *[!A-Za-z0-9_]*) echo "card-db-ownership: 非法库名: $DB" >&2; exit 2 ;;
esac

before=$(psql -d "$DB" -Atc "
  SELECT
    (SELECT count(*) FROM pg_tables   WHERE schemaname='public' AND tableowner <> '$APP_ROLE')
  + (SELECT count(*) FROM pg_sequences WHERE schemaname='public' AND sequenceowner <> '$APP_ROLE')
  + (SELECT count(*) FROM pg_views    WHERE schemaname='public' AND viewowner <> '$APP_ROLE')")

psql -d "$DB" -v ON_ERROR_STOP=1 -q <<SQL
DO \$\$
DECLARE r record;
BEGIN
  FOR r IN SELECT tablename   FROM pg_tables    WHERE schemaname='public' AND tableowner    <> '$APP_ROLE' LOOP
    EXECUTE format('ALTER TABLE public.%I OWNER TO %I', r.tablename, '$APP_ROLE');
  END LOOP;
  FOR r IN SELECT sequencename FROM pg_sequences WHERE schemaname='public' AND sequenceowner <> '$APP_ROLE' LOOP
    EXECUTE format('ALTER SEQUENCE public.%I OWNER TO %I', r.sequencename, '$APP_ROLE');
  END LOOP;
  FOR r IN SELECT viewname     FROM pg_views     WHERE schemaname='public' AND viewowner     <> '$APP_ROLE' LOOP
    EXECUTE format('ALTER VIEW public.%I OWNER TO %I', r.viewname, '$APP_ROLE');
  END LOOP;
END \$\$;
SQL

after=$(psql -d "$DB" -Atc "
  SELECT
    (SELECT count(*) FROM pg_tables   WHERE schemaname='public' AND tableowner <> '$APP_ROLE')
  + (SELECT count(*) FROM pg_sequences WHERE schemaname='public' AND sequenceowner <> '$APP_ROLE')
  + (SELECT count(*) FROM pg_views    WHERE schemaname='public' AND viewowner <> '$APP_ROLE')")

echo "card-db-ownership: db=$DB app_role=$APP_ROLE 待对齐对象 $before -> $after（0 表示已对齐）"
