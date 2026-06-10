# PRD: T5 事件接收(接缝先行 · 服务端自包含,公网暴露后置)

> design-gate-lite L1 产物。源类型:idea(roadmap 推进站,见 `product-requirements/server-infra-roadmap/prd.md` §7.1 T5 行)。Mode:L1。
> 锻造:codebase-scan + 五件套批判(requirement / redteam / implementation / architecture)。认证/暴露策略经 AskUserQuestion 由明博拍板(D2),其余开放项由协调者定 default。

## 1. Summary

T5 给 server-infra-toolkit 新增 observability 事件接收模块的**服务端半边**:`POST /v1/events` 批量接收 Envelope(事件信封)数组 → 入站 JSON Schema 校验 → 按事件级稳定 id 幂等去重 → 单事务批量落 Neon 新表。账单硬天花板放在 DB / 请求体侧(请求体大小 + 单批条数 + 保留周期),独立于 `max-instances`;限流与认证做成**接缝**(挂点就位,但端点默认挂在 feature flag 后、不公网暴露)。给谁用:服务端 + 服务端 CI(收-验-存-幂等链路自检),客户端 app-infra-toolkit 为后续接入方,直接操作者明博。为什么现在做:roadmap 按 ROI 把 T5 排在 T4 前(服务端自包含可完整落地、不造挂起半边、T3 schema 范式趁热复用)。Mode = L1(方向清楚,但触及认证/权限/数据模型/迁移等保守域,缺验收与数据细节)。

## 2. Goal Alignment

目标层由 roadmap §7.1 直接锁定(`status = clear`),无歧义、不触发目标对齐 gate。机制层有多条开放决策,唯一 Mingbo-owned 的认证/暴露策略经 AskUserQuestion 拍板(D2),其余走 default。

- **目标用户/操作者**:服务端 + 服务端 CI(收-验-存-幂等链路自检);客户端 app-infra-toolkit 为后续接入方;直接操作者明博。
- **问题/机会**:客户端要把 observability 事件批量上报到服务端,但当前无接收端点、无入站校验、无防滥用/防重/保留机制;且客户端 0.1.0 未发布,现在就暴露公网写库端点是**无正当流量的攻击面**。
- **成功结果**:服务端事件接收模块完整落地(批量端点 + 入站 schema 校验 + 幂等 + 限流接缝 + 账单硬上限 + 保留周期字段),集成测试覆盖收-验-存-幂等;端点默认不挂公网(feature flag 后置),认证/限流最后一公里待客户端接入定。
- **范围边界**:服务端侧 = 收-验-存-幂等-硬上限-保留字段 + 入站契约 schema + 客户端接入交接,自包含可独立落地;不做公网暴露/真实认证(待客户端接入)、查询/可视化/告警、外部队列/流处理/事件总线、保留清理执行(划 T7)、客户端实现。
- **首个验收信号**:给批量端点发一个含越闭集枚举 `AttributeValue` 的 Envelope 批次 → 服务端经真实 handler 校验路径返回 4xx 标准错误信封且该批零事件落库。

## 3. Background and Problem

- **现状**:T0–T3 已落地。`internal/modules/auth/` 是唯一业务模块先例(`Handler{db}` + `NewHandler(pool)` + `RegisterRoutes(mux)`,经 `NewServer(..., registrars ...)` 的 registrar-callback 挂接,`cmd/api/main.go` 唯一 wiring)。无 observability 模块、无事件表、无幂等/保留机制——T5 在成熟模块模板上做 greenfield。
- **用户痛点**:客户端产生 observability 事件需批量上报,服务端没有接收面;一旦草率开公网写库端点,公开仓库 + 公网可达 + scale-to-zero 按量付费的组合下,会被灌库放大账单。
- **为什么现有不够**:`RateLimiter` facade 是 `auth` 包私有、no-op、shape 未冻结(注释明示可重塑),不是现成的通用限流;客户端 Envelope wire 形状不在 repo(只有散文线索:Envelope model、`AttributeValue` 闭集枚举、hold-and-retry、traceId),需服务端定「接受形状」schema。

## 4. Users and Use Cases

- **主要用户**:服务端 + 服务端 CI(收-验-存-幂等链路自检)。
- **次要用户**:客户端 app-infra-toolkit 及其实现者(后续接入);后续 T7(保留清理)、T4(下载端点复用限流接缝)。
- **用例**:
  1. 客户端批量上报一批 Envelope → 服务端校验通过 → 幂等去重 → 落库,返回 accepted/duplicate/rejected 计数。
  2. 客户端 hold-and-retry 重发同一批 → 服务端按 `(source,event_id)` 去重,事件行数不变。
  3. 攻击者发超大/超多/畸形批 → 在读取早期被请求体/条数/schema 硬上限拒绝,零落库。
  4. 明博本地 `go test` / CI `verify.sh` → conformance + 幂等 + 批量事务在自包含测试里现形,wire 漂移在 CI 红。
  5. 客户端开站实现者拿 client-handoff → 据端点 + schema + 上限 + 幂等键约定评估接入。

## 5. Goals and Non-goals

**Goals**:
- observability 事件接收链路服务端侧完整:批量端点收 Envelope 数组 → 入站 schema 校验 → 幂等去重 → 落 Neon 新表。
- 防滥用/成本硬天花板就位:请求体大小 + 单批条数 + 保留周期三道硬上限,独立于实例数。
- T3 范式正确投影到入站方向:服务端持「接受形状」schema,conformance 测 handler 拒绝畸形输入(非自校响应)。
- 接缝先行:限流 facade + 认证挂点 + 保留清理触发点就位但不实装公网暴露,等客户端接入翻开。
- 客户端接入交接物产出(端点 + Envelope schema + 批量上限 + 幂等键约定 + 待定认证)。

**Non-goals**(临时性的标后续触发):
- 不公网暴露端点 / 不实装真实认证——待客户端 0.1.0 接入时定(本 PRD 留 feature flag + 认证挂点)。
- 不实现保留清理执行——划 T7(本 PRD 只建保留周期字段 + 时间友好表结构,让 T7 清理是 O(1) drop)。
- 不做事件查询/检索/可视化/告警——只接收落库,不提供读端。
- 不引入外部消息队列(Kafka/PubSub/SQS)/流处理/通用事件总线/订阅分发/dead-letter/重投。
- 不做多租户隔离。
- 不实现客户端侧上报代码(跨 repo,客户端开站做)。
- 不把 T5 Envelope/批量 wire 纳入 CONTRACTS frozen 集——客户端未发布,wire 暂 unstable,真实消费后才冻结。
- 不接收 PII/自由文本属性——`AttributeValue` 闭集枚举不含自由文本/用户标识字段。

## 6. Requirements

**FR1**:批量接收端点 `POST /v1/events`(`/v1/` 业务前缀下),接收 Envelope JSON 数组;端点经 registrar-callback 挂接,默认在 feature flag(env)后,未启用时不挂公网路由。
**FR2**:入站 schema 校验——用 `internal/modules/observability/contract/` 下的机器可读 JSON Schema(draft 2020-12,`additionalProperties:false`,必填字段 `required`,`AttributeValue` 闭集枚举)校验每条 Envelope,handler 真实校验路径拒绝畸形 Envelope。
**FR3**:幂等去重——按事件级稳定 id 的 `(source,event_id)` 复合唯一约束去重,客户端 hold-and-retry 重发同批不产生重复落库;重复事件静默跳过(`ON CONFLICT DO NOTHING`)。
**FR4**:批量落库——有效事件单事务批量插入 Neon 新表(`00003_` 迁移 + sqlc 生成 query),尊重连接池上限,单请求不按事件数 fan-out 连接。
**FR5**:账单硬上限(独立于实例数)——`MaxBytesReader` 限请求体大小 + 单批最大条数 + 单事件/字段最大长度,三道硬天花板在读取早期拒绝(413/400),不靠 `max-instances` 兜底存储/写放大账单。
**FR6**:保留周期字段就位——事件表带 `received_at` + 时间友好结构(便于 T7 按时间 drop),保留周期作为字段/约定定义;清理执行划 T7,本 PRD 不实现 DELETE 路径。
**FR7**:限流接缝——对齐 auth 的 `RateLimiter` `Allow(ctx,key)` 接口形状,T5 端点挂限流接缝(可 no-op),诚实标注进程内限流非安全边界(scale-zero 下最坏 2× + 冷启动重置);真实限流策略待公网暴露时定。
**FR8**:入站契约 conformance 测试(`package observability` 内)——正例(合法 Envelope 批经真实 handler 接受)+ 负例(缺必填/多字段/越闭集枚举/类型错经真实 handler 被拒,各一条证明 `additionalProperties`/`required`/`type`/`enum` 真的咬);删 schema 文件 fail-closed。
**FR9**:错误信封 append——T5 新增/复用错误 code(`payload_too_large` / `bad_request` / `rate_limited` / 复用 `unauthorized`),信封顶层结构 `{"error":{"code","message"},"requestId"}` 不变,CONTRACTS append T5 范围声明。
**FR10**:部分成功语义——整批 schema 校验,坏条不毒化整批的不变量钉死;响应给 accepted/duplicate/rejected 计数(不用 207 逐条,降低对未发布客户端的协议假设)。
**FR11**:客户端接入交接物——产出 client-handoff(端点路径 + Envelope schema 路径 + 批量上限 + 幂等键约定 + 待定认证),客户端据此评估接入,owner 明博,本 PRD 不实现客户端侧。
**FR12**:自身遥测走 stdout——事件接收模块自身运行日志/指标只走 slog JSON stdout(沿用 T0 范式),绝不回写 events 表,防递归/放大。

**NFR1**:scale-to-zero 兼容——不引入常驻进程/in-process cron;限流不依赖跨实例强一致;启动不碰 DB(对齐 `/livez` 契约)。
**NFR2**:自包含可测——单元测试(schema 校验/批量上限/信封/限流接缝,fake store + 注入时钟)无 `TEST_DATABASE_URL` 仍绿;幂等/批量事务语义走集成测试(`TEST_DATABASE_URL` 容器,本地 skip);被 verify.sh 覆盖。
**NFR3**:冻结契约不破——`/v1/` 业务前缀、错误信封 append-only 顶层不变、依赖方向(platform 不 import http/modules;http 不 import 本模块;本模块不 import auth)、模块本地渲染错误信封不调 `http.WriteError`。
**NFR4**:迁移纪律——`00003_` 五位零填充、Up/Down 两段、schema 单一真相源、生产前滚不回退、破坏性 Down 标 `IRREVERSIBLE`;新表 + sqlc gen 过 verify.sh 漂移 gate 与迁移 round-trip。
**NFR5**:隐私边界——`AttributeValue` 闭集枚举不含自由文本/用户标识;事件载荷视为可公开遥测,客户端侧负责脱敏;公开仓库 schema 即公开遥测字段设计,接受此。

## 7. User Flow or State Flow

**事件接收流(服务端,本 PRD)**:
1. 客户端 POST 一批 Envelope 数组到 `/v1/events`(feature flag 启用时)。
2. `MaxBytesReader` 在读取早期挡超大请求体(超限 → 413,内存不被打满)。
3. 解码 + 条数上限校验(超条数 → 413/400)。
4. 逐条 schema 校验(`AttributeValue` 闭集枚举等);任一条不符 → 整批 4xx,零落库。
5. 有效批单事务 `INSERT ... ON CONFLICT (source,event_id) DO NOTHING` 落库,重复静默跳过。
6. 返回 accepted/duplicate/rejected 计数(2xx)。

| 状态 | 行为 |
|---|---|
| 空批 | 200,accepted=0(或 400,实现期定;计数语义明确) |
| 校验失败 | 4xx `bad_request`,零落库(坏条不毒化已通过条的语义) |
| 请求体超限 | 413 `payload_too_large`,读取早期拒绝 |
| 条数超限 | 413/400,零落库 |
| 重复批(hold-and-retry) | 200,duplicate 计数,事件行数不变 |
| 限流触发 | 429 `rate_limited`(接缝;真实策略待公网暴露)|
| 未启用 flag | 端点不挂公网路由(接缝先行) |

**客户端接入流(客户端站,本 PRD 不实现)**:客户端拿 client-handoff → 实现批量上报 + hold-and-retry → 接入时与明博定认证/限流最后一公里。

**保留清理流(划 T7,本 PRD 不实现)**:T7 用 Cloud Scheduler/Tasks 外部触发,按 `received_at` drop 超期分区/行。

## 8. Data, API, Permissions

- **被接收实体**:Envelope 事件(客户端生产)。最小字段集(实现期据客户端线索细化,wire 暂 unstable):`source`(来源标识)、`event_id`(事件级稳定 id)、`traceId`、`attributes`(`AttributeValue` 闭集枚举,不含自由文本/PII)、时间戳。
- **事件表**(新 `00003_` 迁移):`events` 单表,`(source,event_id)` 唯一约束(幂等)、`received_at`(保留周期 + 时间友好结构)、payload 列(倾向 jsonb)。无指向第二张表的外键(模块自包含可独立删除)。
- **API**:`POST /v1/events`,请求 = Envelope 数组,响应 = accepted/duplicate/rejected 计数 + 标准错误信封。端点默认 feature flag 后,不公网暴露。
- **契约方向(关键,T3 范式反转)**:T3 是服务端**产**响应 wire 自校;T5 是服务端**收**客户端产的请求 wire。服务端持「接受形状」入站 schema(真相源留服务端,与 T3 归属一致),conformance 测 handler 真实**拒绝**畸形输入(非自校它从不产出的 Envelope)。
- **权限**:本 PRD 端点不实装认证(接缝先行,D2);认证挂点只在 `cmd/api`(模块不 import auth)。
- **隐私/保留**:闭集枚举挡 PII;保留周期对齐 CONTRACTS 用户数据保守域;清理划 T7。
- **迁移**:`00003_` 纯新增,前滚不回退,破坏性 Down 标 `IRREVERSIBLE`;sqlc gen 过漂移 gate。

## 9. Acceptance Criteria

- **AC1**(`automated_test`;FR2,FR8):Given 模块落地,when `go test ./internal/modules/observability`,then conformance 测试通过且 `contract/` 下存在 Envelope 入站 schema 文件。
- **AC2**(`automated_test`;FR2,FR8,FR10):Given 入站 schema 含 `additionalProperties:false` 与 `AttributeValue` 闭集枚举,when 给端点发一个含越枚举 `AttributeValue` 的 Envelope 批,then handler 经真实校验路径返回 4xx 标准错误信封且该批零事件落库。
- **AC3**(`automated_test`;FR3):Given 事件表 `(source,event_id)` 唯一约束,when 同一批含相同 `(source,event_id)` 的请求连发 N 次,then 该事件在 events 表的行数等于 1。
- **AC4**(`automated_test`;FR10,FR2):Given 批量校验,when 一批 100 条中夹 1 条违反 schema 的 Envelope,then 整批返回 4xx 且 events 表零新增、响应报告 rejected 计数(坏条不被静默吞、不毒化整批)。
- **AC5**(`automated_test`;FR5):Given 请求体硬上限,when 发送超过 `MaxBytesReader` 上限的请求体,then 在读满前返回 413 且进程内存不被打满。
- **AC6**(`automated_test`;FR5):Given 单批条数上限,when 发送条数超上限的批,then 返回 413 或 400 且零落库。
- **AC7**(`automated_test`;FR8):Given conformance 测试 fail-closed,when 删除入站 schema 文件后运行 `go test`,then 测试失败(读盘报错),而非跳过或通过。
- **AC8**(`code_review`;NFR3):Given 依赖方向,when `go list -deps ./internal/http` 与 `./internal/modules/observability`,then `internal/http` 不含 observability、observability 不含 `internal/http`、不含 `internal/modules/auth`(认证挂点只在 `cmd/api`)。
- **AC9**(`code_review`;FR1):Given 端点默认不公网暴露,when 审查 `cmd/api` 路由注册,then observability 端点挂在 feature flag(env)后、未启用时不注册公网路由,且 PRD/CONTRACTS 明示这是接缝先行的有意状态。
- **AC10**(`automated_test`;NFR2):Given 自包含测试,when 在未设 `TEST_DATABASE_URL` 下 `go test ./internal/modules/observability`,then 单元测试通过、不建 DB 连接、不监听端口;幂等/批量事务断言在 integration test 中(本地 skip,CI 容器跑)。
- **AC11**(`automated_test`;FR4,NFR4):Given 新表迁移,when verify.sh 跑 sqlc 漂移 gate 与迁移 round-trip,then `00003_` 迁移加重新生成的 `gen/` 无漂移、迁移从 version 0 round-trip 通过。
- **AC12**(`code_review`;FR9,NFR3):Given 错误信封 append,when 审查 T5 的 4xx 响应与 CONTRACTS,then 每个 4xx 的 `error.code` 属 PRD 列举枚举、信封顶层结构 git diff 无顶层 shape 改动、CONTRACTS 有 T5 范围声明。
- **AC13**(`code_review`;FR12):Given 自身遥测,when 审查事件接收 handler 路径,then 不产生对 events 表的自我遥测写入,接收统计仅以 slog JSON 出现在 stdout。
- **AC14**(`code_review`;FR11):Given 客户端接入,when 审查交接物,then client-handoff 含端点路径 + Envelope schema 路径 + 批量上限 + 幂等键约定 + 待定认证,且本 PRD 不实现客户端侧。
- **AC15**(`code_review`;NFR3):Given wire 暂不冻结,when 审查 CONTRACTS frozen 集,then 不含 T5 Envelope/批量端点 wire,PRD 显式声明 T5 对端契约处 unstable 阶段。
- **AC16**(`code_review`;FR6,NFR1):Given 保留周期字段,when 审查事件表 schema 与代码,then 表带 `received_at` 与按时间分区或排序的结构、无 DELETE/清理代码路径(清理划 T7)、PRD non-goal 明示 T5 不清理。

## 10. Edge Cases and Failure States

- **E1**:越闭集枚举的 `AttributeValue` → 整批 4xx,零落库(AC2)。
- **E2**:客户端用随机幂等键灌重复语义事件 → 幂等键用事件级稳定 id;随机键绕过则倚赖账单硬上限兜底(FR5),文档标注规范化内容哈希为更强兜底选项(D5 Override)。
- **E3**:超大单条(巨型 attribute)vs 超多条(微事件刷行)→ 请求体 + 条数 + 单字段三道独立上限(FR5/AC5/AC6),读取早期拒绝。
- **E4**:端点被灌爆后服务缩零 → 清理不依赖持续流量(划 T7 外部触发);T5 期不公网暴露故无真实灌爆面(接缝先行)。
- **E5**:内存限流在缩零/多实例下失效 → 诚实标注非安全边界,账单天花板靠 DB/请求体硬上限不靠限流(FR5/FR7)。
- **E6**:事件混入 PII/设备指纹 → schema 闭集枚举不含自由文本/用户标识(NFR5),保留周期对齐用户数据保守原则。
- **E7**:pgx `CopyFrom` 不支持 `ON CONFLICT` 与幂等冲突 → 用 `INSERT ... ON CONFLICT DO NOTHING`(单语句/数组参数),不用 CopyFrom。
- **E8**:批次部分重复(已落库事件与新事件混批)→ `ON CONFLICT` 静默跳过已存在、新事件落库,响应 accepted/duplicate 计数(AC3/FR10)。

## 11. Risks and Mitigations

- **R1**(medium/medium/Mingbo):认证模型待定,公网暴露时若选错(匿名 vs ingest token vs Bearer)影响限流/数据模型。缓解:接缝先行,暴露前不承担风险;认证挂点 + 限流接缝就位,暴露时定最后一公里(D2)。
- **R2**(medium/medium/engineering):照抄 T3 范式方向反转,照搬会写假绿测试(服务端自校它从不产出的 Envelope)。缓解:PRD 显式定为入站校验契约,conformance 负例走真实 handler 拒绝路径(D3)。
- **R3**(medium/low/engineering):成本封顶维度错位——`max-instances=2` 封 CPU 不封 Neon 存储/写放大账单。缓解:账单天花板放 DB/请求体硬上限,独立于实例数(FR5/D4)。
- **R4**(low/medium/engineering):T5 期表无界增长(清理划 T7)。缓解:暂不公网暴露=无真实流量,表增长非问题;时间友好表结构让 T7 清理 O(1);文档标增长上界。
- **R5**(medium/medium/engineering):Envelope wire 在客户端未发布时过早冻结违反 roadmap NFR6。缓解:T5 wire 标 unstable 不进 frozen 集,golden fixture 吸收,客户端消费后冻结(D9)。
- **R6**(low/low/engineering):幂等去重表/事件表自身成无界增长面。缓解:幂等键用事件表内唯一约束(非独立去重表),去重窗口=保留周期同表副产品(D5)。
- **R7**(medium/medium/Mingbo):隐私/PII 入库撞 CONTRACTS 用户数据保守域,公开仓库使遥测 schema 公开。缓解:闭集枚举挡自由文本,事件视为可公开遥测、客户端脱敏,保留周期对齐用户数据保守;D2 暴露后置降低面。

## 12. Default Decisions

- **D1**:目标方向 = T5 事件接收(roadmap §7.1 锁定),服务端自包含先行。Why:roadmap ROI 排序 + 明博本 session 在剩余三站中选定 T5 进 forge。Override if:明博改选别站。
- **D2**(明博拍板):认证/暴露策略 = 接缝先行·暂不公网暴露——服务端逻辑完整落地加集成测试,端点挂 feature flag 后默认不公网,认证/限流最后一公里待客户端接入定。Why:明博在 R3 选择题选定;客户端 0.1.0 未发布,暴露公网写库端点是无正当流量攻击面,接缝先行让该风险归零且不阻塞服务端落地。Override if:明博要现在就公网可用(则按共享 ingest token + DB 硬上限或复用 T2 Bearer 重做认证层)。
- **D3**:T3 范式方向反转纠正——T5 服务端持「接受形状」入站 schema,conformance 测 handler 拒绝畸形输入(非自校响应)。Why:T5 服务端是消费方非生产方,照抄 T3 自校会写假绿测试(测一个服务端从不产出的码路)。Override if:决定锚客户端 spec 而非服务端持入站 schema(需 roadmap Q1 跨 repo 引用能力)。
- **D4**:账单硬天花板放 DB/请求体侧(请求体 + 条数 + 保留期),限流仅软削峰且诚实标注非安全边界。Why:`max-instances` 封 CPU 不封存储/写放大账单;进程内限流在 scale-zero 下最坏 2× + 冷启动重置。Override if:把限流下沉到边缘层/网关(可归 T6 部署加固)。
- **D5**:幂等键 = 事件级稳定 id 的 `(source,event_id)` 唯一约束(事件表内,非独立去重表),去重窗口=保留周期。Why:模块自包含可独立删除;内容哈希会把合法重发(时间戳微差)误判为新事件。Override if:客户端无稳定事件 id,则退规范化内容哈希兜底。
- **D6**:保留清理执行划 T7,T5 只建保留周期字段 + 时间友好表结构。Why:scale-zero 无常驻进程;请求时惰性清理塞热路径且不保证执行,部署时清理违反「迁移只改 schema/启动不碰 DB」双契约。Override if:T5 期就需清理,则加 Cloud Scheduler 触发的 GC 端点把 T7 最小支撑就近拉入。
- **D7**:限流接缝对齐 auth 的 `Allow(ctx,key)` 形状,是否提升 platform 留实现期定;暂不强行统一三处限流策略。Why:rule-of-three;auth 锁定/T5 事件风暴/T4 下载语义不同,强统一会逼通用层背三套策略。Override if:实现期发现接口可干净共享且策略可参数化。
- **D8**:批量响应给 accepted/duplicate/rejected 计数,不用 207 逐条;坏条不毒化整批不变量钉死。Why:降低对未发布客户端的协议假设;全拒会让 hold-and-retry 无限重投含毒批。Override if:客户端发布后需逐条结果协议。
- **D9**:Envelope/批量 wire 暂不进 CONTRACTS frozen 集,标 unstable + golden fixture。Why:roadmap NFR6——对端契约先 unstable、真实消费验证后才 frozen,防过早固化错误设计。Override if:客户端发布并消费后冻结 T5 wire。
- **D10**:模块脚手架照 auth 先例(Handler/NewHandler/RegisterRoutes,registrar-callback,main 唯一 wiring,编译期 `var _` 守卫),错误信封模块本地渲染。Why:复用冻结范式,守依赖方向(platform←http←modules,模块不 import http)。Override if:无。

## 13. Open Questions

无剩余未决项。唯一 Mingbo-owned 的认证/暴露策略已由明博经 AskUserQuestion 拍板,转 §12 D2 留痕;其余开放项均由协调者定 default(D3–D10)。

## 14. Implementation Tasks

- **T1**(implementer;deps 无;done AC1, AC2, AC7, AC8, AC15):入站 Envelope schema(`internal/modules/observability/contract/`,draft 2020-12,`additionalProperties:false`,`AttributeValue` 闭集枚举)+ conformance 测试(`package observability`,正例 handler 接受 + 负例 handler 拒绝四种漂移,删 schema fail-closed)+ 模块脚手架(Handler/NewHandler/RegisterRoutes,registrar-callback,依赖方向守卫测试)。
- **T2**(implementer;deps T1;done AC3, AC11):`00003_` 迁移建 events 表(`(source,event_id)` 唯一约束、`received_at`、时间友好结构)+ sqlc 批量 upsert query(`INSERT ... ON CONFLICT DO NOTHING`)+ 过 verify.sh 漂移与 round-trip gate。
- **T3**(implementer;deps T1, T2;done AC2, AC4, AC10):handler——批量 schema 校验 → 单事务幂等落库 → accepted/duplicate/rejected 计数响应;集成测试(幂等/批量事务,`TEST_DATABASE_URL`)+ 单测(校验/信封,fake store)。
- **T4**(implementer;deps T1, T3;done AC5, AC6, AC9, AC12, AC13):账单硬上限(`MaxBytesReader` + 条数 + 字段长度,读取早期拒绝)+ 限流接缝(`Allow` 形状,可 no-op,标注非安全边界)+ feature flag 挂点(默认不公网)+ 错误信封 append code + 自身遥测走 stdout。
- **T5**(implementer;deps T2, T3;done AC16):保留周期字段 + 时间友好表结构就位,无 DELETE 代码路径(清理划 T7),non-goal 文档明示 T5 不清理。
- **T6**(implementer;deps T1, T4;done AC12, AC15):`docs/CONTRACTS.md` append T5 范围声明(新 error code、入站契约范式、wire 暂不冻结 unstable、依赖方向、模块落点)。
- **T7**(main_coordinator;deps T1, T3, T4;done AC14):客户端接入交接物 `client-handoff.md`(端点路径 + Envelope schema 路径 + 批量上限 + 幂等键约定 + 待定认证),明确本 PRD 不实现客户端侧,owner 明博。
- **T8**(reviewer;deps T1, T2, T3, T4, T5, T6;done AC8, AC9, AC13, AC15):审查——依赖方向/端点未公网暴露/自身遥测不回写 events 表/wire 未进 frozen 集/无跨模块过早抽象/错误信封 append-only。
