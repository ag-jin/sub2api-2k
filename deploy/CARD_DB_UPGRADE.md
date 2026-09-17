# 升级包：数据库迁移 pre-step（修 "must be owner of table ..."）

## 为什么会撞

上游 v0.2.x 起把 SQL 迁移改成**服务启动时必跑**（删除了 `DATABASE_RUN_MIGRATIONS_ON_STARTUP`
开关）。本仓 dev / 生产库最初由 SQL dump 以 `postgres` 角色恢复，表/序列/视图归 `postgres`，
而服务以 `sub2api` 连接；迁移里的 `COMMENT`/`ALTER` 需要对象归属权，于是启动直接失败：

```
Failed to initialize application: apply migration 229_plugins.sql:
  pq: must be owner of table sub2api_plugin_bindings
```

dev 首次部署 v0.1.193-card 时实测到该失败；生产只读探测：**181 个对象**归 `postgres`
（105 表 + 序列 + 视图）。

## 方案 A（推荐）：pre-step 以属主身份补跑迁移，**不碰归属**

`deploy/systemd/card-db-migrate-preflight.conf` + `deploy/card-db-migrate-preflight.sh` →
装成服务 drop-in 后，每次启动前以 `postgres`（unix socket peer 认证，**unit 不存口令**）
执行同一个二进制的 `--migrate-only`：

```
systemctl daemon-reload           # 装好 drop-in 后
systemctl restart sub2api-copy    # 升级 = 换二进制 + restart，pre-step 自动补迁移
```

dev 实测：人为把 `sub2api_plugin_bindings` 改回 `postgres` 属主并摘掉 `229_plugins.sql`
的迁移记录 → pre-step 退出 0、`Database migrations completed`、迁移记录补齐、**表归属无需改动**。

## 方案 B（备选）：一次性对齐归属，之后无需 pre-step

```
sudo -u postgres sh deploy/card-db-ownership.sh sub2api sub2api
```

幂等（已对齐时 `0 -> 0`），只动 public schema 下属主 ≠ app_role 的表/序列/视图。
对齐后所有权归应用角色，启动迁移与后续升级都按上游默认路径工作。

## 取舍

| | A：pre-step | B：归属对齐 |
| --- | --- | --- |
| 是否改 DDL | 否 | 是（一次性） |
| 每次启动开销 | 一次 `--migrate-only`（已对齐时毫秒级 no-op） | 无 |
| 权限姿态 | unit 里多一条以 postgres 身份跑迁移的 pre-step | 无额外特权步骤 |
| 未来 dump 恢复后 | 自愈 | 需再跑一次脚本 |

pre-step 脚本可手工直接运行排查：`sudo sh /opt/sub2api-copy/card-db-migrate-preflight.sh`。

**2026-09-17 dev 演练记录（方案 A 的两个坑，均已修）**：首版 conf 用绝对路径
`/usr/bin/runuser`（Ubuntu 实际在 `/usr/sbin`），且带 systemd `-` 前缀把失败静默掉 →
演练中迁移没跑、服务照旧失败而 pre-step 无告警。现改为：脚本内用 PATH 查找 runuser
并显式报错；drop-in **不加** `-` 前缀——迁移失败即服务不启动，journal 直接给根因。

以上文件都随 release tar.gz 分发（`.goreleaser.yaml` 的 `files: deploy/*`）。
