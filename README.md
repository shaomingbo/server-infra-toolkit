# server-infra-toolkit

一套 Go 服务端基础设施套件:模块化单体架构,标准库 `net/http` 起壳,Postgres(Neon)做数据层,配套 pgx / sqlc / goose 工具链,目标部署到 Cloud Run。

特点:

- **scale-to-zero 友好**:连接池懒构造、存活探针不碰数据库,空闲缩到零实例不会被探针误杀
- **契约冻结纪律**:目录树、依赖方向、错误信封等核心接口一旦发布即冻结,改动须走受控演进出口(见 [`docs/CONTRACTS.md`](docs/CONTRACTS.md))
- **决策全程留痕**:每个推进站的设计取舍(含被淘汰方案)归档在 `product-requirements/`

技术栈细节:Go + 模块化单体 + `net/http` + Postgres(Neon)+ pgx;`sqlc` 做查询代码生成、`goose` 做迁移。部署 Cloud Run 起步,Hetzner + Coolify 备选。

## 现状

T0–T3 与 T5 已落地:HTTP 壳 + `/livez` + 统一错误信封 + 结构化日志(T0)、连接池 + 重试 + sqlc/goose 工具链(T1)、login/refresh 认证 + 账户锁定防线(T2)、wire 契约 schema 真相源 + conformance 防线(T3)、observability 批量事件接收 + 入站契约校验 + 幂等落库(T5,接缝先行:端点默认挂 feature flag 后不公网暴露)。中期 ROADMAP(按 ROI 排序)见:

- [`product-requirements/server-infra-roadmap/prd.md`](product-requirements/server-infra-roadmap/prd.md) — 完整 PRD(14 节)
- [`product-requirements/server-infra-roadmap/handoff.json`](product-requirements/server-infra-roadmap/handoff.json) — 机器可读的任务与依赖清单

推进顺序:**T0** 线上最小闭环 → **T1** 数据接入层 → **T2** auth → **T3** 契约对账 → **T5** 事件接收(ROI 复评先于 T4 推进)→ **T4** 离线包 → **T6** 部署加固;**T7/T8** 为业务触发的 backlog。

每个推进站独立锻造一份 PRD 后再落地,走结构化 PRD 流程(发散批判 → 收敛决策 → 验收标准)。

## 决策档案

`product-requirements/` 是本仓库的决策档案:每个推进站落地前都先产出一份 PRD,完整保留设计取舍的留痕——包括最终方案、被淘汰的候选方案及其否决理由、风险评估与验收标准。读这些目录可以还原"为什么这么设计",而不只是"设计成了什么"。

## 本地起步

1. `cp .env.example .env` 并填入真实 Neon DSN(连接串需含 `sslmode=require`)。
2. `go run ./cmd/api`(读 `$PORT`,缺省 `8080`)。
3. `curl localhost:8080/livez` 应返回 200 + `version` 字段。

## 校验 / 构建 / 冒烟

- 校验:`bash scripts/verify.sh`(gofmt / vet / test / 依赖方向检查,退出码 0 为通过)。
- 构建镜像:`docker build --build-arg GIT_SHA=$(git rev-parse --short HEAD) -t server-infra-toolkit .`
- 部署后冒烟:`bash scripts/smoke.sh <base-url>`

部署到 Cloud Run 的逐步 runbook 见 [`docs/DEPLOY.md`](docs/DEPLOY.md)。
