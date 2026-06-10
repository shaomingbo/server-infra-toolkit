-- 基础设施自验 query(T2):打通「迁移建表 → sqlc 生成 query → 连接池执行」整链(AC8)。
-- 查的是 db/migrations/00001_infra_selftest.sql 建的 _infra_selftest 自验表,
-- 非业务 query(NG1)。

-- name: GetInfraSelftest :one
SELECT id, label FROM _infra_selftest WHERE id = $1;
