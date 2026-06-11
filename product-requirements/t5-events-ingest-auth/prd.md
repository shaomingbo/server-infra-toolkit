# PRD: T5 事件接收认证(/v1/events 最后一公里)

> design-gate-lite L2 产物(2026-06-11)。两阶段锻造:4 路候选发散(Claude 保守/最快/长期 + Codex 异源)→ 5 路对抗批判(architecture / implementation / requirement / redteam + Codex adversarial)。

## 1. Summary

为 `POST /v1/events` 批量事件接收端点定稿并落地"最后一公里"认证:**静态共享 ingest token,专用 header `X-Ingest-Token`,服务端环境变量只存 SHA-256 哈希(支持新旧双哈希并存以零丢失轮换),配置缺失拒绝启动(fail-closed),校验先于 body 读取**。同步修订客户端交接文档(注入规则、状态码动作表、轮换纪律)与 CONTRACTS / 部署 runbook。本认证的定位是**粗门禁**(挡偶然公网扫描 + 给客户端握手凭据 + 提供可轮换关断阀),不防客户端二进制提取;速率封顶显式划归 T6 边缘层。

## 2. Goal Alignment

- **target_user**:`app-infra-toolkit` 双端(Kotlin/Swift)客户端的事件上报链路(直接消费方);明博作为唯一运维(轮换/关断操作者)。
- **problem**:`client-handoff.md §1` 的认证标"待定",是客户端接入清单(§7)的最后一个前置依赖;不定它,客户端联调无法开工。
- **success_outcome**:认证契约定稿且服务端落地——客户端拿到确定的 header 名、凭据形态、状态码→动作表、轮换纪律;client-handoff §1 的"待定"消除。
- **scope_boundary**:服务端认证实现 + 三份文档同步;不含客户端实现、不翻 `EVENTS_INGEST_ENABLED`、不做真实限流(T6)、不做设备级凭据。
- **first_acceptance_signal**:`verify.sh` 绿,且缺 token 的 `POST /v1/events` 返回 401 标准信封。
- status: clear / confidence: high

## 3. Background and Problem

T5 站已落地收-验-存-幂等-硬上限,端点挂 `EVENTS_INGEST_ENABLED` 默认关(公网 404)。客户端调研(2026-06-11)确认:拦截器链就位(注入 header 成本=一次 map merge)、LoginManager 存在但自动刷新 Deferred、BatchUploader 的 hold-and-retry 把一切 4xx 当永久丢弃、登录前崩溃/匿名遥测是核心场景。认证方案必须同时满足:与登录态解耦、轮换不丢批、配置失误不裸露写库端点。

## 4. Users and Use Cases

- **客户端 uploader**(主):携带 ingest token 批量上报事件,包括登录前/匿名场景。
- **明博(运维)**:生成/配置/轮换 token;误配时希望服务拒绝启动而非裸奔;被滥用时希望有关断阀(改哈希 + redeploy)。
- **攻击者(反向用例)**:公网扫描器无凭据灌库 → 401 挡住;持提取凭据的定向滥用 → 本站不防(见 §11 R1 与 D7)。

## 5. Goals and Non-goals

**Goals**
- G1 认证契约定稿:header 名、凭据形态、哈希口径、状态码语义、轮换纪律全部写死。
- G2 服务端落地:校验逻辑 + fail-closed 配置守卫 + 拒绝日志,verify.sh 全绿。
- G3 客户端可开工:client-handoff 修订到客户端无需再问任何认证问题的程度。

**Non-goals**
- NG1 不做设备级凭据/enroll 端点(rejected,见 §15)。
- NG2 不做真实速率限制(T6 边缘层职责,D7)。
- NG3 不翻 `EVENTS_INGEST_ENABLED`(公网暴露时机仍由明博手动,且有 D7 前置红线)。
- NG4 不改事件 wire 形状(Envelope/批量语义不动)。
- NG5 不防客户端二进制逆向提取 token(威胁模型定位,D7)。

## 6. Requirements

**FR**
- FR1 `POST /v1/events` 要求专用 header `X-Ingest-Token: <token>`;token 为运维生成的 64 字符 hex 串(`openssl rand -hex 32`)。
- FR2 服务端校验:对 header 值的 UTF-8 字节做 SHA-256,得 hex 串后与配置哈希集做常数时间比较(`crypto/subtle`);环境变量 `EVENTS_INGEST_TOKEN_SHA256S` 存 1-2 个逗号分隔的 hex 哈希(current[,previous]),服务端不存明文。
- FR3 fail-closed 耦合校验:`EVENTS_INGEST_ENABLED=true` 且哈希列表为空/缺失 → `config.Load()` 返回错误,进程退出不监听端口。校验逻辑落在 `config.Load()`(唯一 env 读取面)。
- FR4 校验器(verifier)为 `func(http.Handler) http.Handler` 形状,在 `cmd/api` 装配层只包 observability 的路由;校验只读 header,失败时不读取请求 body;不查询 `access_tokens` 表、不 import auth 模块。
- FR5 认证失败返回 401 + 既有 `unauthorized` code(标准信封,不新增 code);缺 header 与错 token 两种情况的响应体逐字节一致。
- FR6 每次 401 拒绝打一条结构化日志 `event=ingest_auth_rejected`(含 request_id;绝不含 token 值或 header 原文)。
- FR7 `client-handoff.md` 修订:§1 认证定稿(header 名/凭据分发/哈希口径样例对);§5 动作表补 401 行(语义=有界 hold:建议至多 3 次重试 + 指数退避,超界丢弃,防无界重投风暴);注入规则(`/v1/events` 注入 `X-Ingest-Token` 且绝不注入 `Authorization`,token 源独立于 LoginManager)。
- FR8 `docs/CONTRACTS.md` T5 节 append 认证契约;`docs/DEPLOY.md` 新增 ingest token 运维节(生成命令/配置方式/轮换流程/公网翻开前置红线)。

**NFR**
- NFR1 零新增:不加表、不加迁移、不加 Go 依赖、不加新 GCP 资源。
- NFR2 observability 模块自身零改动(或仅注释级);依赖方向守卫测试保持绿。
- NFR3 威胁模型定位文档化:本认证=粗门禁;速率封顶=T6 边缘层;公网翻开 flag 的前置条件=真实限流落地(runbook 红线)。
- NFR4 token 明文零入库:repo 文件、git 历史、服务端日志、错误信息中均不出现 token 明文(env 只有哈希)。

## 7. User Flow or State Flow

```
请求 → recover → request-id → access-log → [ingest verifier(本 PRD 新增,只挂 /v1/events)]
  ├─ header 缺失/哈希不匹配 → 401 unauthorized 信封(不读 body)+ ingest_auth_rejected 日志
  └─ 匹配(current 或 previous 任一)→ 进入既有 pipeline(rate-limit seam → body 上限 → 解码 → schema → 幂等落库)
轮换流:生成新 token → env 加新哈希(双哈希并存)→ 客户端版本迁移完成 → 删旧哈希
```

## 8. Data, API, Permissions

- **数据**:无新表、无迁移。配置面新增 `EVENTS_INGEST_TOKEN_SHA256S`(哈希,非密文本体,仍按 Secret 纪律脱敏处理)。
- **API**:`POST /v1/events` 新增必需 header(wire 仍标 unstable,客户端 0.1.0 前定稿正是低成本窗口);401 进入该端点的状态码契约。
- **权限**:单一共享凭据,无用户/设备粒度;关断阀=改哈希+redeploy。

## 9. Acceptance Criteria

- **AC1** `EVENTS_INGEST_ENABLED=true` 且 `EVENTS_INGEST_TOKEN_SHA256S` 未设或为空时,`config.Load()` 返回错误且进程以非 0 退出码终止、不监听端口。(automated_test;FR3)
- **AC2** 配置 current+previous 两个哈希时,携带任一对应 token 的请求通过认证;从配置中移除 previous 后,旧 token 请求返回 401。(automated_test;FR2)
- **AC3** 缺 `X-Ingest-Token` header 与携带错误 token 的请求均返回 401 + `unauthorized` code 标准信封,且两种情况的响应体字节级一致。(automated_test;FR5)
- **AC4** 携带 2 MiB 请求体的未认证请求返回 401(非 413),且服务端未读取请求体、未执行 schema 校验与数据库写入。(automated_test;FR4)
- **AC5** 每次 401 拒绝向 stdout 输出一条 JSON 日志,`event` 字段为 `ingest_auth_rejected`、含 `request_id` 字段,且日志行内不出现 token 值或 `X-Ingest-Token` header 原文。(automated_test;FR6)
- **AC6** 认证通过后既有行为不变:`internal/modules/observability` 现有全部测试(含 limits/幂等/信封字节钉死)与 `scripts/verify.sh` 全量通过。(automated_test;NFR2)
- **AC7** verifier 类型为 `func(http.Handler) http.Handler`、定义在装配层、不 import `internal/modules/auth`、不查询 `access_tokens` 表;observability 的依赖方向守卫测试通过。(code_review;FR4/NFR2)
- **AC8** `/livez` 与 `/v1/auth/login`、`/v1/auth/refresh` 在不携带 `X-Ingest-Token` 时行为与现状一致(verifier 只作用于 `/v1/events`)。(automated_test;FR4)
- **AC9** 哈希口径锚定测试:对一个写死的样例 token,断言 `hex(sha256(utf8_bytes(token)))` 等于写死的期望哈希(防 raw-bytes 与 utf8-text 口径漂移);同一样例对写入 client-handoff §1。(automated_test;FR2/FR7)
- **AC10** `client-handoff.md` §1 含确定的 header 名、token 分发方式、哈希口径样例对、轮换纪律(previous 哈希保留至所有已发布客户端版本完成迁移);§5 动作表含 401 行且语义为有界 hold(上限次数 + 退避,超界丢弃)。(code_review;FR7)
- **AC11** `docs/CONTRACTS.md` T5 节 append 认证契约(header 名 + 401 语义 + env 变量名),错误信封顶层结构零改动;全仓 grep 无 64 字符 hex token 明文入库。(code_review;FR8/NFR4)
- **AC12** `docs/DEPLOY.md` 新增 ingest token 节:生成命令、env 配置、双哈希轮换流程、"公网翻开 `EVENTS_INGEST_ENABLED` 的前置条件=真实限流落地"红线声明。(manual_check;FR8/NFR3)

## 10. Edge Cases and Failure States

- **E1** flag 关 + 哈希已配:合法(预配置),端点仍 404,不报错。
- **E2** flag 开 + 哈希空:启动失败(AC1),绝不进入无认证开放态(rejected S2 的 fail-open)。
- **E3** 哈希列表 >2 个或含非法 hex:`config.Load()` 报错(配置语法错与缺失同级处理)。
- **E4** 轮换跨两代(current+previous 都已不含某长离线客户端的 token):401 → 客户端有界 hold 后丢弃;损失边界=该离线设备的事件,记录于 §11 R2。
- **E5** header 值含空白/大小写差异:不做规范化,字节级原样哈希比较(差异即 401)。
- **E6** 并发请求打在校验路径:无共享可变状态(哈希集启动时解析为不可变切片),无锁。
- **E7** 攻击者探测响应差异:缺 header/错 token 响应字节一致(AC3),不泄露"token 是否曾有效"。

## 11. Risks and Mitigations

- **R1** 嵌入公开 app 二进制的 token 可被提取,认证对定向滥用无效(impact: high / likelihood: medium)。mitigation:威胁模型定位写死(D7/NFR3),公网翻开前置真实限流红线(AC12),T6 边缘层承接速率封顶;单请求 1MiB/500 条硬上限仍是单请求账单天花板。owner: engineering。
- **R2** 轮换超出双哈希窗口的长离线客户端丢事件(impact: medium / likelihood: low)。mitigation:轮换纪律=previous 保留至客户端迁移完成(AC10);客户端 401 有界 hold 给恢复机会。owner: engineering。
- **R3** 双端哈希口径不一致到联调期才发现(impact: medium / likelihood: medium)。mitigation:AC9 锚定测试 + client-handoff 提供同一样例对。owner: engineering。
- **R4** 客户端把 401 实现成无界重投或静默永久丢(impact: medium / likelihood: medium)。mitigation:动作表写死有界 hold 语义(AC10),接入联调时用轮换演练用例验证。owner: engineering。

## 12. Default Decisions

- **D1** 选静态共享 token,不做设备级凭据。reason:设备级方案(候选 S3)的 enroll 端点自身是无认证 mint 面(攻击面更大)、撤销因无设备标识纸面化、需新表撞 schema 保守域、客户端需先落地 SecureStorage。override_if:客户端 SecureStorage 落地且出现真实的按设备撤销需求时,另起 PRD 演进(本方案的 verifier 是单一咽喉点,可平滑替换)。
- **D2** 专用 header `X-Ingest-Token`,不复用 `Authorization: Bearer`。reason:三路批判共振——复用 Bearer 会让客户端拦截器把 ingest token 误入自动刷新路径,服务端同一 header 两种语义易误导后续维护。override_if:客户端拦截器架构重构使 Bearer 隔离成本为零时可重议。
- **D3** env 存哈希非明文、双哈希轮换、耦合校验落 `config.Load()`。reason:服务端配置面泄露不直接得到可用 token;双哈希消除轮换丢批窗口;Load 是唯一 env 读取面。override_if:无。
- **D4** 401 复用既有 `unauthorized` code,不新增 403/forbidden。reason:新增永久语义 code 会被客户端 pin 死,锁死"认证失败可恢复"的演进路径;客户端动作由动作表(FR7)定义,不靠状态码二分猜。override_if:未来确需区分永久拒绝语义时按 append-only 加 code。
- **D5** verifier 落装配层(`cmd/api`),不进 observability 模块。reason:模块内实现需复制安全敏感比较代码(依赖守卫禁 import auth),装配层与 BearerMiddleware 同形挂载是已验证模式。override_if:出现第二个需同类认证的非 auth 端点(rule-of-three)时抽象为共享组件。
- **D6** 客户端 401 动作=有界 hold(建议至多 3 次 + 指数退避,超界丢弃)。reason:无界重试=重投风暴(Codex 对抗审),永久丢=撞登录前崩溃上报 P0;有界 hold 是两者的工程折中,参数属客户端实现域,交接文档给建议值。override_if:客户端联调实测后调参。
- **D7** 反滥用速率封顶显式划归 T6 边缘层;公网翻开 `EVENTS_INGEST_ENABLED` 的前置条件=真实限流落地。reason:redteam 共识——可提取凭据+noop 限流下,认证不构成滥用边界;不写死会被误读为"已防滥用"。override_if:明博显式接受无限流公网暴露的账单风险。
- **D8** token 生成=运维侧 `openssl rand -hex 32`,服务端无 mint 代码。reason:单一共享凭据无需服务端生成路径,少一段代码少一个面。override_if:无。

## 13. Open Questions

(无——所有决策点均给出可推翻 default,见 §12。)

## 14. Implementation Tasks

- **T1** config 扩展:`EVENTS_INGEST_TOKEN_SHA256S` 解析(1-2 个逗号分隔 hex)+ flag⇒哈希耦合校验 + 配置测试。owner: implementer / deps: none / done_when: AC1, AC9
- **T2** 装配层 ingest verifier:常数时间双哈希比较、401 信封、只挂 events 路由、先于 body 读取 + 行为测试。owner: implementer / deps: T1 / done_when: AC2, AC3, AC4, AC7, AC8
- **T3** `ingest_auth_rejected` 结构化日志 + 日志字段测试(含"不含 token"断言)。owner: implementer / deps: T2 / done_when: AC5
- **T4** 全量回归:observability/auth 既有测试 + `verify.sh`。owner: test-runner / deps: T2, T3 / done_when: AC6
- **T5** 文档三件套:client-handoff §1/§5 修订、CONTRACTS append、DEPLOY runbook 节。owner: main_coordinator / deps: T2 / done_when: AC10, AC11, AC12
- **T6** 双盲 review 闭环(reviewer + Codex 异源)+ 修复回归。owner: reviewer / deps: T1, T2, T3, T4, T5 / done_when: AC6, AC11

## 15. Rejected Directions(L2 留痕)

- **S2(静态 token + 403/forbidden + 模块内检查 + 缺密钥 dev 开放态)**:被 S1/S4 全维度支配——fail-open 撞认证保守域红线(配置失误=公网裸写库);新增永久 403 code 锁死演进;检查内联业务 handler 损失可测性;header 名未定却先锁难改的部分(风险排序倒置)。
- **S3(设备级凭据 + enroll 端点 + ingest_tokens 新表)**:四路批判全部否决——无认证 enroll 是自助发凭据的新攻击面(攻击面严格大于共享 token);"按设备撤销"无设备标识支撑、实操纸面化;"复用 auth mint 原语"被依赖守卫与 Go 可见性双重阻断;新表撞 schema/认证双保守域;强加客户端 SecureStorage 前置,而其唯一产品亮点(401 可重试)已被有界 hold 形式移植进获胜方案(D6)。记为演进路径(D1 override)而非当下方案。
- **S4 的 Bearer header 细节**(其余全部吸收):复用 `Authorization: Bearer` 在客户端有拦截器串味雷(ingest token 被误入刷新路径)、在服务端造成同 header 双语义。获胜方案吸收了 S4 的哈希存储、双哈希轮换、fail-closed 启动、读 body 前校验,仅替换 header 载体为专用 header。
- **"现在不定认证,维持 flag 关到联调时再说"**(批判路提出的缺失方向):与本次任务目标直接冲突——定契约正是为了解锁客户端接入清单开工,且定契约≠公网暴露(flag 仍关,D7 红线另行约束暴露时机)。
