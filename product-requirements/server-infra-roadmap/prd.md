# PRD: Server-Infra-Toolkit 中期 ROADMAP(对标 app-infra-toolkit,按 ROI 排序)

> design-gate-lite 产物 · mode **L2** · slug `server-infra-roadmap`
> 配套机器可读文件:`handoff.json`(同目录)

---

## 1. Summary

对标客户端基础设施 `app-infra-toolkit`,为服务端梳理一份按 ROI 排序的中期 ROADMAP。服务端定为**独立新 repo** `server-infra-toolkit`,技术栈沿用前序决策:Go + 模块化单体 + `net/http` + Postgres(Neon)+ pgx + sqlc + go-playground/validator,Cloud Run 起步、Hetzner+Coolify 备选。

本文档不实现任何模块,只产出:① 一份从 T0 到 T6 的有序推进清单(每项可独立开 PRD 落地)+ backlog;② 一套在第一站一次落定的轻量目录与 CI 约定(直击「不想像客户端那样事后专门起一个迭代补目录/CI」的痛点);③ 贯穿所有模块的横切地基规则。本 PRD 由协调者并行调度 8 个 opus sub agent(1 扫描 + 3 候选发散 + 4 视角批判)加 2 个 Codex 异源 job(候选 + 对抗审查)综合而成,被淘汰方向见第 15 节。

---

## 2. Goal Alignment

- **目标用户**:维护者(单人开发 + AI 协作),已为客户端建有 `app-infra-toolkit`。
- **问题**:客户端有基础设施、服务端空白;缺一份按 ROI 排序、可逐个开 PRD 落地的服务端 ROADMAP,且不希望事后专门起一个迭代补目录/CI。
- **成功长什么样**:一份按 ROI 排序的服务端模块 ROADMAP(每项可独立开 PRD),并在第一站一次定下轻量目录/CI 约定,后续模块接入不再为目录/CI 起专门迭代。
- **范围边界**:范围=中期 ROADMAP(模块清单 + ROI 排序 + 依赖 + 目录/CI 约定 + 第一站);非目标见第 5 节。
- **首个验收信号**:文档列出有序 T0–T6,T0 范围明确到可直接另起 PRD session。
- **status**:`clear`(目标从本次输入与前序对话 context 直接点明,A.5 目标对齐 gate 未触发,未打断明博)。

---

## 3. Background and Problem

客户端 `app-infra-toolkit` 已是一套成熟的「窄深 + 契约导向 + 双端同源验证」基础设施(9 模块,TS spec 单一真相源 → codegen 生成三端,golden fixture 跨端验证,ModuleRegistry 声明式装配,契约版本化)。它已经在代码里定义了「需要服务端提供什么」,但全部是 **NotWired 占位接口**(形态已知、wire 未固化)。

服务端目前是 greenfield。明博的诉求是:在业务需求到来前,先把服务端基础设施按 ROI 排好推进顺序,逐个开 PRD 落地。一个明确痛点来自客户端历史——`product-requirements/toolchain-cleanup/` 是一个**专门为补 lint/CI、规整目录而起的迭代**:当时三端 lint 从零、CI 没在真 runner 跑过,且一度想重排 `modules/` 目录,被硬证据否决(重排会撞穿四套构建系统的硬编码路径、让 codegen 漂移检测假绿),最终决策「不动布局」。服务端要避免重蹈:**目录/CI 在第一站一次轻量定好,不事后专门起迭代,且不重排已被依赖的路径。**

---

## 4. Users and Use Cases

- **主要用户**:明博(规划者 + 唯一实现者)。
- **次要消费者**:后续每个模块 PRD 的实现 agent(implementer/reviewer);客户端 `app-infra-toolkit`(作为契约对端)。
- 用例:
  1. 明博按本 ROADMAP 的顺序,逐个 `/design-gate-lite` 开模块 PRD,再落地。
  2. 落地某模块时,查横切约束章节确认安全/部署/契约纪律,不漏地基规则。
  3. 客户端某条链路(如登录)要联调时,从 ROADMAP 找到对应服务端站点(T2)与其契约状态。
  4. 业务需求到来时,从 backlog 判断是否触发某个 deferred 能力(后台任务/Admin/计费)。

---

## 5. Goals and Non-goals

**Goals**(见 `handoff.goals`):按 ROI 排序、可逐个开 PRD 的模块清单;第一站一次定下轻量目录/CI;服务端经共享契约与客户端配套防漂移;明确 T0 及其验收。

**Non-goals**:
- 不在本 session 实现任何服务端代码。
- 不详设每个模块字段(留各自 PRD)。
- 不碰客户端 `app-infra-toolkit`。
- 不规划具体业务功能(只规划基础设施套件)。
- 不在地基期建完整 spec→Go 生成器。

---

## 6. Requirements

**功能性**
- **FR1**:覆盖客户端已定义的 4 个服务端对端(auth 登录/刷新、离线包 manifest+签名+下载、批量事件接收、错误约定),并标注客户端已固化的数据形态线索。
- **FR2**:包含服务端自身地基模块(HTTP 服务壳、数据接入层、配置与密钥注入)——客户端无对端但服务端必需,不得作为隐含项省略。
- **FR3**:给出按 ROI 排序的推进顺序(T0–T6 + backlog),每项标价值/成本/依赖,可逐个开 PRD,排序依据写明。
- **FR4**:T0 为线上最小闭环(HTTP 壳 + 错误约定 + 自身结构化日志 + 配置密钥注入 + CI 入口 + 一次 Cloud Run/Neon 部署冒烟)。
- **FR5**:定义顶层目录约定与 CI 约定(单一入口脚本、首版只校验增量),T0 一次落定,后续不改顶层布局。
- **FR6**:契约配套采用 fixture 先行、完整生成器后置(auth 固化前手写 DTO + golden fixture)。
- **FR7**:超范围能力(后台/定时、Admin/Ops、权限/租户/计费)列 deferred backlog,标触发条件。

**非功能(横切约束,贯穿所有站)**
- **NFR1**:无常驻进程(Cloud Run 缩零),异步/定时任务用外部触发(Cloud Scheduler/Tasks)。
- **NFR2**:密钥/令牌生命周期——refresh token 哈希存储 + 撤销;签名私钥进 Secret Manager,不进 env 明文。
- **NFR3**:DB schema 变更有版本化迁移且可回滚,缩零环境下触发方式明确。
- **NFR4**:服务端自身可观测性(request id/结构化日志/错误码/时延/部署版本)T0 就位,与 T5 对端事件接收区分。
- **NFR5**:auth、事件上报、离线包下载三类入口有限流/配额,防账单与 DB 被打爆。
- **NFR6**:对端契约先 unstable(手写 + golden fixture),经真实客户端消费验证后才 frozen;制品 semver 与契约 semver 双版本轴。
- **NFR7**:每站 PRD 可独立验收/回滚/合并(粒度上限)。
- **NFR8**:独立 repo,Go/Linux CI 与客户端 macOS/Xcode CI 隔离;契约靠 submodule 或发包引用客户端 spec。

---

## 7. ROADMAP(推进流)

### 7.1 有序推进站(ROI 排序)

| 站 | 模块 | 干什么 | 价值 | 成本 | 依赖 |
|---|---|---|---|---|---|
| **T0** | 线上最小闭环 | HTTP 壳 + 错误约定 + 自身日志 + 配置/密钥注入 + CI 入口 + 一次真实 Cloud Run/Neon 冒烟 + 定顶层目录/CI 约定 | 最高:解锁一切;把最大未知(部署链 + Go 上手)在最便宜时点亮 | 中:含 Go 新手集中学习区 | — |
| **T1** | 数据接入层 | pgx 连接池 + sqlc + 迁移规范 + 双缩零连接池适配 | 高:所有有状态模块前置 | 中:sqlc 工作流首配 | T0 |
| **T2** | auth 登录/刷新 | 第一个真实对端;手写 DTO + golden;token/密钥安全;走通后冻结 auth 契约 | 最高:几乎所有业务门槛 + 解锁客户端登录 | 中:token 安全是单人易错区 | T0,T1 |
| **T3** | 契约对账 | fixture 先行 + 最小 spec→Go DTO 对账(防漂移,不做完整生成器);定 submodule/发包 | 中高:防两端漂移的杠杆点 | 中 | T2 |
| **T4** | offline-package | manifest(Ed25519 签名)+ 下载;预留多 keyId 轮换窗口 | 中:解锁客户端离线包链路 | 中-高:签名字节对齐是硬骨头 | T0,T1,T2 |
| **T5** | observability 接收 | 批量 Envelope 端点 + 限流/幂等/保留周期 | 中:闭环客户端事件上报 | 中 | T0,T1,T2 |
| **T6** | 部署加固 + 多环境 | 健康检查/优雅关闭/成本告警/preview 环境 | 中:稳态上线保障 | 中 | T0 |
| **Backlog T7** | 后台/定时任务 | 缩零下用 Cloud Scheduler/Tasks(token 清理、包处理、事件压缩) | 按需 | 中 | T0,T1 |
| **Backlog T8** | 运营与商业化 | Admin/Ops、权限/租户、计费权益 | 取决于商业模式 | 高且易过早 | T2 |

### 7.2 ROI 排序依据(回应「按 ROI 排序」核心诉求)

排序按 **价值 ÷ 成本**,价值三维 = 解锁度(做完它解锁多少后续)+ 客户端依赖紧迫度 + 跨业务复用性;成本三维 = 实现复杂度 + Go 学习曲线 + 依赖前置。要点:
- **T0/T1/T2 排最前**:解锁度与复用性最高(壳/DB/auth 是后面一切的地基),且 T0 把明博最大的未知(部署链 + Go 上手)在最小代价处先点亮——这是单人 Go 新手风险最高、最该前置消化的。
- **T3 紧随 T2**:契约对账复用性高、是防漂移杠杆,但必须等 auth 有了真实 DTO 当样板,所以排 T2 后。
- **T4/T5 中段**:客户端对端、解锁具体链路,但各有硬骨头(签名字节对齐 / 事件入口限流),且都依赖 T0-T2 地基。
- **T6 收尾**:把 T0 的部署冒烟扩成完整运行时基线。
- **T7/T8 入 backlog**:复用性/紧迫度此刻为期货,且最易把单人节奏拖进过早平台化。

### 7.3 目录与 CI 约定(T0 一次落定,直击痛点)

顶层目录树(顶层契约冻结,模块内布局自治):
```
server-infra-toolkit/
├── cmd/api/                    # main 入口
├── internal/
│   ├── http/                   # 服务壳、路由、中间件、统一错误响应
│   ├── platform/               # 横切基础设施
│   │   ├── config/  db/  log/  crypto/  validate/
│   ├── modules/                # 业务对端(模块化单体):auth/ offlinepkg/ observability/
│   └── contracts/generated/    # spec→Go 生成物(T3 起填充,带生成标记)
├── db/migrations/              # 版本化迁移
├── sql/                        # sqlc 源 SQL
├── contracts/specs/            # 契约 spec(submodule 引用客户端 / 或发包)
├── fixtures/contracts/         # golden fixtures
├── product-requirements/<slug>/# 各模块 PRD(本 ROADMAP 也在此)
├── scripts/verify.sh           # CI 单一入口
└── .github/workflows/verify.yml# CI(仅 Linux/Go,不含 macOS/Xcode)
```

CI 约定(复用客户端「单一脚本 + 只 gate 增量」的血泪教训):
- 唯一入口 `scripts/verify.sh`,hook/CI 只编排触发时机、不出现 linter 二进制名(换工具只改一处)。
- **首版(T0)只 gate**:`gofmt`、`go vet`、`go test ./...`,且第一版就接 GitHub Actions 并在真 runner 跑绿一次。
- **随模块加入再加**:`sqlc generate` 无漂移、`migration 可跑`(T1 起)、生成代码无手改(T3 起)。门禁项只对已落地模块生效,不对空目录空跑。
- `internal/contracts/generated/` 在 T3 生成器落地前不建实体文件(避免「generated 里其实是手写」)。

---

## 8. Data, API, Permissions(对端契约形态 + 安全)

服务端对端**形态已知、wire 未固化**,本 ROADMAP 不定字段、只记线索,详设留各站 PRD:
- **auth(T2)**:`POST /v1/auth/login`、`POST /v1/auth/refresh`;`LoginSession = token(string) + expiresAt(Int64)`,严格解析(数字 token 不强转、expiresAt 非整数/越界双端对齐);401 拒绝。
- **offline-package(T4)**:`/v1/manifest`(`active.json`:`version` + SHA-256 `digest` + `keyId` + Ed25519 签名覆盖 `UTF8(version + "\n" + digest)` + 可选 `minAppVersion`)+ `/v1/package`(ZIP)。
- **observability(T5)**:批量事件 `POST` 端点,统一 `Envelope` 模型(`AttributeValue` 闭集枚举),支持客户端 hold-and-retry,透传 traceId。
- **network 错误约定(并入 T0)**:服务端用 HTTP status 表达错误,客户端做 status→ErrorCode 归一;统一错误响应体在 T0 的服务壳里定。

**权限/安全**:API 从第一天 `/v1/` 版本化(呼应客户端版本协商文化);auth 受保护端点用 Bearer token;refresh token 哈希存储 + 撤销(NFR2);签名私钥进 Secret Manager(NFR2);三类入口限流(NFR5)。

---

## 9. Acceptance Criteria

| ID | 验收标准 | 验收方式 | 关联 |
|---|---|---|---|
| **AC1** | 每个模块都能映射到 4 个客户端对端之一或服务端地基之一,无无归属模块。 | code_review | FR1,FR2 |
| **AC2** | 给出 T0–T6 有序列表,每项含价值/成本/依赖,依赖只指向更靠前的项、无循环。 | code_review | FR3 |
| **AC3** | T0 范围同时含 HTTP 壳、统一错误响应、结构化日志、配置/密钥注入、CI 入口、一次 Cloud Run/Neon 冒烟共 6 项,缺一不可。 | code_review | FR4,NFR4 |
| **AC4** | 给出顶层目录树 + CI 入口约定,并声明新增模块不改顶层目录树(后续 PRD 验收含「顶层目录 git diff 为空」)。 | code_review | FR5 |
| **AC5** | 契约配套写明 auth 固化前手写 DTO + golden fixture、完整生成器排 auth 之后;无 T0/T1 建完整生成器的安排。 | code_review | FR6 |
| **AC6** | 后台/定时、Admin/Ops、权限/租户/计费三类列 deferred backlog 并标触发条件,不出现在 T0–T6 主线。 | code_review | FR7 |
| **AC7** | 横切约束章节逐条列 7 条(无常驻进程/密钥令牌/迁移可回滚/自身可观测/入口限流/契约冻结纪律/PRD 粒度),每条标约束哪些站。 | code_review | NFR1–NFR7 |
| **AC8** | handoff.json 的 tasks 给出 T0–T6 依赖,每个 task 的 done_when 至少引一条 AC,next_prompt 指向 T0;校验器通过。 | automated_test | FR3 |
| **AC9** | 声明独立 repo,写明契约引用客户端 spec 的方式(submodule 或发包二选一作默认);CI 约定不含 macOS/Xcode。 | code_review | NFR8 |

---

## 10. Edge Cases and Failure States

- **E1** 客户端占位接口反向变更:对端处 unstable 阶段,golden fixture 吸收,未冻结前不计破坏兼容。
- **E2** Cloud Run + Neon 双冷启动:T1 连接池配置覆盖冷启动,首请求经重试返回正常,不抛连接错误。
- **E3** 某站落地发现需未排序支撑能力:在该站 PRD 就近处理最小必要支撑,不擅自提为独立站,主线顺序不变。
- **E4** 签名密钥轮换但旧客户端只认旧 keyId:T4 manifest 契约预留多 keyId 并存窗口与信任链,实现可先单 key。
- **E5** 客户端事件风暴式上报:T5 入口限流/配额,超限丢弃或降级,DB 写入与账单不被打爆。

---

## 11. Risks and Mitigations

| ID | 风险 | 影响/概率 | 缓解 | owner |
|---|---|---|---|---|
| **R1** | Go 新手在早期高杠杆环节判断不足,AI 生成 Go 难严审 | high/medium | 最大未知前置 T0 消化;签名/连接池各自独立 PRD + golden fixture + 异源 review | Mingbo |
| **R2** | 契约过早冻结,固化错误设计 | medium/medium | 对端先 unstable,真实消费验证后才冻结(NFR6) | engineering |
| **R3** | 目录/CI 信息不全时定死,撞硬编码路径 | medium/low | T0 只冻结顶层目录契约,模块内自治;独立新 repo 从零定,风险低于客户端多构建系统 | engineering |
| **R4** | 平台化过早拖垮单人节奏 | medium/medium | 后台/Admin/权限/计费列 deferred,业务触发(FR7) | Mingbo |
| **R5** | 双冷启动导致首请求超时/连接风暴 | medium/medium | T1 连接池覆盖冷启动;T0 冒烟即验一次真实冷启动 | engineering |
| **R6** | 密钥/令牌处理草率造成安全债 | high/medium | NFR2:token 哈希 + 撤销、私钥进 Secret Manager;T2/T4 验收强制覆盖 | engineering |

---

## 12. Default Decisions

- **D1**:中期锁定为聚焦 T0–T6(约 3–6 个月),运营/商业化列 backlog。*Why*:输入未指定时间上界,按单人节奏取最合理窗口。*Override if*:明博指定更长窗口或要 backlog 进主线。
- **D2**:服务端定为独立新 repo,CI 与客户端隔离,契约靠 submodule/发包引用客户端 spec。*Why*:明博在目标对齐阶段明确选了独立新 repo。*Override if*:改选 monorepo 等则重排目录/CI/契约章节。
- **D3**:T0 = 线上最小闭环(含真实 Cloud Run/Neon 冒烟),而非先做某对端。*Why*:五路批判共识——Go 新手最大未知在部署链,错误约定/对端需 server 壳承载。*Override if*:已有可复用 Go 骨架则 T0 缩减。
- **D4**:契约采用 fixture 先行、生成器后置(排 auth 固化后)。*Why*:codegen 现仅支持两种 kind,新增 Go DTO 生成非零工程量;Go 新手先写生成器顺序颠倒。*Override if*:明博想尽早投入 codegen 自动化。
- **D5**:两个实现层疑问(客户端是否在攒事件、Envelope spec 是否成形)按「对端为占位、spec 未落 TS」处理,ROADMAP 不依赖客户端先动。*Why*:实现层走 default,不打断明博,不影响排序。*Override if*:客户端实际已有可用 spec/在攒事件,则 T5/T3 紧迫度上调。

---

## 13. Open Questions

- **Q1**:契约与客户端 spec 的共享方式最终用 git submodule 还是发布 spec 包?
  - *为什么是明博的*:取决于他如何治理两个 repo 的版本联动与发布节奏(是否愿意维护一条 spec 发布管道),属 repo 间依赖治理的个人运维偏好。
  - *推荐默认*:起步用 git submodule(零发布管道),待 spec 变更频率升高再切发布包;此项在 T3 正式定。

---

## 14. Implementation Tasks

> 每个 task = 后续一个 PRD session(先 `/design-gate-lite` 锻造该站 PRD,再落地)。`deps` 即推进顺序。

- **T0**(deps: —;done: AC3,AC4):线上最小闭环 + 定顶层目录/CI 约定。
- **T1**(deps: T0;done: AC1,AC7):数据接入层 + CI 加 sqlc/migration gate。
- **T2**(deps: T0,T1;done: AC1,AC7):auth 登录/刷新,走通后冻结 auth 契约。
- **T3**(deps: T2;done: AC5):契约对账(fixture 先行 + 最小对账),定 submodule/发包(Q1)。
- **T4**(deps: T0,T1,T2;done: AC1,AC7):offline-package(Ed25519)+ 预留 keyId 轮换。
- **T5**(deps: T0,T1,T2;done: AC1,AC7):observability 事件接收 + 限流/幂等。
- **T6**(deps: T0;done: AC3,AC7):部署加固 + 多环境。
- **T7**(deps: T0,T1;done: AC6,AC7;*backlog*):后台/定时任务(Cloud Scheduler/Tasks)。
- **T8**(deps: T2;done: AC6;*backlog*):运营与商业化(Admin/Ops、权限/租户、计费)。

**owner_agent** 均为 `main_coordinator`:每站先由协调者另起 PRD session 锻造,再编排实现。

---

## 15. 附:被淘汰的候选方向(L2 留痕)

L2 阶段并行发散了 4 套候选(3 Claude 取向 + 1 Codex 异源),经 5 路批判(架构/实现/需求/红队 + Codex 对抗)综合,最终方向以「最小地基线性推进」(原 S4 异源候选)为基底,嫁接「部署链早点亮」(原 S2)与「契约纪律」(原 S3)。被淘汰的两个方向:

- **rejected:1:1 镜像客户端对端(原 S1)**。被否原因(五路共识):把服务端自身骨架/DB/config 当「隐含不做」,但这些是客户端无对端、服务端最难的部分(连接池所有权、密钥存放),不能隐含;第一个做「错误约定」是空中楼阁(无 server 壳承载,做完即在 auth 时推翻重写);「1:1 镜像」把未固化的占位 wire 当稳定契约锁死;排序无 ROI 依据(沿用客户端既有顺序)。被 S4 在「可上线、目录树、ROI 依据」上支配。
- **rejected:契约即源、地基期就投 codegen(原 S3)**。被否原因(五路共识):让 Go 新手在没写过 Go 业务代码时先写「生成 Go 的生成器」,顺序颠倒(得先懂 Go 才写得好生成器);客户端 wire 未固化时全量 codegen 是流沙上盖楼;把实现选择(codegen 优先)当需求,与 ROI 首要价值(业务快速上线)正面冲突;被 S4「fixture 先行、生成器后置」支配。**保留**其「契约纪律」(unstable→frozen 两段冻结、契约双版本轴)并入 NFR6。

部分采纳但拒绝其细节的:**S2 的「CI 远端真跑绿进 PR gate」被拒**(Neon 冷启动 + Cloud Run flaky 会拖垮单人,改为 PR gate 只跑本地、远端冒烟作独立/手动 job);**S2「P0 太胖」被拆**为 T0-T1-T2 线性序列。

---

*本产物由 design-gate-lite(协调者模式)生成,仅做 PRD 锻造,未写任何实现代码。下游落地由明博显式授权后另起 session。*
