-- 00003_events: T5 事件接收 —— observability 单表 events。
--
-- 服务端半边:POST /v1/events 批量接收 Envelope,逐条入站 schema 校验后单语句
-- 幂等落库。本迁移只建 schema,不含业务逻辑(解码/校验/落库在 Go 侧)。
--
-- 幂等键(D5/FR3):UNIQUE (source, event_id)。客户端 hold-and-retry 重发同批靠
-- 这个复合唯一约束去重(query 用 ON CONFLICT DO NOTHING 静默跳过已存在行)。去重
-- 窗口 = 保留周期(同表副产品,不另建独立去重表,模块自包含可独立删除)。
--
-- 保留周期(FR6/AC16):received_at + 时间友好索引,便于 T7 按时间 drop。本 PRD
-- 不实现 DELETE/清理路径(清理执行划 T7),这里只建字段 + 时间排序结构。
--
-- kind 不加 CHECK 约束:wire unstable 期(D9),闭集枚举由入站 JSON Schema 层咬,
-- 免得后续 wire 演进还要改迁移。event_ts 由客户端 timestampMs 经 time.UnixMilli
-- 无损转 timestamptz。attributes 用 jsonb(闭集标量值,非自由文本/PII,NFR5)。
--
-- 事务边界:未标 NO TRANSACTION,goose 默认把整个 Up / 整个 Down 各包一个事务。

-- +goose Up
CREATE TABLE events (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source      text        NOT NULL,
    event_id    text        NOT NULL,
    kind        text        NOT NULL,
    trace_id    text        NOT NULL,
    name        text        NOT NULL,
    event_ts    timestamptz NOT NULL,
    attributes  jsonb       NOT NULL DEFAULT '{}',
    received_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source, event_id)
);
COMMENT ON TABLE events IS 'T5 observability: inbound events. UNIQUE(source,event_id) is the idempotency key (D5); received_at + received_at_idx are the time-friendly retention structure for T7 cleanup (no DELETE path here).';

-- 时间友好结构:T7 按时间清理(drop 超期行)用 received_at 索引。
CREATE INDEX events_received_at_idx ON events (received_at);

-- +goose Down
-- IRREVERSIBLE: DROP TABLE 不可逆丢失 events 表全部事件数据。仅用于本地 / 未发布
-- 迁移撤销;生产默认走前滚修复,绝不跑此 down(见 db/migrations/README.md §③④)。
DROP TABLE events;
