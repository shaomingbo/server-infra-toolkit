-- 00001_infra_selftest: T1 数据接入层的基础设施自验载体(非业务表)。
--
-- 为什么需要这张表:T2 sqlc 把迁移文件目录当作 schema 的单一真相源,
-- 从这里读 CREATE 语句生成类型安全 query。goose 内部的 goose_db_version
-- 版本表不在迁移文件里、sqlc 读不到,所以自验载体必须是这里显式 CREATE
-- 的一张表(见 PRD T1 §12 D4 / FR7)。
--
-- 用途仅为打通「迁移建表 → sqlc 生成 query → 连接池执行」整链(AC8),
-- 不是业务表(NG1)。表注释在 Up 段用 COMMENT ON 显式标注。
--
-- 事务边界:本文件未标 NO TRANSACTION,goose 默认把整个 Up / 整个 Down
-- 各自包在一个事务里执行,半途失败原子回退(FR6)。

-- +goose Up
CREATE TABLE _infra_selftest (
    id    integer PRIMARY KEY,
    label text    NOT NULL
);
COMMENT ON TABLE _infra_selftest IS 'Infrastructure self-test carrier (T1 AC8). NOT a business table (NG1); safe to drop.';

INSERT INTO _infra_selftest (id, label) VALUES (1, 'ok');

-- +goose Down
-- IRREVERSIBLE: DROP TABLE 不可逆丢失表与种子数据。仅用于本地 / 未发布迁移撤销;
-- 生产默认走前滚修复,不跑此 down(见 db/migrations/README.md §down 适用边界)。
DROP TABLE _infra_selftest;
