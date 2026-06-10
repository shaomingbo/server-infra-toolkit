# PRD: T0 线上最小闭环(server-infra-toolkit 第一推进站)

> design-gate-lite 产物 · mode **L1** · slug `t0-online-minimal-loop`
> 上游:`product-requirements/server-infra-roadmap/prd.md` 的 T0 站(ROADMAP task `T0`,deps 空)
> 配套机器可读文件:`handoff.json`(同目录)

---

## 1. Summary

为 server-infra-toolkit(Go 服务端基础设施,独立新 repo,代码层 greenfield)的第一推进站 T0「线上最小闭环」起草可落地 PRD(mode **L1**)。T0 把服务端从空仓库带到「能真实部署上线的最小骨架」,范围 6 项:① HTTP 服务壳 ② 统一错误响应信封 ③ 服务端自身结构化日志 ④ 配置与密钥注入 ⑤ CI 入口脚本 ⑥ 一次真实 Cloud Run/Neon 部署冒烟;并在第一站一次性落定**顶层目录树 + CI 约定 + 四项接口契约**的冻结边界。目的是把明博最大的两个未知——Go 上手 + 部署链——在最便宜的时点点亮,并给后续 T1–T6 一套不需再起迭代去补的地基。本 PRD 由协调者并行调度 5 个 opus sub agent(1 扫描客户端取证 + 4 视角批判)综合而成,关键实现细节落 §12 可推翻 default。

---

## 2. Goal Alignment

- **目标用户**:明博——规划者 + 唯一实现者,熟 Node.js、Go 新手,单人开发 + AI 杠杆。次要消费者:后续 T1–T6 各站的实现 agent(复用 T0 的目录树/CI/错误信封/日志/config 机制);客户端 `app-infra-toolkit`(契约对端,消费服务端 HTTP 状态码做错误归一)。
- **问题**:服务端 greenfield,需要一个能真实上线的最小骨架承载后续所有模块;同时把 Go + 部署链这个最大未知在最便宜处先点亮,并避免客户端历史痛点(事后专门起迭代补目录/CI、重排被依赖路径)。
- **成功长什么样**:clone repo → 本地 `scripts/verify.sh` 全绿 → push 触发 GitHub Actions 真 runner 跑绿 → 一条命令部署到 Cloud Run → `/livez` 返回 200 且日志能看到 request id / 时延 / 部署版本 → 一次性 `SELECT 1` 证明连通 Neon → 整条部署链打通;顶层目录树 + CI 约定 + 四项接口契约已写死冻结,后续模块接入零目录/CI 迁移。
- **范围边界**:范围 = 6 项闭环 + 目录/CI 约定 + 四项接口契约冻结 + 本地可跑性;非目标见 §5(逐条钉死防超做)。
- **首个验收信号**:一次 Cloud Run 部署后对线上 URL 跑冒烟脚本,5 条端到端断言全绿(见 AC12)。
- **status**:`clear`(目标从 ROADMAP §7 T0 条目与其 handoff `next_prompt` 直接点明,A.5 目标对齐 gate 未触发——`target_user` 与 `success_outcome` 单一可信,所有开放点都在实现层而非目标层,未打断明博)。

---

## 3. Background and Problem

server-infra-toolkit 是明博的服务端基础设施套件,与客户端 `app-infra-toolkit` 配套。ROADMAP(`product-requirements/server-infra-roadmap/prd.md`)已用 L2 流程把服务端排成 T0–T6 + backlog 的有序推进流,**T0 是其第一站、所有后续站的依赖根**(handoff task `T0` 的 deps 为空)。本 PRD 把 ROADMAP 里 T0 那「一行表格」zoom-in 成可直接落地的规格。

当前 server repo 代码层 greenfield:只有 `README.md`、`.gitignore`(已按 Go 项目配好,且含 `.env`/`*.pem`/`*.key`——密钥不进 repo 的基础防护已就位)、以及 ROADMAP 产物。没有任何 Go 代码、`cmd/`、`internal/`。

一个明确痛点来自客户端历史:`app-infra-toolkit/product-requirements/toolchain-cleanup/` 是一个**专门为补 lint/CI、规整目录而起的迭代**,其决策 D3 明确「不重排 `modules/` 布局」,因为「重排会撞穿四套构建系统硬编码路径,并让 codegen 漂移检测假绿」(移动脚本读写的文件 → 硬编码 glob 不再匹配 → 漂移检测静默 pass 实则已坏)。T0 要避免重蹈:**目录/CI 在第一站一次轻量定好、不事后专门起迭代、不冻死会演进的内部布局。** 但 T0 比客户端风险低——greenfield 单一 Go 构建系统,没有多构建系统并存。

为什么 T0 先做「最小闭环」而非先做某个客户端对端:这是 ROADMAP 五路批判的共识(其 D3)——Go 新手最大的未知在部署链,且错误约定/对端都需要一个 server 壳来承载,先做对端等于在没有壳的空中建楼。

---

## 4. Users and Use Cases

- **主要用户**:明博(按本 PRD 把空 repo 建成可部署骨架,一次部署冒烟点亮部署链;此后每站复用 T0 地基)。
- **次要消费者**:T1–T6 各站实现 agent(implementer/reviewer);客户端 `app-infra-toolkit`(契约对端)。
- 用例:
  1. 明博照本 PRD 的 task DAG(T1–T9)把空 repo 建成可部署骨架,一次 Cloud Run 部署冒烟验证整条链路。
  2. T1 起每站复用 T0 的顶层目录树、`scripts/verify.sh`、错误信封、结构化日志、config 加载机制,不重新搭地基、不改顶层布局。
  3. 后续站 PR 验收含「顶层冻结路径未被重命名/删除」检查项(对齐 ROADMAP AC4)。
  4. 客户端某链路(如登录)联调时,server 用正确的 HTTP 状态码(401/403/429/5xx)让客户端 `ErrorCode` 归一逻辑工作(客户端纯靠状态码归一,不解析响应体)。

---

## 5. Goals and Non-goals

**Goals**(见 `handoff.goals`):
- 6 项闭环逐项可跑通,且每项都有运行时验收锚点(不靠「文档列全了」过关)。
- 一次落定顶层目录树 / CI 约定 / 四项接口契约(包依赖方向、错误信封、版本语义、config 加载机制),区分冻结集 vs 自治集两张清单,后续模块接入零目录/CI 迁移。
- 本地可跑性:clone → 照 README ≤3 步 → 本地起 server + `verify.sh` 绿(降低后续每站上手摩擦)。
- 为 T1–T6 留接缝但不超前抽象(config 机制与内容分离、错误信封 append-only 扩展)。

**Non-goals**(逐条钉死——`requirement` 与 `redteam` 一致指出:AC「缺一不可」只防漏做,T0 真正高发的是**超做**,因 AI 生成 Go 倾向「教程式完整」):
- 不建 pgx 连接池、不引 sqlc、不建表、不写迁移(全属 **T1**)。T0 的「连通 Neon」= 一次性裸连接 `SELECT 1`、用完即关,不留连接池代码。
- 不做 auth、不加 Bearer 中间件、不建任何业务/对端路由(属 **T2**)。T0 只有运维探针端点。
- 不实现限流/配额(NFR5 在 ROADMAP 是横切约束,首次落地在 **T2/T5** 真实入口;T0 无业务入口可限)。
- 不做完整健康检查(依赖探测)、不做优雅关闭的连接 draining、不做多环境/成本告警完整版(属 **T6**)。T0 只做最小 `/livez` + 可选的 SIGTERM 短 grace。
- 不碰签名私钥(属 **T4** offline-package;T0 唯一真 secret 是 Neon DSN,不为演示密钥注入而造 secret)。
- 不定义业务错误码枚举闭集(各业务站按需 append;T0 只定错误信封结构 + 通用 slug)。
- 不建完整 codegen 生成器(属 **T3+**);`internal/contracts/generated/` 在 T0 不建实体文件。

---

## 6. Requirements

### 功能性(FR)

- **FR1 — HTTP 服务壳**:`net/http` 起 server,监听 Cloud Run 注入的 `PORT` 环境变量(缺省 fallback、bind `0.0.0.0` 而非 localhost);中间件链固定顺序 `recover(最外) → request-id → access-log → handler`,所有错误经统一错误响应出口。
- **FR2 — 统一错误响应信封**:所有错误经统一出口;HTTP status 表达错误类别(客户端靠 status 归一,见 §8 证据);响应体为可扩展稳定信封 `{"error":{"code":string,"message":string},"requestId":string}`,`code` 用通用 slug(如 `internal`/`bad_request`/`not_found`);**T0 不定义业务错误码枚举闭集**;401/429 等留各业务站以「新增 code 值 + 可选字段」append-only 扩展,不改信封顶层结构。
- **FR3 — 服务端自身结构化日志**:每请求一条 JSON 日志输出到 stdout(Cloud Logging 采集),含 `request_id` / `method` / `path` / `status` / `latency` / `version` 六字段;`request_id` 优先透传入站 `X-Request-Id`,缺失则生成,并回写到响应 header。明确标注:这是服务端**自身**可观测(NFR4),与 T5 客户端事件接收端点是两回事,不混。
- **FR4 — 配置与密钥注入**:单一 `Config` struct,`os.Getenv` 为唯一读取面;本地用 `.env`(gitignored)加载到进程 env,生产把 Neon DSN 存 Secret Manager 并由 Cloud Run 以 `--set-secrets` 挂成 env(代码不调 Secret Manager SDK,降一个未知);`Config` T0 只含 `Port`/`Version` 等非密项 + 经 secret 注入的 DSN,**不为 T1/T2/T4 的未来字段提前建抽象**(YAGNI);secret 字段用专门类型(`String()`/`MarshalJSON()` 返回 `[REDACTED]`)从类型层阻止进日志/序列化。
- **FR5 — CI 入口脚本**:`scripts/verify.sh` 为唯一入口(CI/hook 只 `bash scripts/verify.sh`,脚本里不出现 linter 二进制名,换工具只改一处);首版 gate = `gofmt -l`(输出非空即失败) + `go vet` + `go test -count=1 ./...`;接 `.github/workflows/verify.yml` 在 GitHub Actions 真 runner(版本显式 pin,不用 `-latest`)跑绿一次。**此 gate 只跑本地类校验,不触发任何远端部署。**
- **FR6 — 顶层目录树 + 四项接口契约冻结**:按 ROADMAP §7.3 落定顶层目录契约(fixtures 布局按客户端实际修正,见 §12 D9);**冻结对象 = 目录树第一层 + 四项接口契约(包依赖方向 / 错误信封 / 版本语义 / config 加载机制) + `scripts/verify.sh` 作为 CI 唯一入口这一事实**;产出**冻结集 vs 自治集**两张显式清单;含受控演进出口(新增子目录/子包 = append 允许;重命名/删除已存在路径 = 需 migration note + 更新冻结清单)。
- **FR7 — 一次真实部署冒烟**:构建容器(多阶段 Dockerfile) → 先 `--no-traffic` 部署 Cloud Run 新 revision → 跑冒烟脚本(5 断言,见 AC12) → 全绿才 `update-traffic` 切流量;冒烟脚本化(`scripts/smoke.sh`,接 base URL 参数,退出码非 0 即失败),作独立/手动 job,**不进 PR gate**(理由:ROADMAP §15——Neon 冷启动 + Cloud Run flaky 会拖垮单人);部署带成本护栏(`--max-instances` 设小值 + `--min-instances=0` 显式)。
- **FR8 — 本地可跑性**:`README` 含 ≤3 步本地启动指引;clone 后按指引能本地起 server 且 `./scripts/verify.sh` 退出码 0;缺 `.env` 时报明确错误而非 panic。
- **FR9 — module path 与 Go 版本一次定死**:`go.mod` 的 module path 作为与目录树同级的冻结契约(给出确切字符串,被依赖后不可侵犯——`never break userspace`);Go 版本三处一致(`go.mod` 的 `go` 指令 / `setup-go` version / Dockerfile `FROM golang` 标签)。

### 非功能(横切约束,T0 适用项)

- **NFR1 — 无常驻进程**:HTTP 壳适配 Cloud Run 缩零(无后台常驻 goroutine、无本地状态依赖);T0 可选最小 SIGTERM 响应(`server.Shutdown` 短 grace),不做连接 draining(留 T6)。
- **NFR2 — 密钥注入纪律**:Neon DSN 走 Secret Manager(明博 R3 选定);密钥/密码不进 env 明文、不进 repo、不进 image layer、不进日志;`.dockerignore` 排 `.env`/`*.key`/`*.pem`/`.git`;不用 build-arg 传 secret(会留痕)。
- **NFR3 — 包依赖方向契约**:`internal/platform/*` 为最底层,不得 import `internal/http` 与 `internal/modules`;`internal/http` 可依赖 `platform/*`,不依赖 `modules` 具体实现;`internal/modules/*` 之间不直接 import。可用 depguard 或 `go list -deps` 机器校验。
- **NFR4 — 服务端自身可观测性 T0 就位**:`request_id` / 结构化日志 / 时延 / 部署版本。版本来源 = build 时 ldflags 注入 git short SHA(主) + Cloud Run revision 环境变量兜底(**具体变量名实现时以 Cloud Run 当前文档核实,不凭记忆硬写**)。
- **NFR5 — 独立 repo CI 隔离**:CI 只含 Go/Linux 步骤,不含 macOS/Xcode。

---

## 7. User Flow / State Flow

### 7.1 请求处理流(每个入站请求)

```
入站请求
  → recover 中间件(最外层,兜 panic → 统一错误出口 → 500 + 错误信封)
  → request-id 中间件(读入站 X-Request-Id;缺失则生成;回写响应 header)
  → access-log 中间件(请求结束记一条 JSON:request_id/method/path/status/latency/version)
  → 路由
      ├ GET /livez → 200 {"status":"ok","version":"<sha>"}(不探 DB)
      └ 未匹配路由 → 统一错误出口 → 404 错误信封
```

### 7.2 部署冒烟状态流

| 状态 | 触发 | 下一步 |
|---|---|---|
| build 镜像 | 多阶段 Dockerfile,ldflags 注入 git SHA | 成功 → 部署;失败 → T0 未完成,排查 Dockerfile/CGO/GOOS |
| 部署新 revision(`--no-traffic`) | `gcloud run deploy --no-traffic` | revision Ready → 冒烟;失败 → 排查 PORT/IAM(403)/sslmode |
| 冒烟(5 断言) | `scripts/smoke.sh <url>` | 全绿 → 切流量;任一红 → 流量留旧 revision(首次无旧则失败 revision 不接流量),T0 未完成 |
| 切流量 | `gcloud run services update-traffic --to-latest` | 线上由新 revision 服务 → T0 部署项完成 |
| 回滚(失败时) | 流量保持旧 revision / `update-traffic --to-revisions=<prev>=100` | 用户不受失败 revision 影响 |

---

## 8. Data, API, Permissions

### API(T0 仅运维端点,无业务端点)

- `GET /livez` → `200` + `{"status":"ok","version":"<sha>"}`,**不探 DB**(依赖探测属 T6;若探 DB,Neon 缩零会让健康检查失败 → Cloud Run 杀实例,与「T0 不做完整健康检查」矛盾),**非版本化路径**(不挂 `/v1/`)。
- 任一未匹配路由 → 经统一错误出口返回错误信封(如 `404` + `{"error":{"code":"not_found",...},"requestId":...}`)。

### 错误信封 schema(契约对端形态)

```json
{ "error": { "code": "string(通用 slug)", "message": "string(人类可读)" }, "requestId": "string" }
```
- `Content-Type: application/json`;标 `unstable until 业务站消费`。
- **客户端归一证据**(codebase-scan,`app-infra-toolkit/modules/network/.../ErrorCode.kt` + `ErrorCodeNormalizer.kt`):客户端把 HTTP **status** 映射成其私有 `ErrorCode` 7 值枚举(`401→UNAUTHORIZED` / `403→FORBIDDEN` / `429→RATE_LIMITED` / `5xx→SERVER_ERROR` / 其他 `4xx→CLIENT_ERROR` / fallback `UNKNOWN`),**不解析响应体取 code**——body 只喂人类 `message`。**对 T0 的硬约束:只要 HTTP status 正确,客户端归一就工作;错误信封不是客户端归一的前提,但服务端日志里的 `code` 值若取自那 7 个 wire 词,两端说同一套词汇。**

### 版本化决策

- T0 **不上** `/v1/` URL 版本化。证据(codebase-scan):客户端对 `/v1`、`/api/`、`baseUrl`、`Accept-Version` 全部零命中——**客户端根本不用 URL 版本化**(它用 module-artifact semver + contract semver + jsbridge 整数版本 handshake)。ROADMAP §8 说的「第一天 /v1/ 版本化」是客户端不镜像的新约定。
- `/v1/` 版本化纪律从 T2 第一个业务端点开始;运维探针端点**永不**版本化(业务 API 升 v2 时探针不该变 `/v2/health`)。T0 无业务端点,`/v1/` 此刻是「已声明但为空的命名空间」,属合理留缝而非过度设计。

### Data

- T0 不定义任何持久化实体;唯一 DB 交互是冒烟期一次性 `SELECT 1`(验 DSN + 网络 + Neon 醒着,**不**建池/不建表/不引 sqlc)。

### Permissions / Secrets

- Neon DSN 存 Secret Manager(明博 R3 选定 D2);Cloud Run runtime service account 需授 `roles/secretmanager.secretAccessor`(否则运行时读 secret 报 403,且报错在 IAM 层不在 Go 代码,新手最难自查——见 E3);本地用 `.env` 提供 DSN。
- Neon 强制 TLS,DSN 需含 `sslmode=require`(具体值实现时核实);用 pooler 端点还是直连端点实现时明确选一。

---

## 9. Acceptance Criteria

> 来源 A = sub agent 标 `Verifiable: yes` 的 finding 转 AC;来源 B = 协调者补的核心 done criteria。每条引用存在的 FR/NFR,description 不含模糊词。**关键设计:把 ROADMAP 对 T0 的 `code_review` 验收(只验「文档列全 6 项」)重写成运行时/冒烟断言(验「6 项真的跑通」)。**

| ID | 验收标准 | 验收方式 | 关联 |
|---|---|---|---|
| **AC1** | 代码通过 `os.Getenv("PORT")` 读端口、`ListenAndServe` 处无硬编码端口字面量、bind `0.0.0.0`;本地起服务后 `curl /livez` 返回 200。 | automated_test | FR1 |
| **AC2** | 中间件三件套各有断言:(a) 同进程两次请求的 `request_id` 不同;(b) 构造一条会 panic 的测试路由,触发后进程不退出、经统一错误出口返 5xx、日志记录该 panic;(c) 每请求 stdout 有一条含 `method`/`path`/`status`/`latency` 的 JSON 日志。 | automated_test | FR1, FR3 |
| **AC3** | `curl` 一个不存在路由,响应体是 `{"error":{"code","message"},"requestId"}` 固定结构、`Content-Type: application/json`,且该响应的 `requestId` 与日志中该请求的 `request_id` 一致。 | automated_test | FR2 |
| **AC4** | 一次请求的日志 JSON 含 `request_id`/`method`/`path`/`status`/`latency`/`version` 六字段且非空;入站带 `X-Request-Id` 时日志 `request_id` 等于入站值。 | automated_test | FR3, NFR4 |
| **AC5** | 线上 `/livez` 的 `version` 等于部署所用 `git rev-parse --short HEAD`(不是 `dev`/`unknown`);本地未注入时为确定 fallback 值 `dev`。 | manual_check | NFR4, FR4 |
| **AC6** | `.gitignore` 含 `.env`/`*.pem`/`*.key`(已就位);存在 `.env.example` 且值全为占位;`.dockerignore` 含 `.env`/`*.key`/`*.pem`/`.git`;secret 字段类型的 `String()`/`MarshalJSON()` 返回 `[REDACTED]`,单测断言含 secret 的 config 序列化/打日志后输出不含明文 secret 值;`git log -p` 全历史 grep 不到私钥头 `-----BEGIN`。 | automated_test | NFR2, FR4 |
| **AC7** | Neon DSN 经 Secret Manager 注入(部署用 `--set-secrets`),不以明文出现在 workflow/repo/Dockerfile;Cloud Run runtime service account 绑定 `roles/secretmanager.secretAccessor`。 | manual_check | NFR2, FR4 |
| **AC8** | `verify.sh` 在「故意制造一个 gofmt 违例」时退出码非 0、在「故意写一个失败测试」时退出码非 0;repo 至少含 1 个真实 `*_test.go`,覆盖错误信封序列化 / secret 脱敏 / PORT 解析中至少 2 项;`go test` 带 `-count=1`。 | automated_test | FR5 |
| **AC9** | `.github/workflows/verify.yml` 只 `bash scripts/verify.sh`、不 spell 内部步骤、不出现 linter 二进制名;runner 版本显式 pin(非 `-latest`);`go.mod` 的 `go` 指令 / `setup-go` version / Dockerfile `FROM golang` 标签三处 Go 版本字符串一致;GitHub Actions 真 runner 跑绿一次。 | manual_check | FR5, FR9, NFR5 |
| **AC10** | 冒烟执行一次 `SELECT 1` 对 Neon 返回成功;`grep -rn pgxpool internal/ cmd/` 为空;`go.mod` 无 sqlc 依赖;DB 探活代码带注释标注「T0 smoke only, pool 见 T1」且行数 < 30;无 `db/migrations/` 实体文件、无 `sql/` 实体文件。 | automated_test | FR7 |
| **AC11** | `/livez` handler 不发起任何 DB 连接(不 import db 探活包或不调用探活);Neon 缩零时 `/livez` 仍返回 200。 | automated_test | FR7 |
| **AC12** | 部署冒烟「通过」= 对线上 URL 的 5 条端到端断言全绿:① Cloud Run revision Ready 且 serving;② `GET /livez` → 200 且 `version` == git SHA;③ Cloud Run 日志中该请求有结构化条目(含 `request_id`/`latency`/`version`);④ 一次性 `SELECT 1` 对 Neon 返回成功;⑤ `GET` 不存在路由 → 约定错误信封 JSON。5 条同时绿才算冒烟通过。 | manual_check | FR7, NFR4 |
| **AC13** | 部署采用先 `--no-traffic` 部署新 revision → 跑冒烟 → 通过才 `update-traffic --to-latest`;冒烟失败时线上仍由旧 revision(或无流量的失败 revision)服务,用户不受影响(首次无旧 revision 的情况单独标注为已知例外);存在 `scripts/smoke.sh`,接受 base URL 参数,退出码非 0 即冒烟失败。 | manual_check | FR7, NFR1 |
| **AC14** | 仓库含多阶段 Dockerfile,`docker build` 本地成功产出可运行镜像;builder 阶段含 `CGO_ENABLED=0 GOOS=linux`;runtime 基于 distroless 或 alpine(`docker run --rm <img> which go` 失败,即镜像不含 Go 工具链);`docker history` 各层无 secret 文件;无 `ARG` 传递 secret。 | manual_check | FR7, NFR2 |
| **AC15** | PRD/repo 含两张显式清单(frozen-paths / autonomous-paths);T0 完成后 `find . -type d -empty` 为空(无 `.gitkeep` 占位空目录);目录树文档每个节点标 `[T0 实体 \| 后续实体化]`;CI 含一条检查:对模拟「重命名 `internal/http` 或 `internal/modules/X`」的 diff 报红、对「新增子目录」放行。 | code_review | FR6 |
| **AC16** | CI 含依赖方向检查(depguard 或 `go list -deps` 断言):`internal/platform/*` 的依赖闭包不含 `internal/http` 或 `internal/modules`;对故意让 `platform/config` import `internal/http` 的改动报红。 | automated_test | NFR3 |
| **AC17** | PRD 声明错误信封顶层结构 + T0 不引入业务错误码枚举;声明 401/429 通过「新增 code 值 + 可选字段」扩展、不改信封顶层结构;探针端点不带 `/v1/` 前缀、`/v1/` 仅承载业务 API。 | code_review | FR2, FR6 |
| **AC18** | `README` 含 ≤3 步本地启动指引;按指引 clone 后能本地起 server 且 `./scripts/verify.sh` 退出码 0;缺 `.env` 时报明确错误而非 panic。 | manual_check | FR8 |

---

## 10. Edge Cases and Failure States

- **E1 — 部署冒烟失败**:5 断言任一红 → 流量留旧 revision(首次无旧 revision 则失败 revision 不接流量),标 T0 未完成、排查部署链;这不算回滚操作,是「未切流量」。
- **E2 — PORT 未注入/写死**:容器报「failed to start and listen on the port defined by the PORT environment variable」→ 代码必须读 `os.Getenv("PORT")`、bind `0.0.0.0`、有 fallback(8080)。这是 net/http 新手 + Cloud Run 的头号失败模式。
- **E3 — Secret Manager 403**:Cloud Run service account 缺 `secretAccessor` → 运行时读 secret 报 403 PermissionDenied(在 IAM 层,不在 Go 代码)→ 部署前置 checklist 含授权步骤,冒烟失败第一排查项指向 IAM。
- **E4 — ldflags 注入失败**:`.dockerignore` 排了 `.git` 导致 builder 阶段 `git rev-parse` 失败 → `version=dev` 进生产 → 视为冒烟不通过(AC5);推荐用 `--build-arg GIT_SHA` 在 `docker build` 外面算好 SHA 传入,比容器内跑 git 可靠。
- **E5 — Neon 连接缺 sslmode**:连不上(Neon 强制 TLS)→ DSN 必须含 `sslmode=require`(实现核实具体值);pooler 端点 vs 直连端点明确选一。
- **E6 — CI 空跑绿**:几乎空的 repo 上 `go test ./...` 对无测试包返回退出码 0、`gofmt` 默认只打印不返回非零 → gate 形同虚设。缓解:`gofmt -l` 输出非空即失败 + 至少 1 个真测试 + `go test -count=1`(AC8)。
- **E7 — .env 误提交**:`.gitignore` 已含 `.env`(已就位)+ 提供 `.env.example` 占位 + `.dockerignore` 排 `.env`,三层防护。
- **E8 — 顶层目录后续被重排**:冻结集 CI 检查对「重命名/删除」报红 + 受控演进出口(重命名需 migration note + 更新冻结清单),避免重蹈客户端 toolchain-cleanup。

---

## 11. Risks and Mitigations

| ID | 风险 | 影响/概率 | 缓解 | owner |
|---|---|---|---|---|
| **R1** | Go 新手在部署链/Dockerfile/ldflags/IAM 等高杠杆环节判断不足,AI 生成的 Go 难严格自审 | high/medium | 最大未知前置 T0 消化;每个踩坑点(PORT/CGO/GOOS/sslmode/secretAccessor/ldflags)写进 PRD 与 task;冒烟 5 断言防假绿 | Mingbo |
| **R2** | 范围超做——顺手把 T1 的 DB 连接池、T2 的 auth、T6 的优雅关闭、NFR5 限流做进 T0(AI 倾向教程式完整) | medium/high | 逐条 non-goal 钉死;AC10/AC11 机器校验(`grep pgxpool` 为空、`/livez` 不探 DB 等) | engineering |
| **R3** | CI/冒烟假绿——空跑绿、只连不发 query、只验 happy path | medium/medium | AC8 防空跑绿(gofmt -l 非空即 fail + 真测试 + -count=1);AC12 五断言含错误路径与真 query | engineering |
| **R4** | 密钥泄漏——进日志 / 进 image layer / 进 git 历史 | high/medium | NFR2 多层:专门类型 `[REDACTED]` + `.dockerignore` + 不 build-arg 传 secret + Secret Manager;AC6 单测+历史 grep | engineering |
| **R5** | 顶层契约定得太死或太松,撞 toolchain-cleanup 覆辙 | medium/low | 冻结集 vs 自治集两清单;只冻第一层 + 四接口契约;受控演进出口;greenfield 单构建系统风险本就低于客户端 | engineering |
| **R6** | GCP/Neon/GitHub 前置未就绪,导致 T0 部署/CI task 阻塞 | medium/medium | 前置 checklist 作 Mingbo 介入节点(T6 CI 首跑、T9 部署);见 Open Questions Q1 | Mingbo |

---

## 12. Default Decisions

> 协调者替明博做的可推翻决策。D2 是明博在 R3 选择题里选定的,其余是协调者按 Brief + 五路调研 + 客户端证据默拍。

- **D1**:T0 冒烟连 Neon = 一次性裸连接 `SELECT 1`(`pgx.Connect` + `Close`),不引连接池/sqlc/迁移。*Why*:五路共识,连接池与缩零适配是 T1,T0 只证 DSN+网络+Neon 醒着。*Override if*:明博想 T0 就建连接池(则 T1 范围前移)。
- **D2**:T0 密钥注入走 Secret Manager(Neon DSN 存 Secret Manager,Cloud Run `--set-secrets` 挂成 env,本地用 `.env`)。*Why*:明博在 design-gate R3 选择题里选定,贯彻 NFR2 不留安全债。*Override if*:GCP Secret Manager 未就绪(见 Q1)→ 改 Cloud Run env 直配 DSN 并标 NFR2 临时例外、T4 收口。
- **D3**:Cloud Run 部署用多阶段 Dockerfile(不用 Buildpacks)。*Why*:对 Go 新手可读、可本地 `docker build` 复现、ldflags 好注入;是 Hetzner+Coolify 备选路径的通用资产;distroless 镜像小、冷启动代价低。*Override if*:明博想用 `--source` Buildpacks 省 Dockerfile(则放弃 ldflags 版本注入的可控性)。
- **D4**:部署版本号 = build 时 ldflags 注入 git short SHA(主) + Cloud Run revision 环境变量兜底。*Why*:唯一干净的版本来源,冒烟靠它确认「线上是不是我刚推的版本」。*Override if*:实测 Cloud Run 不注入该 revision 变量,则只用 ldflags + 本地 fallback。
- **D5**:健康检查 `/livez` 不探 DB、走非版本化路径(不挂 `/v1/`)。*Why*:依赖探测属 T6;探针挂 `/v1/` 会把运维探针和业务版本轴错误耦合。原定 `/healthz`,因 GCP 保留该路径(T0 部署冒烟实测被边缘层 GFE 拦截、请求到不了容器)改用 `/livez`。*Override if*:明博要 T0 就做 readiness/liveness 分离(前移 T6 部分)。
- **D6**:T0 不上 `/v1/` URL 版本化;`/v1/` 纪律从 T2 第一个业务端点起。*Why*:codebase-scan 证据——客户端零 `/v1/` URL 版本化,T0 也无业务端点。*Override if*:明博要 T0 就立 `/v1/` 命名空间约定。
- **D7**:错误响应体定可扩展信封 `{error:{code,message},requestId}`,T0 不定义业务错误码枚举闭集,`code` 用通用 slug。*Why*:客户端纯靠 HTTP status 归一、不解析 body code;业务码留各站 append-only。*Override if*:业务站发现需 T0 就锁错误码枚举(则回填)。
- **D8**:顶层冻结对象 = 目录树第一层 + 四项接口契约(包依赖方向 / 错误信封 / 版本语义 / config 加载机制) + CI 唯一入口;产冻结集 vs 自治集两清单;含受控演进出口(append 允许,重命名/删除需 migration note)。*Why*:architecture 洞察「冻结目录 ≠ 锁接口」,只冻目录名是客户端教训学一半。*Override if*:明博认为四项接口契约某项不该在 T0 冻结。
- **D9**:T0 只实体化当前用到的目录(`cmd/api`、`internal/http`、`internal/platform/{config,log}`、`scripts`、`.github/workflows`、`product-requirements`),不用 `.gitkeep` 占空目录,缺的目录由首次用到的站创建;fixtures 与 owning package 共置(**修正** ROADMAP §7.3 的顶层 `fixtures/contracts/`——客户端实际把 fixtures 放在各 module 内部,无顶层 fixtures 目录)。*Why*:git 不跟踪空目录、空目录让 CI 空跑;对齐客户端实际布局。*Override if*:明博偏好 `.gitkeep` 占位完整目录树。
- **D10**:`go.mod` module path + Go 版本作为与目录树同级的冻结契约(module path 给确切字符串、Go 版本三处一致)。*Why*:module path 是被依赖后不可侵犯的接口(`never break userspace`),改它要全局替换。*Override if*:module path 选错需改,走受控迁移流程。
- **D11**:部署带成本护栏(`--max-instances` 设小值 + `--min-instances=0` 显式声明)。*Why*:防单人开发者最怕的「半夜账单」(min-instances>0 保活或失控重试会产生账单)。*Override if*:明博要 `min-instances>0` 换更低冷启动延迟(牺牲缩零)。
- **D12**:T0 不碰签名私钥(T4 才用),唯一真 secret 是 Neon DSN。*Why*:避免为演示密钥注入而造 secret、过早引入 T4 能力。*Override if*:明博要 T0 就验签名私钥注入链路。

---

## 13. Open Questions

- **Q1**:T0 动手前,GCP 项目(+ Secret Manager 权限)、Neon 数据库(+ DSN)、GitHub repo(+ Actions)三处前置是否已就绪?
  - *为什么是明博的*:只有明博知道自己这三个云账号/项目的实际状态,且都需要他本人登录操作(`gcloud` 认证、建 Neon project、配 GitHub secrets),协调者无法代为确认或配置。
  - *推荐默认*:动手前按前置 checklist 逐项确认,缺失项先补齐。其中 Secret Manager 的 `secretAccessor` 授权是 D2 选定方案的硬前提——若未就绪则触发 D2 的 override(T0 临时改 Cloud Run env 直配 DSN,标 NFR2 例外,T4 收口)。

---

## 14. Implementation Tasks

> 每个 task 是 T0 实现 session 内的一步。`deps` 即施工顺序,`done_when` 引用 §9 的 AC 作为验收闸。owner 主力 `implementer`(opus);**T6(CI 首跑)与 T9(部署冒烟)是明博介入节点**(需明博的 GitHub / GCP / Neon 凭据与操作)。

- **T1**(deps: —;done: AC10, AC15):repo 骨架 + 目录冻结。建顶层目录树(只实体化用到的,不占空目录)、`go mod init`(定 module path)、`cmd/api/main.go` 空壳、`.dockerignore` + `.env.example`、写冻结集/自治集两清单。
- **T2**(deps: T1;done: AC1, AC11):HTTP 壳 + `/livez`。`net/http` 监听 `$PORT`、最小 mux、`/livez` 返回 200 + version(先用 `dev`)。
- **T3**(deps: T2;done: AC2, AC3, AC4, AC17):三横切。结构化日志(JSON→stdout)、中间件链(recover→request-id→access-log,顺序固定)、统一错误出口 + 错误信封 schema。
- **T4**(deps: T2;done: AC6, AC18):config 加载 + 密钥脱敏。`LoadConfig` 只读 env、本地 godotenv 加载 `.env`、`Config{Port,Version,DSN}`、secret 专门类型 `[REDACTED]`、缺 `.env` 报明确错。
- **T5**(deps: T3, T4;done: AC5):version 注入接线。`main.version` 变量、ldflags 接线(`--build-arg GIT_SHA`)、Cloud Run revision 兜底。
- **T6**(deps: T3, T4;done: AC8, AC9, AC16):verify.sh + CI(明博介入:首次真 runner)。`scripts/verify.sh`(gofmt -l 非空即 fail / go vet / go test -count=1)+ 依赖方向检查 + `.github/workflows/verify.yml` 调脚本、真 runner 跑绿一次。
- **T7**(deps: T5;done: AC14):Dockerfile。多阶段(builder `CGO_ENABLED=0 GOOS=linux` + ldflags;runtime distroless/alpine)。
- **T8**(deps: T4;done: AC10):Neon 冒烟探活函数。裸 `pgx.Connect` + `SELECT 1` + `Close`,DSN 从 config 读,不建池/不引 sqlc,标注 T0-only。
- **T9**(deps: T7, T8;done: AC7, AC12, AC13):Cloud Run 部署冒烟 + 回滚(明博介入:GCP/Neon/部署)。`gcloud run deploy --no-traffic` → 冒烟 5 断言 → 通过才 `update-traffic`;Secret Manager 挂 DSN + 授 secretAccessor;成本护栏。

**关键路径**:T1→T2→T3→T5→T7→T9(Dockerfile + 部署是长尾)。**并行点**:T3 ‖ T4;T5 ‖ T6;T7 ‖ T8。

---

## 15. 附:本 PRD 的锻造来源(L1 留痕)

L1 五件套并行锻造(协调者单 message 启 5 个 opus sub agent,均只读):
- **codebase-scan**(design-explorer):扫客户端 `app-infra-toolkit` 取证——错误归一靠 status 不靠 body、客户端零 `/v1/` URL 版本化、CI 单一入口形态、契约/fixture 布局、toolchain-cleanup 教训。**纠正了 ROADMAP 两个想当然**(错误码表、`/v1/`)。
- **requirement** / **redteam** / **implementation** / **architecture**(design-reviewer):四视角批判协调者归一化的 T0 Brief,产出超做风险警告、CI/冒烟假绿防治、密钥安全失败模式、「冻结目录≠锁接口」洞察、9 步 task DAG 与 7 个实现层 default。

唯一升 R3 问明博的决策:**T0 密钥注入策略**(走 Secret Manager / 先 env 留 T4)→ 明博选「走 Secret Manager」,落 D2。其余开放点全部 R1(给 default + 本节 §12 audit)。

*本产物由 design-gate-lite(协调者模式)生成,仅做 PRD 锻造,未写任何实现代码。下游 T0 落地由明博显式授权后另起 session。*
