# PRD: T3 契约对账(全双向 · 服务端持 schema 真相,两端各自 CI 校验)

> design-gate-lite L2 产物(取代同目录上一轮单向 GA1 版)。源类型:idea(路线图推进站)。Mode:L2。
> 锻造:codebase-scan + 客户端侦查 → L2 阶段1 候选发散(option-conservative/fast/longterm + Codex 异源)→ L2 阶段2 五路对抗批判(architecture/implementation/requirement/redteam + Codex)。机制 S1–S4 淘汰、S5 选定见 §12。

## 1. Summary

T3 给 auth 的 login/refresh 响应 wire(接口传出去的数据形状)建一道**全双向**防漂移防线。机制:服务端把 wire 真相从 CONTRACTS §6 的自然语言**升级**成一份机器可读 schema(JSON Schema,留在服务端 repo、纯 append),服务端 CI 校验自己真实序列化的 wire 符合这份 schema(抓服务端漂移,**自包含、不引 submodule**);客户端在它**自己的 CI** 校验其 Kotlin/Swift 解码器吃得下这份 schema(抓客户端漂移)。两端各自防漂、各在自己 CI 现形。给谁用:服务端 + 服务端 CI + 客户端 app-infra-toolkit(双向两端),直接操作者明博。为什么现在做:T2 产出第一个真实 auth DTO 当样板,是防两端漂移性价比最高的杠杆(roadmap T3),且明博明确要"防客户端悄悄漂移"。Mode = L2(目标锁定后机制有多条实质不同路 + 跨 repo 对外契约保守域 + 五路批判在"契约真相归谁"上分裂,明博拍定服务端持真相)。

## 2. Goal Alignment

A.5 目标层经历**两次锁定**:首轮明博在"单向 GA1 vs 双向 GA2"选 GA1(单向);本轮明博看到单向的固有盲区后**显式推翻、升级为 GA2 全双向**(D1)。status = **selected**。机制层(S1–S5)的"契约真相归谁"再经一道选择题,明博选定**服务端持真相(S5)**(D2)。

- **目标用户/操作者**:服务端 + 服务端 CI(防服务端漂移)+ 客户端 app-infra-toolkit 及其 CI(防客户端漂移);直接操作者明博。
- **问题/机会**:单向只防服务端自己改坏 wire,防不住"服务端和客户端契约不一致";明博要双向防漂,且不愿把服务端自己输出契约的真相所有权交出去。
- **成功结果**:服务端持有机器可读 wire schema 当契约真相,服务端 CI 校验自己 wire 符合它,客户端 CI 校验解码器符合它;任一端漂移在其自己 CI 红。
- **范围边界**:服务端侧(本 PRD)= 写 schema + 服务端自检 + §6 升级,自包含可独立落地;客户端侧 = 独立前置活(pin/校验服务端 schema),本 PRD 只声明依赖、由明博协调客户端开站;不做实时双向(pin 天花板)、不做 codegen 生成器、不把真相搬出服务端。
- **首个验收信号**:改 loginSession 一个 json struct tag(camelCase→snake_case)但不更新 schema → 服务端 conformance 测试红、go test ./... 退出码非 0。

被考虑的**目标层**备选(锁定留痕,见 §12 D1):

| id | 解读 | 成功长什么样 | 互斥 P0 |
|---|---|---|---|
| GA1 | 单向·守住服务端 | 服务端 golden 自检,客户端 spec 仅人工对照不进 CI | 客户端 spec 不进 CI 比对回路 |
| GA2(**选定**) | 全双向 | 服务端 wire 与客户端契约的漂移,任一端都在其 CI 被抓 | 客户端契约进 CI 比对回路(经 S5 落在客户端自己 CI) |

## 3. Background and Problem

- **现状**:T2 落地后 LoginSession wire `{userId, accessToken, refreshToken, expiresAt(Unix ms)}`(全 camelCase)已在 `docs/CONTRACTS.md` §6 用自然语言冻结。仓库与客户端均无机器可读 schema:服务端形状在 `internal/modules/auth/login.go` 的 `loginSession`(未导出 struct,login/refresh 共用,经 `json.NewEncoder` 输出);客户端 app-infra-toolkit 的形状在手写的 `LoginSession.kt/.swift`(两端严格解码、拒额外字段、`expiresAt` 毫秒),当前与服务端完全一致,但靠人肉对齐。
- **用户痛点**:服务端改 auth 代码可能无意改 wire(改字段/把毫秒改成秒),客户端会静默错乱,而双方 CI 全绿、无信号。单向只防服务端这一侧。
- **为什么现有不够**:`envelope_test.go` 只比服务端内部两层信封(非 wire、非跨端);sqlc drift gate 与 wire 无关。无任何机制保证服务端 wire 与客户端期望一致。

## 4. Users and Use Cases

- **主要用户**:服务端 + 服务端 CI;客户端 app-infra-toolkit 及其 CI;直接操作者明博。
- **次要用户**:后续 T4/T5 实现者(复用"生产方持 schema 真相、生产方 CI 自校、消费方在其 CI 校验"范式)。
- **用例**:
  1. 明博改 login 代码误改 `loginSession` 字段 tag → 服务端 conformance 测试红 → 提交前修正。
  2. 客户端改 Decodable 期望(把 expiresAt 当秒读)→ 客户端 CI 校验解码器不符服务端 schema → 客户端 CI 红。
  3. 明博有意 append 一个可选字段 → 改 schema + 改 wire + 改值断言,服务端 CI 绿;客户端 bump pin 同步消费新 schema。
  4. T4 manifest wire → 照搬"服务端持 manifest schema、服务端 CI 自校"范式。

## 5. Goals and Non-goals

**Goals**:
- auth login/refresh wire 有服务端持有的机器可读 schema 当契约真相;服务端漂移在服务端 CI 被自动拦截。
- 双向闭合:服务端 CI 校验自己 wire 符合 schema,客户端 CI 校验其解码器符合同一 schema。
- 契约真相**留在服务端**(生产方),CONTRACTS §6 纯 append 升级、冻结归属不破。
- 建立可被 T4/T5 照抄的"生产方持 schema、双端各自 CI 校"范式。

**Non-goals**:
- 不做实时双向(pin 天花板:跨 repo 必 pin 固定版本,客户端漂移最快在客户端 bump pin 时被发现)——见 §11 R5。
- 不把契约真相搬到客户端 repo(已淘汰 S3,见 §12)。
- 不做 spec→Go/native 生成器(roadmap 硬约束;已淘汰 S4)。
- 不在本 PRD 实现客户端侧 gate——客户端开站做,本 PRD 只声明依赖(FR9)。
- 不把错误信封纳入 T3(见 §12 D7)。
- 不改 T0–T2 已落地代码、不导出 `loginSession`。
- 不新建顶层 `contracts/` 目录(避免触 CONTRACTS §1.1 冻结集;见 §12 D3)。

## 6. Requirements

**FR1**:在服务端 repo 写一份机器可读 schema(JSON Schema 2020-12,匹配客户端 contracts/ 惯例)描述 login 响应 wire,字段名/类型/全字段 required/`additionalProperties:false` 严格;作为服务端发布的 wire 契约真相。
**FR2**:schema 覆盖 login 与 refresh 两个响应,按端点独立锚定(即便当前共用 `loginSession`),防未来分化漏检。
**FR3**:服务端 conformance 测试(`package auth` 内)用固定输入构造 DTO → 经与 handler 相同的 `json.NewEncoder` 路径序列化 → 校验输出符合 schema(字段名/类型/required/拒额外字段)。任一结构漂移导致测试失败;删除 schema 文件时测试失败(fail-closed)。
**FR4**:conformance 测试加值级判别断言,覆盖纯结构 schema 抓不到的语义漂移(`expiresAt` 用非平凡固定毫秒值,使秒↔毫秒被区分;数值字段为 number 非 string)。
**FR5**:用固定/注入的确定性输入(固定字面 token、固定 `expiresAt` 毫秒、固定 UUID),不调 CSPRNG token 或 `time.Now`,保证可重复。
**FR6**:服务端侧完全自包含——conformance 只用服务端 repo 内 schema + 自己 wire,不引 submodule、不连 DB、不起 HTTP server、不依赖 `TEST_DATABASE_URL`;被 verify.sh step 3 `go test ./...` 覆盖,不强制新增独立 step。
**FR7**:schema 落 `internal/modules/auth/contract/`(不新建顶层目录、不触 §1.1 冻结集);conformance 测试在 `package auth` 内(`loginSession` 未导出);不落 `internal/platform` 下(不违反依赖方向)。
**FR8**:`docs/CONTRACTS.md` §6 纯 append 升级——声明 wire 真相由该机器可读 schema 持有(§6 自然语言为人类摘要、schema 为机器可读规范,主从明确)、服务端 CI 校验 wire 符合 schema、客户端在其 CI 消费此 schema 校验解码器、真相源**留在服务端**;为 append 不需 migration note。
**FR9**(跨 repo 依赖声明,本 repo 不实现):客户端 app-infra-toolkit 侧前置活——pin/消费服务端 schema + 在客户端 CI 校验其 Kotlin/Swift 解码器接受该 schema 声明的形状——作为依赖记录,owner 明博,客户端开站做。

**NFR1**:schema 严格度 ≥ 客户端解码器严格度(`additionalProperties:false` + 全 required),防"schema 比解码器宽"导致服务端 append 字段被放过却在客户端炸。
**NFR2**:服务端校验 wire 时针对 Int64 精度用 `json.Decoder.UseNumber()` 或等价手段,大整数 token/`expiresAt` 不经 float64 丢精度。
**NFR3**:以最小形态实现,不引入 codegen/生成器,不建 `internal/contracts/generated/`,不预先抽象跨模块框架(T4/T5 复用时按 rule-of-three 再提取)。
**NFR4**:范式对 T4/T5 为 append 可复用——"生产方持 schema 真相、生产方 CI 自校、消费方在其 CI 校验"。

## 7. User Flow or State Flow

**服务端漂移检测流(本 PRD,自包含)**:
1. 开发者改 auth 代码 → 本地 `bash scripts/verify.sh` / push。
2. verify.sh step 3 `go test ./...` 跑到 `package auth` conformance 测试。
3. 固定输入构造 DTO → 真实 `Encode` 序列化 → 校验符合 schema + 值级断言。
4. 符合 → 绿;漂移 → 红(退出码非 0)。

**客户端漂移检测流(客户端站,本 PRD 不实现,记为依赖)**:
1. 客户端 pin 服务端某 commit 的 schema。
2. 客户端 CI 校验其 Kotlin/Swift 解码器接受该 schema 声明的形状。
3. 客户端改 Decodable 期望不符 schema → 客户端 CI 红。

**有意变更流**:改 wire → 同步改 schema + 改值断言(服务端 CI 绿)→ 客户端 bump pin 消费新 schema。

## 8. Data, API, Permissions

- **被对账实体**:`loginSession`(未导出 struct,`internal/modules/auth`),`{userId, accessToken, refreshToken, expiresAt(Unix ms)}` camelCase,login/refresh 共用。
- **真实编码路径**:`json.NewEncoder(w).Encode(session)`(尾随 `\n` + 默认 HTML 转义)。conformance 必须走同一路径,不用 `json.Marshal`。
- **契约 artifact**:JSON Schema 2020-12,落 `internal/modules/auth/contract/login.schema.json`(及 refresh),`additionalProperties:false` + 全 required。它是服务端发布的、客户端经 submodule pin 消费的契约面(客户端读文件,不 import Go 代码)。
- **真相源关系**:CONTRACTS §6 自然语言 = 人类摘要;机器可读 schema = 机器规范;wire 真相**留在服务端**。二者主从明确,schema 由 conformance 测试用真实 encode 反向锁(schema 与 wire 冲突即测试红)。
- **跨 repo**:客户端 pin 服务端 schema 是**客户端 repo 的事**(客户端 CI 校验)。服务端侧零 submodule。
- **契约演进**:T3 纯 append(新增 contract/ schema + conformance 测试 + §6 append),不动冻结路径,不需 migration note。

## 9. Acceptance Criteria

- **AC1**:Given schema 已 committed,when 运行 `go test ./internal/modules/auth`,then conformance 测试通过且 `internal/modules/auth/contract/` 下存在 login 与 refresh 的 schema。`automated_test`;refs FR1, FR2。
- **AC2**:Given login 与 refresh 各有独立 schema 锚定,when 篡改 refresh schema 一个字段而不动 login,then refresh 的 conformance 断言失败、login 的不失败。`automated_test`;refs FR2。
- **AC3**:Given schema 已 committed,when 把 `loginSession` 某 json struct tag 从 camelCase 改成 snake_case 但不更新 schema,then `go test ./...` 退出码非 0。`automated_test`;refs FR3。
- **AC4**:Given schema 含 `additionalProperties:false`,when 给 `loginSession` append 一个未在 schema 声明的字段而不更新 schema,then conformance 测试失败(拒额外字段)。`automated_test`;refs NFR1, FR3。
- **AC5**:Given conformance 测试,when 删除 schema 文件后运行 `go test`,then 测试失败(读盘报错),而非跳过或通过。`automated_test`;refs FR3。
- **AC6**:Given `expiresAt` 用非平凡固定毫秒值,when 把 `UnixMilli` 语义改成 `Unix`(秒)而不更新断言,then 值级判别断言失败。`automated_test`;refs FR4。
- **AC7**:Given conformance 测试构造 DTO,when 连续运行 `go test -count=5`,then 五次一致、无间歇失败。`automated_test`;refs FR5。
- **AC8**:Given conformance 测试,when 在未设 `TEST_DATABASE_URL` 下运行,then 测试通过、不建立 DB 连接、不监听 HTTP 端口、不引入 submodule。`automated_test`;refs FR6。
- **AC9**:Given 测试落点,when 审查 T3 改动,then schema 在 `internal/modules/auth/contract/` 下、未新建顶层目录、测试在 `package auth` 内、未导出 `loginSession`、未落 `internal/platform` 下。`code_review`;refs FR7。
- **AC10**:Given T3 落地后的 `docs/CONTRACTS.md` §6,when 审查文档,then 含一条 append 说明:wire 真相由机器可读 schema 持有、§6 文字为人类摘要、服务端 CI 校验 wire 符合 schema、客户端在其 CI 校验解码器、真相源留在服务端(非 migration note 级变更)。`code_review`;refs FR8, NFR4。
- **AC11**:Given 跨 repo 依赖,when 审查 PRD/handoff,then 客户端前置活(pin/校验服务端 schema)被显式记为依赖项 + owner 明博,本 PRD 不实现它。`code_review`;refs FR9。
- **AC12**:Given Int64 精度,when 服务端校验 wire 的 `expiresAt` 与 token 字段,then 用 `UseNumber` 或等价手段,大整数不经 float64 丢精度(反例:不用 `UseNumber` 时大整数触发精度断言失败)。`automated_test`;refs NFR2。
- **AC13**:Given T3 实现完成,when 审查仓库依赖与目录,then 未引入 codegen/生成器、未建 `internal/contracts/generated/`、helper/schema 工具为 auth 包内局部实现未提前跨模块抽象。`code_review`;refs NFR3, NFR4。

## 10. Edge Cases and Failure States

- **E1**:schema 比客户端真实解码器宽松(忘写 `additionalProperties:false`)→ 期望:schema 严格度 ≥ 解码器,`additionalProperties:false` + 全 required(AC4)。
- **E2**:用 `json.Marshal` 而非 handler 的 `Encode` 路径校验,字节/行为差 → 期望:conformance 走与 handler 相同 `Encode` 路径。
- **E3**:Int64 大整数经 float64 丢精度误判 → 期望:`UseNumber`(AC12)。
- **E4**:单位 ms→s 形状不变 → 期望:值级判别断言(AC6);token base64 编码、null↔缺字段等残留盲区诚实标注(§11 R4)。
- **E5**:客户端 station 迟迟不开 → 期望:服务端侧自包含先落地(防服务端漂移 + 发布机器可读契约有独立价值);双向的客户端那半作为依赖挂起、不阻塞服务端;PRD 明示"客户端未开站时只有服务端单向自检生效,双向未闭合"。
- **E6**:同源风险——人改 wire 顺手同改 schema 一起漂 → 期望:conformance 用真实 encode 对 schema + 值级断言,改 wire 要蒙混需同时改 schema 且改值断言,门槛更高;诚实标注此为 schema-同作者残留同源风险(§11 R2)。
- **E7**:跨平台 checkout 行尾 CRLF → 期望:schema/样例文件经 `.gitattributes` 标 `*.json text eol=lf`。

## 11. Risks and Mitigations

- **R1**(impact medium / likelihood medium / owner Mingbo):客户端 station 不开/拖延 → 双向只闭合服务端半边,客户端漂移无 gate。缓解:服务端半边自包含先落地有独立价值;客户端半边作为依赖项明博协调;PRD 明示未闭合状态(E5)。
- **R2**(medium / medium / engineering):schema 由服务端作者手写,可能比真实 wire 宽或与 wire 同源漂移。缓解:conformance 用真实 encode 对 schema + 值级断言,改 wire 不同步 schema 即红;`additionalProperties:false` 强制(AC4)。
- **R3**(low / low / engineering):引入 `jsonschema/v6` 给极简 go.mod 扩供应链面(传递依赖)。缓解(Q2 已定用库,见 §12 D6):依赖已核实=1 直接 + 少量成熟/官方传递,只测试用不进生产二进制,在 CONTRACTS 申报;实现时 `go mod tidy` 核实传递面,超预期则按 D6 Override 退回手写断言 + 元测试。
- **R4**(medium / medium / engineering):形状 schema 对单位/编码语义失明(ms↔s、base64、null↔缺字段)。缓解:NFR2 值级判别 + 诚实标注残留盲区。
- **R5**(low / medium / Mingbo):pin 天花板——客户端校验的是 pinned 服务端 schema 版本,服务端发新 schema 而客户端不 bump pin 则客户端校旧契约。缓解:S5 把这一向放客户端 CI(bump pin 是客户端自己的纪律),服务端漂移在服务端 CI 实时抓(不受 pin 影响);残留窗口仅"服务端发新 schema→客户端 bump"之间。
- **R6**(medium / medium / engineering):T3 范式被 T4/T5 照抄,缺陷放大。缓解:S5 的"生产方持 schema、自校 + 消费方在其 CI 校"是干净范式,把同源/宽 schema 反模式写进 PRD 警示。

## 12. Default Decisions

被淘汰的机制候选(L2 留痕):**S1**(客户端样例字节比对)——客户端不产登录响应,其侧只能断言"能解开样例",双向成色最弱 + 字节比对脆;**S2**(服务端写 schema + submodule 校验客户端样例)——schema↔golden 自指半条是装饰,且引入服务端第二真相源;**S3**(客户端持真相 + 服务端 CI 校验)——把服务端自己输出契约真相搬到客户端 repo,动 §6 冻结归属、挂在客户端 v0「可破坏」schema 上、有反向盲区;**S4**(codegen 两端投影)——撞"不做生成器"硬约束,客户端 0.1.0 未发布无物可 pin、不可落地。

- **D1**:方向锁定为全双向(GA2),明博从单向 GA1 显式推翻升级。Why:明博看到单向固有盲区后要双向防漂。Override if:明博改回单向则用 GA1 版思路。
- **D2**:机制选 S5(服务端持 schema 真相 + 两端各自 CI 校验)。Why:明博在"契约真相归谁"选择题选定服务端持真相;S5 守住服务端对自己输出契约的所有权 + §6 冻结不破 + 无反向盲区 + 真双向。Override if:明博改选把真相搬客户端(S3)或只要最省(S1)。
- **D3**:schema 落 `internal/modules/auth/contract/`,不新建顶层 `contracts/`。Why:避免触 CONTRACTS §1.1 冻结的 5 个顶层目录;§4 允许新增子目录为 append;客户端经 submodule pin 整 repo 仍可读该路径文件。Override if:明博要可见顶层 `contracts/` 提升可发现性(则走 §1.1 migration note)。
- **D4**:§6 为纯 append 升级、真相源留服务端,不走 migration note。Why:S5 不外移真相源,§6 从"自然语言"扩展为"自然语言摘要 + 指向机器可读 schema",是 append 不是迁移冻结路径。Override if:审查认为指向 schema 构成冻结项结构变更则补 migration note。
- **D5**:refresh 独立 schema 锚定 + 独立断言(即便当前共用 `loginSession`)。Why:防未来分化漏检,成本近零。Override if:确认 refresh 永久复用 login DTO。
- **D6**(明博 Q2 拍板:用现成库):服务端 conformance 用成熟库 `santhosh-tekuri/jsonschema/v6`(v6.0.2,2025-05 维护中、支持 draft 2020-12、~140 项目在用)加载 schema 文件校验真实 wire,而非手写镜像断言。依赖成本(已核实 go.mod):主 module 现仅 pgx+godotenv+x/crypto,新增 1 直接依赖 `jsonschema/v6` + 其传递依赖(`golang.org/x/text` 必然;pattern 验证可能引 `dlclark/regexp2`),均成熟/官方;只在 conformance 测试引用,不进生产二进制,但进 go.mod/go.sum——在 CONTRACTS 申报。Why:S5 已强制 schema 文件作为跨语言契约真相存在,用库让服务端自检直接吃这份文件 = 单一真相零漂移,兑现 S5"服务端持真相";手写断言会制造第二份真相(schema 文件 vs Go 断言)有漂移风险。Override if:实现时 `go mod tidy` 显示传递依赖面超预期(拖入非官方重依赖),退回手写字段集 + 类型断言 + 元测试断言其与 schema 等价。
- **D7**:错误信封不纳入 T3。Why:已有 `envelope_test` 跨层比对 + §4 append-only 管住。Override if:客户端开始依赖信封 wire 形状。
- **D8**:schema/helper 为 auth 包内最小局部实现,不预抽象跨模块框架。Why:rule-of-three,T4/T5 真复用再提取。Override if:T4 落地时提取共享。
- **D9**:固定样本值用明显占位字面量(固定 UUID/字面 token/固定毫秒 `expiresAt`),暂不强制对齐客户端 magic 值。Why:协调者可给默认;对齐是优化非阻塞。Override if:客户端已有占位约定。
- **D10**(明博 Q1 拍板:等 schema 落地再写客户端交接):服务端 schema 落地前不写客户端交接物(现在写只能是提纲,schema 字段/路径/pin 纪律未定)。落地后产出一份准确的「客户端交接说明」(schema 路径 / pin 方式 / bump 纪律 / 客户端如何用其 CI 校验解码器),并由客户端仓库拿真 schema + 本 PRD/handoff 自跑 design-gate 决定开站。Why:明博在 Q1 选"等 schema 落地再写",避免提纲版返工。Override if:客户端团队需提前占位对齐,则先出提纲版交接。

## 13. Open Questions

两个原实现层 open question 均经 AskUserQuestion 由明博拍板,转 §12 Default Decisions 留痕,本节无剩余未决项:

- ~~Q1:客户端何时/是否开站做客户端半边~~ → 明博定:服务端 schema 落地后再写客户端交接说明(不现在写提纲),客户端仓库拿真 schema 自跑 design-gate;见 §12 D10 + FR9 + §14 T6。
- ~~Q2:是否引入 JSON Schema 验证器依赖~~ → 明博定:用现成库 `santhosh-tekuri/jsonschema/v6`;见 §12 D6。

## 14. Implementation Tasks

- **T1**(implementer;deps 无;done AC1, AC3, AC5):写 login 机器可读 schema(JSON Schema 2020-12,`additionalProperties:false`,全 required)放 `internal/modules/auth/contract/` + 服务端 conformance 测试(`package auth`,固定输入,真实 `Encode` 路径,经 `santhosh-tekuri/jsonschema/v6` 加载 schema 校验 wire 符合 schema(见 D6),删 schema fail-closed)+ `.gitattributes` 标 `*.json eol=lf`。
- **T2**(implementer;deps T1;done AC2, AC4):refresh 独立 schema + 独立断言 + `additionalProperties:false` 拒额外字段断言。
- **T3**(implementer;deps T1;done AC6, AC7, AC12):值级判别断言(ms↔s 反例)+ 确定性固定输入 + `UseNumber` 防 Int64 丢精度。
- **T4**(implementer;deps T1;done AC8):保证服务端侧自包含(无 submodule/DB/HTTP,不依赖 `TEST_DATABASE_URL`),被 verify.sh step 3 覆盖。
- **T5**(implementer;deps T1;done AC10):`docs/CONTRACTS.md` §6 纯 append 升级 + 目录树登记 `contract/` 落点。
- **T6**(main_coordinator;deps 无;done AC11):把客户端前置活(pin/校验服务端 schema)记为跨 repo 依赖项 + owner 明博,明确本 PRD 不实现;服务端 schema 落地后产出客户端交接说明(见 D10)。
- **T7**(reviewer;deps T1, T2, T3, T4, T5;done AC9, AC13):审查落点/依赖方向/未导出符号未导出/无 codegen/无 generated 目录/无跨模块过早抽象。
