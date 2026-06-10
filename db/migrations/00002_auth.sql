-- 00002_auth: T2 auth「T1 地基」—— 用户 + 刷新令牌 + 访问令牌三表。
--
-- 本迁移只建 schema 地基,不含任何业务逻辑(argon2 哈希、登录校验、token 生成、
-- refresh 轮换、Bearer 验证全在 Go 侧后续 task 填)。表结构为 T2-T7 预留列位:
--   - users.failed_attempts / locked_until:登录失败计数 + 锁定窗口(后续 task 填策略)。
--   - refresh_tokens.selector/verifier_hash:split-token 模式(selector 明文查行,
--     verifier 存 SHA-256,防时序攻击 + 防 DB 泄露即得明文)。
--   - refresh_tokens.token_family / used_at:轮换重放检测(同 family 内某 token 被二次
--     使用 → 整个 family 撤销)。轮换/检测逻辑后续填,这里只建列。
--   - access_tokens.token_hash:opaque 访问令牌的 SHA-256 查找列(不存明文)。
--
-- id 主键用 uuid + gen_random_uuid()(Postgres 13+ 核心内置,无需 pgcrypto 扩展):
-- userId 在 API 层要序列化为 string,uuid 天然是字符串,避免 bigint 暴露行数/可枚举。
--
-- 事务边界:本文件未标 NO TRANSACTION,goose 默认把整个 Up / 整个 Down 各自包在一个
-- 事务里执行,半途失败原子回退(FR6)。

-- +goose Up
CREATE TABLE users (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    username        text        NOT NULL,
    password_hash   text        NOT NULL,
    status          text        NOT NULL DEFAULT 'active',
    failed_attempts integer     NOT NULL DEFAULT 0,
    locked_until    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE users IS 'T2 auth: application users. password_hash holds an argon2 hash (filled by later tasks); failed_attempts/locked_until back login lockout.';

-- 大小写不敏感唯一:用 lower(username) 表达式唯一索引(不引入 citext 扩展)。
-- 同时让 username 的查询走 lower() 归一(见 GetUserByUsername query)。
CREATE UNIQUE INDEX users_username_lower_key ON users (lower(username));

CREATE TABLE refresh_tokens (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    selector      text        NOT NULL,
    verifier_hash bytea       NOT NULL,
    token_family  uuid        NOT NULL,
    expires_at    timestamptz NOT NULL,
    revoked_at    timestamptz,
    used_at       timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE refresh_tokens IS 'T2 auth: split-token refresh credentials. selector is the plaintext lookup key; verifier_hash is the SHA-256 of the verifier; token_family groups a rotation chain for replay detection.';

-- selector 是明文查找键,必须唯一且建索引(每次刷新按 selector 定位单行)。
CREATE UNIQUE INDEX refresh_tokens_selector_key ON refresh_tokens (selector);
-- 按 family 撤销时批量定位同链 token。
CREATE INDEX refresh_tokens_family_idx ON refresh_tokens (token_family);
CREATE INDEX refresh_tokens_user_idx ON refresh_tokens (user_id);

CREATE TABLE access_tokens (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash bytea       NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE access_tokens IS 'T2 auth: opaque access tokens. token_hash is the SHA-256 of the opaque value (never the plaintext); Bearer verification (filled by later tasks) looks the token up by this hash.';

-- Bearer 验证按 token 的 SHA-256 查找,必须唯一且建索引。
CREATE UNIQUE INDEX access_tokens_token_hash_key ON access_tokens (token_hash);
CREATE INDEX access_tokens_user_idx ON access_tokens (user_id);

-- +goose Down
-- IRREVERSIBLE: DROP TABLE 不可逆丢失 users / refresh_tokens / access_tokens 三表
-- 及其全部用户、令牌数据(含密码哈希)。仅用于本地 / 未发布迁移撤销;生产默认走前滚
-- 修复,绝不跑此 down(见 db/migrations/README.md §③④)。CASCADE 顺序:先删依赖
-- users 的子表(refresh_tokens / access_tokens),再删 users。
DROP TABLE access_tokens;
DROP TABLE refresh_tokens;
DROP TABLE users;
