# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目是什么

服务端基础设施套件(与客户端的 `app-infra-toolkit` 配套)。技术栈:Go + 模块化单体 + 标准库 `net/http` + Postgres(Neon)+ pgx;sqlc 做查询代码生成,goose 做迁移。部署目标 Cloud Run(scale-to-zero),Hetzner + Coolify 备选。

module path:`github.com/shaomingbo/server-infra-toolkit`(已冻结,不可改)。

按 ROI 排序的推进站:**T0** 线上最小闭环 →(已落地)**T1** 数据接入层 →(已落地)**T2** auth →(已落地)**T3** 契约对账 →(已落地)T4 离线包 → T5 事件接收 → T6 部署加固。每个推进站另起 session 用 design-gate-lite 锻造独立 PRD 后落地,PRD 落在 `product-requirements/<station>/`。

## 常用命令

```sh
# 本地起服务(读 $PORT,缺省 8080)
go run ./cmd/api
curl localhost:8080/livez            # 应返回 200 + {"status":"ok","version":...}

# 部署前 Neon 探活(不起 HTTP,一次性 SELECT 1 后退出)
go run ./cmd/api -smoke

# 全量校验(CI 的唯一入口,本地也跑这个)
bash scripts/verify.sh               # gofmt / vet / test / 依赖方向 / sqlc 漂移 / 迁移 round-trip

# 跑单个测试 / 单个包
go test ./internal/platform/db -run TestRetry_TransientThenSucceeds
go test -count=1 ./internal/http     # -count=1 禁用缓存,verify.sh 默认带

# 构建并推镜像(必须 linux/amd64,Cloud Run 不跑 arm64)
bash scripts/build.sh [git-sha]

# 部署后冒烟(对线上 base-url 跑外部可观测断言)
bash scripts/smoke.sh https://<service>.run.app [expected-version]
```

sqlc / goose 命令必须先 `cd tools`(工具链锁在独立 module,见下文):

```sh
cd tools && go tool sqlc -f ../sqlc.yaml generate                          # 改了迁移/query 后重新生成
cd tools && go tool goose -dir ../db/migrations postgres "$NEON_DSN" up     # 迁到最新(部署前独立一步,不在服务启动路径)
cd tools && go tool goose -dir ../db/migrations postgres "$NEON_DSN" status # 注意:空库首次跑会建版本表,非纯只读
```

完成判据:`bash scripts/verify.sh` 退出码 0。涉及 push 触发 CI 的,push 后跟踪 CI 至绿才算完成。

## 架构大局(读单文件看不出来的部分)

**分层与依赖方向(这是冻结契约,verify.sh 第 4 步会强制)**:`internal/platform/*`(config/log/db)是最底层,**绝不能 import** `internal/http` 或 `internal/modules`;`internal/http` 可依赖 platform,但不依赖 modules 具体实现。违反会让 CI 红。

**入口串联**(`cmd/api/main.go`):`config.Load()` → `db.NewRetryPool(ctx, dsn)` → `apphttp.NewServer(cfg, pool)` → 标准库 `http.Server` + 优雅停机(先排空 HTTP in-flight 请求,再关池)。`main.go` 里有一行编译期守卫 `var _ apphttp.DB = (*db.Pool)(nil)`,确保池类型满足 HTTP 层的窄接口,签名漂移在 build 时就炸。

**数据访问层三条路径**(`internal/platform/db/`,刻意分开):
- **Smoke**(`db.go`):部署探活,一次性裸连接 SELECT 1 后关闭,**永不**走池,`/livez` 永不调它。
- **NewPool**(`pool.go`):运行时连接池。懒构造——启动不拨号、min conns 强制 0、不预热(scale-to-zero 友好)。`defaultMaxConns=5`、硬上限 `maxConnsCeiling=20`(即使 DSN 指定更大也夹到这);算账:5 × 最多 2 实例 = 10 ≪ Neon 端点上限 ~97,防 scale-zero 唤醒时连接风暴。
- **NewRetryPool**(`retry.go`):包住池做"双缩零重试"——仅重试 **pre-send 的瞬时连接级失败**(冷启动拨号失败、`SafeToRetry`、Class 08 / operator-intervention SQLSTATE);**绝不重试**服务器已报的 PgError(约束/语法错)和 context 取消/超时,因为非幂等语句重跑不安全。3 次尝试上限、100ms 起指数退避、10s 总预算(必须 < 15s HTTP WriteTimeout)。重试事件打到独立 JSON logger(`db_retry_attempt` / `_succeeded` / `_exhausted`)。

**HTTP 中间件链顺序不可乱**(`internal/http/middleware.go`):`recover`(最外)→ `request-id` → `access-log` → handler。request-id 必须夹在中间:recover 从 header 读它、access-log 要带它。错误统一从 `errors.go` 的 `WriteError` 出口,信封形状冻结见下。

**`/livez` 是无依赖的存活探针**:返回 `{status,version}`,**永不碰数据库**(scale-to-zero 安全——Neon 睡着时 Cloud Run 不会因探针失败杀实例)。`livez_guard_test.go` 用 AST + `go list -deps` 强制 `/livez` 不引入 db 包。注意用 `/livez` 不是 `/healthz`(GCP 边缘层保留了 `/healthz`,会被拦)。

**config / log 惯用法**:config 只用 `os.Getenv` + 本地 `.env` 兜底(godotenv,缺失不报错),不引入 flag/配置文件/远程配置(冻结契约)。`NEON_DSN` 必填,包在 `Secret` 类型里——`String()`/`MarshalJSON()`/`LogValue()` 全返回 `[REDACTED]`,只有 `Reveal()` 吐明文(仅开连接时调)。log 是 `log/slog` 的薄封装,JSON 打到 stdout,字段固定(request_id/method/path/status/latency_ms/version)以保持下游日志 schema 稳定。

**tools module 隔离**(`tools/go.mod`):goose + sqlc 用 go 1.26 `tool` 指令锁在独立 module,不污染主服务 module(主 go.mod 只有 pgx + godotenv),不全局安装。所以所有 sqlc/goose 命令都要 `cd tools`,且 `-f ../sqlc.yaml` / `-dir ../db/migrations` 用相对 tools 的路径。

## sqlc / 迁移工作流(顺序不能反)

1. 在 `db/migrations/` 加迁移文件(先建 schema)。文件名五位零填充 `00001_xxx.sql`(sqlc 按字典序读、goose 按序号定序,`10` 会排在 `2` 前)。单文件含 `-- +goose Up` / `-- +goose Down` 两段。
2. `cd tools && go tool sqlc -f ../sqlc.yaml generate` —— sqlc 直接从迁移文件读 schema(迁移目录是 **schema 单一真相源**,不另建第二份 DDL),生成代码到 `internal/platform/db/gen/`(package `dbgen`,带 `Querier` 接口便于 mock)。
3. 本地编译 + `verify.sh`。先写引用某表的 query、表却没迁移建出来 → generate 失败。

`gen/` 全是生成代码,只读,改了迁移/query 必须重新 generate(verify.sh 有漂移 gate,用 `git status --porcelain` 检测,覆盖未 git add 的新生成文件)。

**生产迁移默认前滚不回退**:已发布 schema 出问题写新迁移 `00002_...` 改对,不跑 `down`。破坏性 Down(`DROP TABLE/COLUMN`)标 `IRREVERSIBLE` 注释、不入生产回滚流程。记住"代码回滚 ≠ schema 回滚":`git revert` 代码不会撤销已在 Neon 跑过的迁移。完整规范见 `db/migrations/README.md`。

## 测试分层

- **单元测试**(`*_test.go`,同包):不碰真实 DB,对不可达 DSN 构造池来验策略;无 build tag。
- **集成测试**(`integration_test.go`,外部 `_test` 包,走公开面):只读 `TEST_DATABASE_URL`(**永不**碰 Neon、不硬编码 DSN),未设则 skip(本地默认跳过,CI 用 Postgres 容器)。验证 migration → sqlc → retry pool 整链,只动非业务的 `_infra_selftest` 表。

## 冻结契约(改之前先读 docs/CONTRACTS.md)

`docs/CONTRACTS.md` 是接口宪法。被依赖后兼容性不可侵犯(never break userspace)。冻结项:第一层目录树、module path、包依赖方向、错误信封顶层结构 `{"error":{"code","message"},"requestId"}`、探针端点不带 `/v1/`(`/v1/` 仅业务 API,T2 起)、config 只用 `os.Getenv`+`.env`、`scripts/verify.sh` 是 CI 唯一入口。改冻结项必须走"受控演进出口"(migration note + 更新清单);错误信封只能 append(新增 code / 可选字段),不改顶层 shape。

## 技术选型取舍(本仓库的第一性原则)

默认优化顺序:真实反馈速度 > 跨项目复用性 > 可替换性 > AI 编码友好度 > 可维护性 > 成本可控 > 扩展性。可逆决策优先快,不可逆决策优先稳。

但触及这些必须保守、优先稳:用户数据、认证登录、权限模型、支付订阅、**数据库 schema、数据迁移**、安全边界、日志审计、AI 调用成本、外部服务锁定。CONTRACTS.md 的 frozen 集和迁移"前滚不回退"规范正是这套保守原则的落地。

硬规则:选主流成熟、文档多、AI 熟悉的栈,不为技术审美选冷门;不为"未来可能需要"提前上微服务/中台/复杂权限/workflow engine;商品化基础设施用成熟服务/库不自研,只有差异化业务能力才自建;所有外部服务(LLM/支付/邮件/存储/队列/通知)通过 adapter/facade 隔离,每个模块可独立删除替换迁移;任何新增依赖/抽象/后台任务/数据库表/外部服务都要说明必要性,没有明确收益的复杂度默认拒绝。

## 自维护

本项目发生有意义的结构/契约变更(新增冻结路径、改工作流命令、新推进站落地)时,同步更新本文件与 `docs/CONTRACTS.md`。
