-- T5 事件接收 query(observability)。批量幂等落库。
-- 查询的表由 db/migrations/00003_events.sql 建出(schema 单一真相源)。
--
-- 设计说明(走简报 §4 fallback,见下):理想形态是单语句多数组 unnest INSERT,
-- 但 sqlc v1.31 内置静态分析器不支持多参数 unnest(anyarray, anyarray, ...) 的列类型
-- 推断(报 "function unnest(unknown,...) does not exist"——它的内置 catalog 只有单参数
-- unnest)。配真实 database analyzer 会让 verify.sh 的 sqlc 漂移 gate 依赖跑着的库,
-- 超出本阶段范围。故退到 §4 fallback:单行参数化 INSERT,handler 侧用一个显式事务
-- 把整批逐条排队发出(单连接、单事务、原子),仍满足 FR4「单请求不按事件数 fan-out
-- 连接」。每条 ON CONFLICT (source,event_id) DO NOTHING 静默跳过重复(D5/FR3)。

-- name: InsertEvent :execrows
-- 单条幂等插入。:execrows 返回实际插入行数(0 = 命中已存在被 ON CONFLICT 跳过,
-- 1 = 新插入);handler 累加得 accepted,duplicate = 批大小 - accepted。
INSERT INTO events (source, event_id, kind, trace_id, name, event_ts, attributes)
VALUES (@source, @event_id, @kind, @trace_id, @name, @event_ts, @attributes)
ON CONFLICT (source, event_id) DO NOTHING;
