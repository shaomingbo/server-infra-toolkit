# server-infra-toolkit

明博的服务端基础设施套件,与客户端基础设施 `app-infra-toolkit` 配套。

- 技术栈(T0 已落地):Go + 模块化单体 + `net/http` + Postgres(Neon)+ pgx;`sqlc` / `go-playground/validator` 为后续引入(T1 起)
- 部署:Cloud Run 起步,Hetzner + Coolify 备选

## 现状

T0 线上最小闭环已落地:`cmd/api` 的 HTTP 壳 + `/livez`、统一错误信封、结构化日志、config 加载 + 密钥脱敏、CI 入口脚本、多阶段 Dockerfile、Neon 一次性探活。中期 ROADMAP(按 ROI 排序)见:

- [`product-requirements/server-infra-roadmap/prd.md`](product-requirements/server-infra-roadmap/prd.md) — 完整 PRD(14 节)
- [`product-requirements/server-infra-roadmap/handoff.json`](product-requirements/server-infra-roadmap/handoff.json) — 机器可读的任务与依赖清单

推进顺序:**T0** 线上最小闭环 → **T1** 数据接入层 → **T2** auth → **T3** 契约对账 → **T4** 离线包 → **T5** 事件接收 → **T6** 部署加固;**T7/T8** 为业务触发的 backlog。

每个推进站另起 session 用 design-gate-lite 锻造独立 PRD 后再落地。

## 本地起步

1. `cp .env.example .env` 并填入真实 Neon DSN(连接串需含 `sslmode=require`)。
2. `go run ./cmd/api`(读 `$PORT`,缺省 `8080`)。
3. `curl localhost:8080/livez` 应返回 200 + `version` 字段。

## 校验 / 构建 / 冒烟

- 校验:`bash scripts/verify.sh`(gofmt / vet / test / 依赖方向检查,退出码 0 为通过)。
- 构建镜像:`docker build --build-arg GIT_SHA=$(git rev-parse --short HEAD) -t server-infra-toolkit .`
- 部署后冒烟:`bash scripts/smoke.sh <base-url>`

部署到 Cloud Run 的逐步 runbook 见 [`docs/DEPLOY.md`](docs/DEPLOY.md)。
