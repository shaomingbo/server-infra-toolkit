# CONTRACTS

冻结契约清单。本文件是 T0 起的"接口宪法":列出哪些路径/约定一旦确立就不可随意改(frozen),
哪些可以自由演进(autonomous),以及受控演进的出口规则。

被依赖后兼容性不可侵犯(never break userspace)。改 frozen 项必须走"受控演进出口"。

---

## 1. frozen-paths(冻结集)

以下项目一经确立即冻结。**重命名/删除**需走受控演进出口(见 §4);**新增**按 append 规则允许。

### 1.1 目录树第一层

- `cmd/`
- `internal/`
- `scripts/`
- `.github/`
- `docs/`

### 1.2 go.mod module path

- `github.com/shaomingbo/server-infra-toolkit`(FR9 / D10,已冻结,不可改)

### 1.3 四项接口契约

1. **包依赖方向(NFR3)**:`internal/platform/*` 是最底层,绝不能 import `internal/http` 或
   `internal/modules`。`internal/http` 可依赖 `internal/platform/*`,但不依赖 modules 具体实现。
2. **错误信封顶层结构(FR2 / D7)**:HTTP 错误响应顶层结构冻结为
   ```json
   {"error":{"code":"<slug>","message":"<human readable>"},"requestId":"<string>"}
   ```
   `Content-Type: application/json`。T0 `code` 用通用 slug(如 `internal`/`bad_request`/`not_found`)。
3. **版本语义**:探针端点(如 `/livez`)**永不**带 `/v1/` 前缀;`/v1/` 前缀**仅**用于业务 API,
   从 T2 起引入。
4. **config 加载机制**:`os.Getenv` 是唯一的环境变量读取面 + 本地 `.env` 兜底。
   不引入其他配置来源(flag/配置文件/远程配置)。

### 1.4 CI 入口

- `scripts/verify.sh` 是 CI 的唯一入口(单一事实来源)。CI 不绕过它直接跑工具。

---

## 2. autonomous-paths(自治集)

以下可自由演进,无需更新本清单:

- `internal/http/` 内部文件结构
- `internal/platform/*` 各子包内部实现与文件组织
- 各包内 fixtures(测试数据与 owning package 共置)
- 测试文件组织方式

---

## 3. 目录树文档

每个节点标注实体化状态:

```
.
├── cmd/
│   └── api/                       [T0 实体] 服务入口 main.go
├── internal/
│   ├── http/                      [T0 实体] HTTP 层(router/handler/错误信封)
│   ├── modules/
│   │   └── auth/                  [T2 实体] login/refresh/Bearer/账户锁定/凭据脱敏
│   │       └── contract/          [T3 实体] login/refresh wire 的机器可读 JSON Schema(wire 真相源)
│   └── platform/
│       ├── config/                [T0 实体] 配置加载(os.Getenv + .env)
│       ├── log/                   [T0 实体] 结构化日志
│       └── db/                    [T0 骨架→T1 实体] pgx 连接池/重试;gen/ 为 sqlc 生成代码
├── db/
│   └── migrations/                [T1 实体] goose 迁移(schema 单一真相源)
├── sql/                           [T1/T2 实体] sqlc query 源(auth.sql 等)
├── tools/                         [T1 实体] 独立 module:goose+sqlc(go tool)
├── product-requirements/         [T1 起] 各站 design-gate-lite PRD
├── scripts/                       [T0 实体] verify.sh 等 CI 脚本
├── .github/
│   └── workflows/                 [T0 实体] CI 工作流
├── docs/                          [T0 实体] CONTRACTS.md 等文档
├── go.mod                         [T0 实体]
├── sqlc.yaml                      [T1 实体] sqlc 配置(schema=db/migrations, out=db/gen)
├── .dockerignore                  [T0 实体]
├── .env.example                   [T0 实体]
├── .gitignore                     [T0 实体,已就位]
└── README.md                      [T0 实体,已就位]
```

注:目录靠放入文件来实体化,不使用 `.gitkeep`(git 不跟踪空目录会让 CI 空跑)。
后续新增目录同样靠放文件实体化,不用 `.gitkeep` 占位。

---

## 4. 受控演进出口(FR6 / AC15 / AC17)

- **新增子目录/子包** = append,直接允许,无需审批;建议同步在 §3 目录树追加节点。
- **重命名/删除已存在的冻结路径(§1)** = 需要 migration note(说明动机、影响面、迁移步骤)+
  更新本清单。
- 错误信封演进:**append-only**。401/429 等新场景通过"新增 `code` 值 + 可选字段"扩展,
  **不改信封顶层结构**。

---

## 5. T0 范围声明

- T0 **不**引入业务错误码枚举闭集;`code` 用通用 slug。
- 错误信封顶层结构冻结;扩展只能 append(新增 code 值 / 新增可选字段),不改顶层 shape。
- 探针端点(`/livez`)不带 `/v1/`。

---

## 6. T2 范围声明(auth 站,W2a–d 落地)

- **业务 API 前缀**:`/v1/auth/{login,refresh}` 走 `/v1/`;探针仍不带 `/v1/`(兑现 §1.3#3)。
- **登录响应 wire 冻结(D13)**:`LoginSession` = `{userId, accessToken, refreshToken, expiresAt(Unix ms)}`,字段名/类型被客户端依赖,冻结;token 明文进体,不脱敏。
- **错误信封**:T2 append 了 `code` 值 `unauthorized`(401),顶层结构不变(兑现 §4 append-only)。
- **反枚举不变量(安全契约)**:所有凭据类失败(密码错 / 用户不存在 / 账户锁定 / 账户禁用)返回逐字节一致的 401 + 同 message,各走恰好一次 argon2 等价计算 + 一次主键 UPDATE;**账户锁定不返 429、不带 `Retry-After`、不加锁定专属 code**(锁定态对外不可观测)。
- **防爆破主防线**:DB 侧账户失败计数 + 锁定(`failed_attempts`/`locked_until`),单语句原子(CTE + `FOR UPDATE`),跨实例生效;通用限流仅留 `RateLimiter` facade 空接缝,未实装。
- **cmd/api 子命令**:`-smoke`(T0 探活)、`-unlock`(T2 运维解锁被 DoS 锁死的账户)。
- **DB 时钟纪律**:账户锁定 / refresh 轮换的时间判定在单一 DB 时钟域(`now()`/`db_now`),不混 app 时钟。

### 6.1 T3 升级说明(契约对账,纯 append)

- **wire 真相源迁移**:auth login/refresh 的 wire 真相,自 T3 起由 `internal/modules/auth/contract/` 下的机器可读 JSON Schema 持有(`login.schema.json` / `refresh.schema.json`)。
- **主从关系明确**:§6 上述自然语言文字自此降为给人看的「人类摘要」,机器可读规范以 schema 为准——schema 为主,文字为从,二者冲突以 schema 为准。
- **服务端校验(两段式防线链)**:服务端 CI(`scripts/verify.sh` step 3 的 `go test ./...`)用两道既有测试合起来咬住「真实响应 wire 符合 schema」,而非单点全链路。第一段——package auth 内的 conformance 测试,把 handler 的返回类型(`loginSession`)经与 handler 同款的 `json.NewEncoder(w).Encode` 路径序列化,校验产出的 wire 符合上述 schema(为何不走 DB/HTTP/`time.Now()` 全链路:PRD FR5/FR6 显式禁止,故测试针对类型本身)。第二段——T2 既有 handler 级测试(`TestLogin_Success` 等)断言真实响应体的精确键集,把「handler 真实输出 ↔ `loginSession` 类型」钉死。两段相接即「真实响应 ↔ schema」:改 `loginSession` 的 json tag 时两道防线同时变红(mutation 实演已验证),防文字与实现漂移。
- **客户端校验(跨 repo)**:客户端仓库 `app-infra-toolkit` 在它自己的 CI 里消费此 schema 校验其解码器(跨 repo pin 由客户端侧负责维护,本仓库不承担客户端 pin 的同步)。客户端以 git-commit-pin 方式 vendored 这两份 schema,故**任何改动 `internal/modules/auth/contract/` 下文件的 commit,message 必须含 `NEEDS-CLIENT-BUMP` 标记**提示客户端 bump pin(纪律详情见 `internal/modules/auth/contract/README.md`)。
- **真相源归属**:真相源留在服务端(本仓库 `internal/modules/auth/contract/`),客户端为消费方。本次变更为纯 append、非 migration note 级变更(未改写/删除任何既有冻结项,仅新增 schema 与人类摘要的主从约定)。
- **新增测试依赖申报**:conformance 测试引入 `github.com/santhosh-tekuri/jsonschema/v6`(v6.0.2)做 schema 校验,**仅测试引用、不进生产二进制**。实测 `go mod tidy` 后本仓库 go.mod 仅新增这一个直接依赖;其模块图中的传递依赖未被本仓库引用面触及——`golang.org/x/mod`/`x/sys`/`x/text`/`x/tools` 均未因本次进入 go.mod/go.sum(go.mod 既有的 `x/sys`/`x/text` indirect 来自 pgx 链,与本次无关),仅 `github.com/dlclark/regexp2 v1.11.0` 因 jsonschema 自身测试引用它而落了两行 go.sum 校验和,不进 go.mod、不参与本仓库任何构建或测试二进制。

---

## 7. T5 范围声明(observability 事件接收站,纯 append)

T5 给服务端新增 observability 事件接收模块(`internal/modules/observability/`):`POST /v1/events` 批量接收 Envelope 数组 → 入站 JSON Schema 校验 → 按 `(source,event_id)` 幂等去重 → 单事务批量落 `events` 新表。本节为纯 append,**未改写/删除任何既有冻结项**。

- **错误信封(append-only,兑现 §4)**:T5 append 了两个 `code` 值——`payload_too_large`(413,请求体超 1 MiB 或单批超 500 条)、`rate_limited`(429,限流接缝拒绝时;noop 期不会真返回,码先 append 留位)。复用既有 `bad_request`(解析失败/schema 违规/空批/非数组)与 `internal`(500,DB 故障——必须 5xx 让客户端保批重试)。**信封顶层结构 `{"error":{"code","message"},"requestId"}` 不变**;批量校验的 rejected 计数放在错误信封 `message` 文本里(如 `"schema validation failed: 1 of 100 events rejected"`),**不**给信封 append 顶层字段。
- **入站契约范式(T3 范式方向反转)**:T3 是服务端**产**响应 wire 自校;T5 是服务端**收**客户端产的请求 wire。服务端持「接受形状」入站 schema(真相源留服务端,与 T3 归属一致),conformance 测试测 handler 真实**拒绝**畸形输入(`additionalProperties:false` / `required` / `type` / `enum` / `AttributeValue` 闭集逐项咬),而非自校它从不产出的 Envelope。schema 文件在 `internal/modules/observability/contract/`(`event.schema.json` 单条事件七字段:其中 `eventId`/`kind`/`traceId`/`timestampMs`/`source`/`name` 六个必填,`attributes` 可选——缺省视同空对象,客户端 init 默认空 map;`batch.schema.json` 数组级不变量),go:embed 编进二进制 + 包级一次性编译 fail-closed(删 schema → 编译失败)。
- **wire 暂不进 frozen 集(unstable,D9)**:T5 的 Envelope 单条形状(`eventId`/`kind`/`traceId`/`timestampMs`/`source`/`name`/`attributes` 七字段,前六个必填、`attributes` 可选缺省视同空对象)与批量请求/响应 wire(裸 JSON 数组请求、`{accepted,duplicate,rejected,requestId}` 响应)**暂标 unstable,不纳入 §1 frozen 集**——客户端 `app-infra-toolkit` 0.1.0 未发布,对端真实消费验证后才冻结(roadmap NFR6,防过早固化错误设计)。
- **模块落点与依赖方向(兑现 §1.3 包依赖方向)**:`internal/modules/observability/` 照 auth 先例(`Handler{store}`+`NewHandler(pool)`+`RegisterRoutes(mux)`,经 `NewServer` 的 registrar-callback 挂接,`cmd/api/main.go` 唯一 wiring,编译期 `var _ observability.DB = (*db.Pool)(nil)` 守卫)。模块**不 import** `internal/http`(错误信封本地渲染,字节钉桩与 `internal/http.WriteError` 一致)、**不 import** `internal/modules/auth`(认证挂点只在 `cmd/api`);`internal/http` 不 import 本模块。`go list -deps` 守卫测试强制。
- **端点默认 feature flag 关、不公网暴露(接缝先行,D2)**:`POST /v1/events` 默认**不挂公网路由**——`cmd/api` 仅当环境变量 `EVENTS_INGEST_ENABLED == "true"` 时才把模块 registrar 传入 `NewServer`,关时路由不注册、请求命中 catch-all 404。这是接缝先行的**有意状态**:服务端逻辑完整落地 + 集成测试覆盖,但公网面暂闭,认证/限流最后一公里待客户端接入定。限流为 `RateLimiter` facade(`Allow(ctx,key)` 同 auth 形状)+ noop 默认,**进程内限流非安全边界**(scale-zero 下最坏 2× + 冷启动重置),账单天花板靠 FR5 请求体/条数硬上限(独立于 max-instances)。
- **保留清理划 T7**:`events` 表带 `received_at` + 时间友好索引(`events_received_at_idx`),便于 T7 按时间 drop;T5 **不实现** DELETE/清理代码路径(模块内零清理)。去重窗口 = 保留周期(同表 `UNIQUE(source,event_id)` 副产品,不另建独立去重表)。
- **自身遥测走 stdout(FR12)**:模块每批接收完成后以 slog JSON 打一条 `events_ingested`(accepted/duplicate/rejected/批大小/request_id)到 stdout,**绝不回写 `events` 表**(防递归/放大)。
- **迁移**:`00003_events.sql` 纯新增,前滚不回退,破坏性 Down(`DROP TABLE events`)标 `IRREVERSIBLE`;sqlc gen 过漂移 gate、迁移从 version 0 round-trip 通过。

---

## 8. T6 范围声明(部署加固站,纯 append)

T6 把部署运行时从「能跑」升级为「已验证」:Cloud Run startup probe 配置 + 关停预算对账 + 双缩零 DEFERRED 验收清偿;账单加被动 budget 告警层;部署链加只读前置校验脚本(`scripts/deploy-precheck.sh`)与正式的蓝绿候选「主线外验证」文档路径。本节为纯 append,**未改写/删除任何既有冻结项**。

> **状态边界(防误读为"已完成")**:本段列的是 T6 范围,**不等于已全部线上落地**。已落地的是 runbook 文本与代码侧防线(关停预算对账常量 + `TestShutdownBudgetFitsGracePeriod`、`deploy-precheck.sh` 只读校验)。其中四项 GCP 实操——probe 配置、budget 告警、双缩零验收、蓝绿演练——按 `docs/DEPLOY.md` runbook 人在回路执行后才回写 `DEPLOY §9`(双缩零)/`§10`(probe)/`§12.5`(蓝绿)的 DEFERRED 标记;**执行前 DEPLOY 各节标的 DEFERRED 是如实状态,不是文档滞后**。

- **工件落点(兑现 §1.1 目录树冻结 + 做小,D7)**:T6 工件全落 `scripts/` + `docs/`,**不开新第一层目录**——`scripts/deploy-precheck.sh`(只读前置校验)+ `docs/DEPLOY.md` 大修(probe/宽限期/蓝绿候选/Neon 用量/budget/坐标 grep 章节)+ 本 §8。第一层目录集合仍与 §1.1 一致。不引入 `service.yaml` 声明式部署路径(probe 期望值写 runbook + describe 只读对账,避免声明式文件与命令式部署双真相漂移)。
- **无新 env var / 端点 / secret / schema / 日志字段(兑现 §1.3#4 config 机制 + 做小)**:T6 **不动 config 机制**(`internal/platform/config/` 零改动,不加 `APP_ENV`/profile/新 secret/日志 env 字段,D8),**不新增端点**(probe 复用既有 `/livez`,绝非 `/healthz`——GCP 边缘保留),**不新增 secret**(Neon 用量降级手动检查,不引入 Neon API key,D2),**不新增 Go 依赖**(go.mod 不变,NFR4)。
- **`scripts/verify.sh` 仍是唯一 CI 入口(兑现 §1.4)**:`deploy-precheck.sh` **不被 `verify.sh` 引用、不进 CI、`.github/workflows/` 无新增文件与步骤**——它属手动部署链(与 `build.sh` / `smoke.sh` 同列),是部署链的只读前置 gate,**非 CI gate**。退出码区分配置缺失(`1`)与凭据缺失(`3`)。
- **`gcloud` 写操作维持「刻意不进脚本」(D6/D9)**:probe 配置、budget 创建、部署 / 切流量 / 回滚等所有 `gcloud` 写操作均为 runbook 文本模板,人在回路手动执行,**不进任何脚本**;`deploy-precheck.sh` 只调只读 API(`get-iam-policy` / `secrets versions access` / 镜像 tag 探测),绝不写。部署保持手动,T6 只加护栏不加自动化(D9),不引入 CD / IaC(Terraform/Pulumi)/ 第二 Cloud Run service(NFR4)。
- **无主动探测(NFR1)**:T6 不新增任何周期性请求服务的任务(Cloud Scheduler/cron/CI 定时);监控只用云厂商被动侧数据(GCP budget alert 邮件 + Neon 控制台手查)。
- **probe 期望值落 runbook + describe 只读对账(R3 漂移检查)**:startup probe 只配指向 `/livez`(参数 `initialDelaySeconds=0, periodSeconds=10, timeoutSeconds=1, failureThreshold=6`,冷启动余量 60s);**不配 liveness probe**(D4,进程挂死发现手段缺位记为已接受风险 R1)。GCP 侧手工配置的期望值落 `docs/DEPLOY.md §10`,部署检查清单含一次 `gcloud run services describe` 只读对账。**Cloud Run 关停宽限期 = 平台固定 10s 常量**(不可配、不出现在 describe),与 `cmd/api` 关停预算锚定断言(HTTP 排空 + 池关闭之和 ≤ 10s)是同一数值,两处同步纪律见 `docs/DEPLOY.md §10.5`。
- **坐标占位纪律(NFR2)**:真实 GCP 坐标(project number / region / 服务域名 / billing account)**只存在于本地 `.env`**,文档与脚本一律占位符;脚本从 `gcloud config` / `.env` / 参数取值,不硬编码。全仓坐标 grep 自查模式与白名单见 `docs/DEPLOY.md §14`,落地后全仓 grep 无真实坐标命中。
