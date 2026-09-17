#!/bin/sh
# 升级包 pre-step：以数据库属主身份补跑迁移（见 deploy/CARD_DB_UPGRADE.md）
#
# 背景：上游 v0.2.x 起 SQL 迁移改为服务启动必跑；本仓库由 dump 以 postgres 恢复、
# 对象归 postgres，应用角色无归属权 → 启动迁移报 "must be owner of table ..."。
# 本脚本在服务启动前，用同一二进制的 --migrate-only 走 unix socket + peer 认证，
# 以 postgres 身份把待应用迁移跑完；服务随后启动时迁移已是 no-op，归属不再影响运行。
#
# 可手工直接运行排查（需 root）：sudo sh card-db-migrate-preflight.sh
# 可用环境变量覆盖：CARD_BIN（二进制路径）、CARD_DB（库名）、CARD_DB_HOST（默认
# /var/run/postgresql，即本机 unix socket；远程库需改 host 并处理口令）。
set -u

BIN="${CARD_BIN:-/opt/sub2api-copy/sub2api}"
DB="${CARD_DB:-sub2api}"
SOCKET_HOST="${CARD_DB_HOST:-/var/run/postgresql}"

if [ ! -x "$BIN" ]; then
  echo "card-db-migrate-preflight: 二进制不存在或不可执行: $BIN" >&2
  exit 2
fi
if ! command -v runuser >/dev/null 2>&1; then
  echo "card-db-migrate-preflight: 找不到 runuser（Debian/Ubuntu 在 /usr/sbin）；" >&2
  echo "  可改用备选方案：sudo -u postgres sh deploy/card-db-ownership.sh $DB <app_role>" >&2
  exit 3
fi

echo "card-db-migrate-preflight: db=$DB bin=$BIN dsn=host=$SOCKET_HOST（以 postgres 身份 --migrate-only）"
exec runuser -u postgres -- env "MIGRATION_DATABASE_DSN=host=$SOCKET_HOST dbname=$DB sslmode=disable" "$BIN" --migrate-only
