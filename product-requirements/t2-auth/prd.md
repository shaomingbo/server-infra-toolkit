# PRD: T2 auth 登录/刷新(server-infra-toolkit 第三推进站)

> design-gate-lite 产物 · mode **L1** · slug `t2-auth`
> 上游:`product-requirements/server-infra-roadmap/prd.md` 的 T2 站(ROADMAP task `T2`,deps `T0,T1`)
> 前序:`t0-online-minimal-loop/prd.md`(HTTP 壳)、`t1-data-access/prd.md`(连接池/sqlc/迁移)
> 配套机器可读文件:`handoff.json`(同目录)

---

## 1. Summary

为 server-infra-toolkit(Go 服务端基础设施,独立 repo)第三推进站 T2「auth 登录/刷新」起草可落地 PRD(mode **L1**)。在 T0(HTTP 壳 / 统一错误信封 `WriteError` / 结构化日志 / 配置密钥 `config.Secret`)+ T1(连接池 `db.Pool` 的 `DBTX` 窄接口含 `Begin` 事务 / sqlc 工作流 / goose 迁移)之上,建**第一个真实业务对端**、同时是**第一个 `internal/modules/auth`**:用户名+密码登录(argon2id 哈希)→ 颁发 opaque access token + refresh token,refresh 滚动轮换续期、可撤销;Bearer 中间件保护端点(由 auth 导出、main 装配);DB 层账户失败锁定防爆破;手写 DTO + golden fixture,经客户端 `app-infra-toolkit` 真实消费验证后冻结 auth 契约,作为 T3-T5 业务对端的可复用范式。明博已在锻造前选定 login 凭据 = 用户名密码多用户(D1);协调者据「多用户密码」定威胁模型(D2:核心认证安全属性不降级,只延后规模型/合规型增强)。本 PRD 由协调者并行调度 L1 五件套(codebase-scan + requirement/redteam/implementation/architecture)+ Codex 异源批判综合而成,关键实现细节落 §12 可推翻 default。

---

## 2. Goal Alignment

- **目标用户**:客户端 `app-infra-toolkit`(消费 `POST /v1/auth/login` + `POST /v1/auth/refresh`)+ 维护者(单人开发 + AI 协作的实现者)。次要消费者:T2 落地的 implementer/reviewer/test-runner;**T3**(契约对账拿 auth 当首个真实 DTO 样板);**T4/T5**(import auth 的 Bearer 中间件保护各自端点)。
- **问题**:ROADMAP 把 T2 auth 排为 T0/T1 之后第一个真实业务对端,但只有一行表格。auth 是所有受保护业务端点的门槛(T4/T5 受 Bearer 保护、T8 权限建其上),需先展开成可落地 PRD 并走通后冻结契约立范式。
- **成功长什么样**:客户端用用户名+密码登录拿 `LoginSession{userId, accessToken, refreshToken, expiresAt}`(全 camelCase、expiresAt=Unix 毫秒),用 refreshToken 续期;密码 argon2id 哈希、refresh token 哈希存储 + 滚动轮换 + 可撤销;Bearer 中间件保护端点;基本爆破 / 用户枚举 / refresh 重放有防护;契约经客户端真实消费验证后冻结,立 T3-T5 可复用的 wire + Go 接口样板。
- **范围边界**:范围 = 纯 T2 auth 单站(用户名密码登录/刷新 + 首个 `internal/modules/auth` + Bearer 中间件 + DB 账户锁定 + 契约固化)。非目标见 §5 逐条钉死。不做 OAuth/SSO/MFA、权限角色、自助注册、密码重置、客户端、离线包/事件。
- **首个验收信号**:对 seed 用户跑 `POST /v1/auth/login` 返回 200 + `LoginSession`;错误密码 / 不存在用户均返回统一 401;refresh token 换新 access、撤销后 401;整链经连接池 + 事务 + sqlc 跑通(dockerized PG),handoff 校验通过。
- **status**:`clear` —— 目标层清楚(ROADMAP §8 直接点明端点 + 数据形态 `LoginSession{userId,accessToken,refreshToken,expiresAt}`(W1 核实)+ 安全约束)。唯一根本空白「login 用什么凭据」是实现层 Mingbo-owned 决策,由明博在锻造前选定(记 D1),不属目标层歧义,故 A.5 目标对齐 gate 不触发。
- **confidence**:high

---

## 3. Background and Problem

T0(`t0-online-minimal-loop`)已把 server 带到「能真实部署的最小骨架」:HTTP 壳(`cmd/api/main.go` 含 graceful shutdown)、统一错误信封(`internal/http/errors.go` 的 `WriteError` 单一出口)、结构化日志(`internal/platform/log`,slog JSON)、配置密钥(`internal/platform/config` 的 `Config{Port,Version,DSN}` + `Secret` 三重脱敏)。T1(`t1-data-access`)已把数据接入地基建出:pgxpool 连接池(`internal/platform/db`)、`DBTX` 窄接口(`Exec/Query/QueryRow/Begin`)、双缩零重试、sqlc 工作流(`internal/platform/db/gen`)、goose 迁移规范(`db/migrations`)。

**T2 起点(codebase-scan 核实)**:

- **零 auth 脚手架**:`internal/modules/` 目录不存在(ROADMAP §7.3 已规划 `internal/modules/auth/`,T0/T1 未建),T2 首建。Bearer / 限流 / token / login 无任何实现代码。
- **客户端 auth wire 契约未在本 repo 落地**:`docs/CONTRACTS.md` 只冻结了通用错误信封 `{"error":{"code","message"},"requestId"}` 和版本语义(`/v1/` 仅业务 API、探针无前缀),没有任何 login/refresh 请求/响应/错误码定义。
- **login 输入凭据完全未定义**:全 repo + 全 spec grep(`password|credential|username|apikey|device|magic|anonymous`)命中 0。客户端只固化 login 的**输出**(`LoginSession{userId,accessToken,refreshToken,expiresAt}`,全 camelCase、expiresAt Unix 毫秒,Decodable 全字段必填严格解析、401 拒绝,W1 核实)和**失败语义**,没固化**输入**。明博本 session 选定 = 用户名密码多用户(D1)。
- **T1 接缝预留**:`apphttp.DB` 窄接口含 `Begin`,`pool.go` 注释明示 `Begin` 早预留给「T2 写 refresh token 事务」;但 `server.go:40` 当前 `_ = db` 把注入的 db 丢弃,且 sqlc 生成的 `dbgen.DBTX` **不含 Begin**(只 `pool.go` 的 `DBTX` 有)—— 数据访问接缝尚未真正打通(见 D9/R3)。

**为什么 auth 先于其他对端**:ROADMAP ROI 排序——auth 是几乎所有业务门槛 + 解锁客户端登录,走通后冻结契约立可复用范式(T4/T5 受 Bearer 保护、T3 拿 auth DTO 当对账样板)。

---

## 4. Users and Use Cases

- **主要用户**:客户端 `app-infra-toolkit`(发 login/refresh 请求、带 Bearer 访问受保护端点)。
- **次要用户**:明博(实现者 + 运维:seed 用户、管理/撤销会话);T2 落地的 implementer/reviewer/test-runner;T3(首个真实 DTO 对账样板);T4/T5(import auth 中间件)。
- 用例:
  1. 客户端用用户名+密码登录 → 拿 `LoginSession{userId,accessToken,refreshToken,expiresAt}`。
  2. 客户端 access token 将过期 → 用 refresh token 换新 access(滚动轮换)。
  3. 客户端带 Bearer access token 访问受保护端点 → 中间件验证放行 / 401。
  4. 明博撤销某会话 → 该 refresh token 后续 refresh 返回 401、其 access token 在 TTL 内即失效。
  5. 攻击者爆破登录 → 账户失败计数累积达阈值 → 锁定退避(跨实例)。
  6. 明博 seed / 预置一个用户(算 argon2id 哈希,口令注入非硬编码)。
  7. T4/T5 `import internal/modules/auth` 的中间件构造器保护自己的端点。

---

## 5. Goals and Non-goals

**Goals**(见 `handoff.goals`,价值导向):

- **G1**:客户端能用户名+密码登录拿 token、refresh 续期(首个真实业务闭环)。*价值*:解锁客户端登录链路。
- **G2**:token 安全——密码 argon2id 哈希、refresh token 哈希存储 + 滚动轮换 + 可撤销。*价值*:降低凭据泄露与重放风险。
- **G3**:Bearer 中间件保护端点,作 T4/T5 复用范式(auth 导出 Go 接口面)。*价值*:后续受保护端点不各搭一套认证。
- **G4**:基本爆破 / 用户枚举 / refresh 重放防护(DB 层账户锁定 + 恒定工作量响应 + token family 重放检测)。*价值*:多用户密码登录的安全底线。
- **G5**:auth 契约经客户端真实消费验证后冻结,立可复用的 wire + Go 接口样板。*价值*:防两端漂移、给 T3-T5 立模板。
- **G6**:首个 `internal/modules/auth` 立模块化单体范式(依赖方向、DB 接缝、错误信封复用)。*价值*:T4/T5 照抄不踩坑。

**Non-goals**(逐条钉死防超做与串味):

- **NG1**:不做 OAuth / 第三方 IdP / SSO(纯本地用户名密码)。
- **NG2**:不做完整 codegen(手写 DTO + golden,spec→Go 生成器排 **T3**)。
- **NG3**:不做 MFA / 邮箱验证。
- **NG4**:不做权限 / 角色 / 多租户(属 **T8**)。
- **NG5**:不碰客户端 `app-infra-toolkit`。
- **NG6**:不做离线包 / 事件接收(T4/T5)。
- **NG7**:不做自助注册端点(用 seed 预置;注册 defer backlog,触发条件:需对外开放多用户自助注册)。
- **NG8**:不做密码重置 / 账户恢复(defer backlog;无邮箱通道,与 NG3 一致)。
- **NG9**:不做 token 窃取后的传输层重放防护(设备绑定 / 指纹)——设备绑定属规模型增强、T2 defer(D2);token 走 TLS,重放靠 refresh 轮换检测。
- **NG10**:不做分布式限流(Redis / 网关),且 T2 通用进程内限流也不实装——只留可替换 `RateLimiter` 接缝;账单由 Cloud Run `max-instances=2` 封顶,跨实例防爆破靠 DB 账户锁定(D5)。

---

## 6. Requirements

### 功能性(FR)

- **FR1 — login 端点**:`POST /v1/auth/login`,请求体 `{username, password}` 严格 JSON 解析(NFR8)。验证 users 表 + argon2id 密码校验。成功返回 `LoginSession{userId:string, accessToken:string, refreshToken:string, expiresAt:int64}`(200,全 camelCase,expiresAt=Unix 毫秒绝对时刻;字段名与客户端逐字一致 D13);失败统一 401,**不区分**「用户不存在」与「密码错」。
- **FR2 — 密码哈希**:用户密码用 argon2id 哈希存储,不存明文。参数(memory/time/parallelism)实现时查当前 OWASP 推荐 + 本地校准(NFR3),哈希串自带参数(PHC 格式,支持登录时 rehash 升级)。哈希依赖 = `golang.org/x/crypto/argon2` 自写 PHC(D14);salt 16B/key 32B 出处 RFC 9106、参数实现时上 OWASP 复核 + 本地校准(NFR3)。
- **FR3 — access token**:opaque 随机串(crypto/rand,≥256 bit,base64url 无填充),落 DB,Bearer 中间件每请求查 DB 验证(撤销天然支持)。短 TTL。**禁止接受 client 提供的 token 值**(防会话固定);每次登录签发全新 token。
- **FR4 — refresh token**:opaque 随机串,split-token 存储(public selector 查行 + secret verifier 哈希存 DB,查后常量时间比对)。`POST /v1/auth/refresh` 用 refresh token 换新 access,**滚动轮换**:发新 refresh + 废旧 + 同 token family;旧 refresh 重用 → 撤销整个 family。撤销后返回 401。验证 + 轮换在 T1 `Begin` 事务内 + 行锁(`SELECT ... FOR UPDATE`)防并发双花。
- **FR5 — Bearer 中间件**:受保护端点验 Bearer access token(查 DB),失败 401。中间件由 **auth 模块导出构造器** `func(http.Handler) http.Handler`,在 `cmd/api/main.go` 装配进链,位置 nest 在 request-id 与 access-log **之内**(每个 401 有 requestId 且进访问日志供审计),**不包住 `/livez`**。
- **FR6 — 错误信封**:401(`unauthorized`)、429(`rate_limited`)经 `WriteError` 新增 code slug 值,不改信封顶层结构,带 requestId、`Content-Type: application/json`。429 是否带 `Retry-After` 在契约明示。T2 新增 slug 集合一并纳入契约冻结。
- **FR7 — schema + 迁移**:goose `00002` 建 users 表(`id / username(unique, 大小写归一) / password_hash / status / failed_attempts / locked_until / 时间戳`)、refresh token 表(`selector / verifier_hash / token_family / user_id / expires_at / revoked_at`)、access token 存储(opaque 查找 + `expires_at / revoked_at`)。sqlc 类型安全访问。refresh 写入用 `Begin` 事务。users 最小固定字段集先定,注册特有字段(如 email)defer 到 FR9 关闭后追加迁移。
- **FR8 — 防爆破分层**:① **DB 层账户失败计数 + 锁定**(失败 N 次锁 M 分钟,状态落 users 表,跨实例生效)= **唯一主防线**;② **argon2id 前置廉价拦截**(IP/账户失败计数先于哈希计算,防 CPU/内存 DoS 放大)。命中返回 429,阈值/退避数值查当前推荐(NFR3)。**通用限流不属安全防线**:抽成可替换 `RateLimiter` facade,**T2 最小版不实装**(no-op 空接缝)——账单已由 Cloud Run `max-instances=2` 封顶(D5),仅当出现明确账单尖峰才经 facade 接成熟库 `golang.org/x/time/rate`(不自研,硬规则③),换网关/Redis 经同一 facade(D5)。
- **FR9 — 用户来源**:seed 预置——一条 goose migration 或 cmd 子命令(如 `-seed`)算 argon2id 哈希插入用户,seed 口令从 Secret Manager / env 注入,**非默认值、非明文、不硬编码进 migration/git**。自助注册端点 defer(NG7)。
- **FR10 — 用户枚举 / 恒定工作量**:login 失败统一 401 + 通用 message;用户不存在时也执行一次等价成本 argon2id(对固定假 hash);畸形 JSON / 缺字段 / 错凭据 / 不存在 四类输入响应状态码 + message 一致。
- **FR11 — 凭据脱敏**:密码 / token 在内存用 `config.Secret` 风格包装类型(`String()/LogValue()/MarshalJSON()` redact);禁记 request body;审 panic/recover 路径不携带凭据。
- **FR12 — 契约固化**:手写 login/refresh DTO + golden fixture,契约写进 `CONTRACTS.md` 标 `unstable`;经客户端 `app-infra-toolkit` 真实消费 login+refresh 链路验证通过(且 D3/D4 关闭;wire 契约形态已据 W1 客户端核实锁定 D13)后才标 `frozen`。给可冻结 checklist;golden 可复用客户端 `fixtures/token-cases.json`。
- **FR13 — 用户/会话状态联动**:refresh 与 Bearer 验证除查 token 有效,也查 user status(禁用用户的 token 立即失效)。
- **FR14 — auth 模块导出 Go 接口面**:`internal/modules/auth` 导出 T4/T5 复用符号(Bearer 中间件构造器 + token 校验器接口),声明为模块间复用面;auth 在**消费侧声明自己的 DB 窄接口**(不 import `internal/http` 的 `DB`,避免 modules→http 耦合,接口取自身声明或 `internal/platform/db` 的 `DBTX`),refresh 事务用 `db.Begin` → `dbgen.New(tx).WithTx`。

### 非功能(NFR)

- **NFR1 — token/密钥安全**:refresh 哈希存储 + 撤销;access opaque 查 DB;密码 argon2id;若引入任何签名/对称密钥进 Secret Manager(复用 `config.Secret`)不进 env 明文。
- **NFR2 — 包依赖方向**:`internal/modules/auth` 不被 `internal/platform` import;`internal/http` 不 import `internal/modules`(verify.sh CI 强制,modules 建后真生效);`internal/modules/auth` 不 import `internal/http`。具体类型 `*pgxpool.Pool` 不穿 auth 边界(继承 T1)。
- **NFR3 — 训练数据时效性闸门**:argon2id 参数 / token 长度 / 哈希算法 / 账户锁定阈值,实现时查当前 OWASP / 官方文档,不写死凭记忆的魔数。
- **NFR4 — 可观测**:auth 事件(登录成功/失败、refresh、撤销、锁定、限流命中)走 T0 slog JSON,字段含可溯源(`user_id` 或 username hash、来源 IP、requestId、时间),**绝不含明文密码/token/Authorization 头/request body**;不新增 metrics 端点。T2 不建独立 audit 持久化(defer,触发:合规要求);Cloud Logging 保留期内可查。
- **NFR5 — 版本化**:auth 端点 `/v1/` 前缀(首次真实使用),探针仍无前缀。
- **NFR6 — livez 不探 DB**:继承 T1;Bearer 中间件不包住 `/livez`,`/livez` 不发起任何 DB 调用。
- **NFR7 — expiresAt 双端对齐**:expiresAt = **Unix 毫秒绝对时刻 int64**(W1 核实客户端 `LoginSession.expiresAt` Int64/Long + `fixtures/token-cases.json` 全毫秒实证,D13),写进 `CONTRACTS.md`;golden fixture 体现具体毫秒值。
- **NFR8 — 严格请求解析**:login/refresh 请求体 body size cap(`http.MaxBytesReader`)、拒绝未知字段(`json.Decoder.DisallowUnknownFields`)、类型错配/越界 → 400 不 panic。
- **NFR9 — 威胁模型边界**:T2 **核心认证安全属性不降级**(密码哈希存储/校验、access+refresh token 生命周期/轮换/撤销/重放检测、登录失败处理 + 用户枚举防护、会话/权限边界、安全事件最小可追溯),**只延后规模型/合规型增强**(分布式限流、设备绑定/指纹、独立持久化审计表、通用进程内限流实装);CSRF 仅当客户端是浏览器才纳入。边界显式记录,可推翻(D2)。

---

## 7. User Flow / State Flow

### 7.1 登录流

```
POST /v1/auth/login {username, password}
  → 中间件链 recover → request-id → access-log
  → auth handler:严格解析 body(NFR8)→ username 归一查 users
      → argon2id 校验(不存在用户走 dummy hash,恒定工作量 FR10)
      → 检查账户锁定状态(FR8)
          ├ 成功 → 事务内创建 access+refresh token → LoginSession{userId,accessToken,refreshToken,expiresAt} 200
          ├ 失败 → 失败计数+1、统一 401 通用 message → 达阈值则锁定
          └ 锁定中 → 429(或统一 401)
```

### 7.2 刷新流

```
POST /v1/auth/refresh {refreshToken}
  → Begin 事务 + 行锁(SELECT ... FOR UPDATE)
      → split selector 查行 → 常量时间比对 verifier → 检查 未撤销/未过期/user status
          ├ 有效 → 轮换(发新 refresh + 废旧 + 同 family)+ 新 access → 新 LoginSession 200
          ├ 旧/已轮换 refresh 重用 → 撤销整个 token family → 401
          └ 撤销/过期/用户禁用 → 401
```

### 7.3 受保护端点 + 状态表

`Bearer access token → 中间件查 DB → 有效放行 / 401`。`/livez` 永不走 auth。

| 状态 | 触发 | 响应 |
|---|---|---|
| 登录成功 | 凭据正确 + 账户未锁 | 200 LoginSession |
| 凭据无效 | 密码错 / 用户不存在 / 畸形 body | 统一 401(或 400),message 一致 |
| 账户锁定 | 失败累计达阈值 | 429 / 401(不泄露锁定细节) |
| refresh 成功 | refresh 有效未撤销 | 200 新 LoginSession + 轮换 |
| refresh 重放 | 旧 token 复用 | 401 + 撤销整个 family |
| 撤销/过期/禁用 | token 失效 | 401 |
| 受保护访问无/坏 token | 缺 Bearer / 无效 / 过期 | 401 unauthorized |

---

## 8. Data, API, Permissions

### Data(entities,schema 单一真相源 = `db/migrations`,sqlc 从它读)

- **users**:`id, username (unique, 大小写归一), password_hash (argon2id PHC), status (active/disabled), failed_attempts, locked_until, created_at, updated_at`
- **refresh_tokens**:`id, user_id, selector (public, indexed), verifier_hash, token_family, expires_at, revoked_at, created_at`
- **access_tokens / sessions**:`id, user_id, token 查找(opaque value 或其哈希), expires_at, revoked_at`

### API

- `POST /v1/auth/login`(请求 `{username,password}`)→ 200 `LoginSession{userId,accessToken,refreshToken,expiresAt(Unix 毫秒)}` / 401 / 400 / 429
- `POST /v1/auth/refresh`(请求 `{refreshToken}` camelCase)→ 200 完整 `LoginSession`(轮换后含新 refreshToken)/ 401 / 400 / 429
- 受保护端点(T4/T5 提供)经 Bearer 中间件;`/livez` 不变(无 `/v1/`、不探 DB)

### Permissions / Secrets

- 复用 `NEON_DSN`(T1)。seed 口令经 Secret Manager / env 注入(FR9)。若引入 token 签名密钥进 Secret Manager(NFR1)。
- 客户端靠 status code 归一 ErrorCode(T0 §8 证据):全 401 + 通用 message,客户端 refresh 失败才升级到重新登录;若客户端需区分过期 vs 撤销,经 message 或新增 code slug 体现(append-only)。
- **audit/retention**:NFR4(slog 带可溯源字段,无独立 audit 表,defer)。

---

## 9. Acceptance Criteria

> 每条引用存在的 FR/NFR,description 不含模糊词黑名单。本表 AC 编号为 T2 PRD 独立命名空间。

| ID | 验收标准 | 验收方式 | 关联 |
|---|---|---|---|
| **AC1** | 对 seed 用户用正确用户名+密码 `POST /v1/auth/login` 返回 200,body 含 `userId`/`accessToken`/`refreshToken`(均 JSON 字符串)+ `expiresAt`(int64,Unix 毫秒);`accessToken` 解码后字节长度 ≥32;字段名与客户端 `LoginSession` 逐字一致(camelCase)。 | automated_test | FR1, FR3, FR9 |
| **AC2** | 错误密码、不存在用户、畸形 JSON、缺字段四类输入,同类响应的 status code 与 `error.message` 逐字节一致,不泄露用户是否存在。 | automated_test | FR1, FR10 |
| **AC3** | users 表 `password_hash` 列不含明文;同一密码两次 seed 哈希不相等(salt 不同);改一字符校验失败;哈希串自带参数可解析。 | automated_test | FR2 |
| **AC4** | 用户存在(密码错)与用户不存在两种失败的 p50/p95 响应时间差小于实现时设定阈值(如 10ms);两路径都经过一次 argon2id 等价计算。 | automated_test | FR10 |
| **AC5** | 用有效 refresh token `POST /v1/auth/refresh` 返回新 access + 新 refresh;旧 refresh 再次使用返回 401,且该 token family 全部撤销(family 内其他 token 后续 refresh 也 401)。 | automated_test | FR4 |
| **AC6** | 对同一 refresh token 并发发起 2 次 refresh,结果确定:有且仅有一次成功换新、另一次 401;不出现两个都成功(双花)或都失败(锁死合法用户)。 | automated_test | FR4 |
| **AC7** | 撤销一个 refresh token 后用它 refresh 返回 401;撤销会话后用其 access token 访问受保护端点在 TTL 内返回 401(opaque 查 DB 即时撤销)。 | automated_test | FR3, FR4, FR13 |
| **AC8** | 受保护路由在 无 Bearer 头 / 无效 token / 过期 token 时均返回 401 经 `WriteError` `unauthorized` slug;`/livez` 在有/无 Bearer 头时均 200 且不触发 DB 调用。 | automated_test | FR5, NFR6 |
| **AC9** | 被 auth 拒绝的请求其 access-log 行存在且 requestId 非空、与响应关联;`go list -deps internal/http` 不含 `internal/modules/auth`。 | automated_test | FR5, NFR2 |
| **AC10** | 对单账户以低于速率阈值节奏持续提交错误密码,累计 K 次后触发锁定(状态落 DB);锁定状态在进程重启 / 缩零唤醒后仍生效(对已锁账户继续拦截)。 | automated_test | FR8 |
| **AC11** | 单实例并发 N 个错误密码请求,内存峰值低于实例上限;前置失败计数命中阈值后的请求不进入 argon2id 计算。 | automated_test | FR8 |
| **AC12** | 构造带已知明文密码的 login 触发 401 与触发 500,断言响应体与该请求的 slog 日志均不含该明文密码;对 login handler 注入 panic,recover 日志 grep 不到明文密码/token。 | automated_test | FR11, NFR4 |
| **AC13** | login/refresh 请求体 body 超 size cap、含未知字段、类型错配时返回 400 经 `WriteError`,进程不 panic。 | automated_test | FR1, NFR8 |
| **AC14** | 401/429 响应体顶层字段集与现有 400/404/500 一致(`error.code` / `error.message` / `requestId`),仅 code 值为 `unauthorized` / `rate_limited`;现有 server_test 信封断言不被破坏。 | automated_test | FR6 |
| **AC15** | goose `00002` up 后 users/refresh_tokens/access_tokens 表存在、down-to 0 后消失;sqlc drift gate 绿;refresh handler 在单 `Begin` 事务内完成撤销+写入,事务中途注错则旧 token 不被消费、新 token 不落库。 | automated_test | FR7, FR4 |
| **AC16** | `go list -deps internal/modules/auth` 不含 `internal/http`;`grep pgxpool.Pool internal/modules/auth` 为空;`internal/` 顶层子目录集合无新增(只在 `internal/modules/` 下加 auth)。 | automated_test | FR14, NFR2 |
| **AC17** | `internal/modules/auth` 导出 Bearer 中间件构造器 `func(http.Handler) http.Handler` + token 校验器接口;`cmd/api/main.go` 装配它进链;auth 中间件代码不在 `internal/http` 包内。 | code_review | FR5, FR14 |
| **AC18** | `CONTRACTS.md` 记录 login/refresh 完整字段(请求 `{username,password}`/`{refreshToken}`、响应 `{userId,accessToken,refreshToken,expiresAt 毫秒}`)+ 错误 slug,标 `unstable`,冻结条件写明「客户端真实消费验证通过后」;T2 交付不把 auth DTO 列入 frozen-paths;golden fixture 与序列化输出一致、与客户端 `fixtures/token-cases.json` 对齐。 | code_review | FR12, NFR7 |
| **AC19** | 代码库 + 迁移文件 grep 不到明文 seed 口令;seed 口令来自运行时注入。 | automated_test | FR9 |
| **AC20** | 禁用用户(`status=disabled`)的 access token 访问受保护端点返回 401、refresh 返回 401。 | automated_test | FR13 |

---

## 10. Edge Cases and Failure States

- **E1 — 用户枚举(时序+状态码+message)**:统一 401 + 不存在用户走 dummy argon2id 恒定工作量(FR10/AC4)。
- **E2 — 并发 refresh 双花/锁死**:`Begin` 事务 + 行锁,有且仅有一次成功(FR4/AC6)。
- **E3 — refresh token 重放(旧 token 被盗复用)**:token family 检测,旧 token 用即撤销整个 family(FR4/AC5)。
- **E4 — 通用限流在缩零多实例下不可靠**:故 T2 不依赖它做安全——防爆破唯一主防线是 DB 层账户锁定(跨实例,FR8/AC10);通用限流退化为可替换 `RateLimiter` facade 空接缝(D5)。
- **E5 — argon2id CPU/内存 DoS 放大**:前置廉价拦截先于哈希(FR8/AC11)。
- **E6 — 凭据泄露日志(panic/error/body)**:Secret 包装 + 禁记 body + 审 panic(FR11/AC12)。
- **E7 — expiresAt 单位双端错位**:已锁定 Unix 毫秒(D13/NFR7),golden fixture 钉死毫秒值;若误用秒会让 token 永不/提前失效(差 1000 倍)。
- **E8 — seed 弱口令/硬编码**:口令注入、非默认、不入 git(FR9/AC19)。
- **E9 — 禁用用户仍能 refresh**:refresh/Bearer 查 user status(FR13/AC20)。
- **E10 — 畸形/超大 JSON**:body size cap + 严格解析 400 不 panic(NFR8/AC13)。
- **E11 — access token 撤销在 JWT 下无法即时生效**:选 opaque 查 DB 规避(D3)。
- **E12 — username 大小写/唯一性**:归一化 + unique 约束(FR7/E12)。

---

## 11. Risks and Mitigations

| ID | 风险 | 影响/概率 | 缓解 | owner |
|---|---|---|---|---|
| **R1** | Go 新手 + AI 在 token/密钥/密码处理草率(ROADMAP R6,单人易错区) | high/medium | NFR3 查文档不写魔数 + L1 五路 + Codex 异源 review + D3 opaque 规避 JWT 撤销坑 + 实现后再做 Codex 异源 review | Mingbo |
| **R2** | 误把通用限流当安全防线(缩零多实例配额放大、重启清零,实为假防护) | medium/low | T2 不实装通用进程内限流、不依赖它做安全;防爆破唯一主防线 = DB 账户锁定(跨实例);账单由 `max-instances=2` 封顶;通用限流抽成可替换 `RateLimiter` facade 留升级口(成熟库/网关/Redis,硬规则③④) | engineering |
| **R3** | 数据访问接缝未通(`server.go:40 _=db`、sqlc `DBTX` 无 Begin) | high/high | D9/FR14 显式化 `server→handler→db.Begin→dbgen.WithTx`,作 implementer 首要任务(T1) | engineering |
| **R4** | 契约过早冻结固化错误(客户端未验证) | medium/medium | FR12/D8「走通」= 客户端真实消费验证后才 frozen,T2 内只 unstable | Mingbo |
| **R5** | access token 形态选错(JWT 难撤销 / opaque 性能) | high/low | D3 opaque + 查 DB(撤销天然支持);连接池预算评估(每请求 +1 往返 vs `maxConns=5×2`) | engineering |
| **R6** | refresh 重放 / 会话固定 | high/medium | FR4 rotation + token family、FR3 服务端生成 token 不接受 client 提供值 | engineering |
| **R7** | 用户枚举泄露 | medium/medium | FR10 统一 401 + dummy hash | engineering |
| **R8** | expiresAt 双端错位致 token 永不/提前失效 | low/low | D13/NFR7 已 W1 核实客户端=Unix 毫秒 + golden fixture 钉死 | Mingbo |

---

## 12. Default Decisions

> 协调者替明博做的可推翻决策。D1 是明博在锻造前选定;D2 是据 D1 推出的高影响 default(高亮);D13/D14 据 W1 前置调研核实/明博选定(原 Q1/Q2 关闭);其余按 L1 五路 + Codex 调研默拍。

- **D1**:login 凭据 = **用户名+密码多用户**(users 表 + 密码哈希 + 后续注册)。*Why*:客户端只固化 login 输出未固化输入,凭据形态是 Mingbo-owned 产品决策,明博在锻造前 AskUserQuestion 选定。*Override if*:明博改为预共享密钥 / 设备注册 → 重锻造。
- **D2(高亮)**:威胁模型 = **核心认证安全属性不降级 + 规模型/合规型增强 defer**。**不降级(T2 必做)**:密码 argon2id 哈希存储/校验、access+refresh token 生命周期/滚动轮换/撤销/重放检测、登录失败处理 + 用户枚举防护(恒定工作量)、会话/权限边界清晰、安全事件最小可追溯(slog 字段 NFR4)。**可 defer(规模型/合规型)**:分布式限流、设备绑定/指纹、独立持久化审计表、通用进程内限流实装。**升级触发条件**:公开自助注册 / 客户规模增长 / 引入管理员角色 / 合规要求 / 异常登录频率上升。*Why*:server-infra-toolkit 是单人项目 + 小型客户端、非公网大型 SaaS,但认证登录 / 安全边界是不可逆决策(明博硬规则:此类必须优先稳),故**核心安全一律不降级**,只把规模型/合规型增强按需延后——避免「因为小所以降级安全」的误读。*Override if*:命中上述任一升级触发条件 → 重评 defer 项(优先分布式限流 + 设备绑定 + 独立 audit)。
- **D3**:access token = **opaque + 查 DB**。*Why*:撤销天然支持(FR4 撤销是 P0)、零新依赖、与 refresh 哈希存储同构、消解「FR3 说 opaque 但 NFR1 又备签名密钥」的矛盾;JWT 自包含难即时撤销。*Override if*:受保护端点 QPS 高到每请求 DB 往返成瓶颈(连接池预算超限)→ 引入 access token 缓存或短 TTL+JWT+黑名单。
- **D4**:refresh = **滚动轮换 + token family**。*Why*:OWASP 对 opaque refresh 标准实践,可检测重放(旧 token 用即撤销 family),与哈希存储成本相同。*Override if*:明博要简化并接受「refresh 泄漏长期有效」残留风险。
- **D5**:防爆破**唯一主防线** = **DB 层账户失败计数 + 锁定**(跨实例);**通用限流 T2 不实装**,只留可替换 `RateLimiter` facade 空接缝。*Why*:Cloud Run 缩零多实例下进程内限流配额放大 N 倍且重启清零(不可靠),DB 层账户锁定天然跨实例;账单已由 `max-instances=2` 封顶(进程内限流的账单收益与之重复,硬规则⑤没明确收益默认拒绝);通用限流属商品化基础设施,自研撞硬规则③,故 facade 隔离待需要时接成熟库/网关/Redis。*Override if*:出现明确账单尖峰 → 经 facade 接 `golang.org/x/time/rate`;需跨实例配额 → 接 Redis/网关分布式限流(backlog)。
- **D6**:用户来源 = **seed 预置**(migration / 子命令算 argon2id),注册端点 defer。*Why*:自助注册连带查重/防滥用/邮箱验证(与 NG3 冲突)、撑爆 T2 范围;seed 让 login/refresh 闭环先跑通。*Override if*:需对外开放多用户自助注册。
- **D7**:用户枚举防护 = **统一 401 + dummy hash 恒定工作量**。*Why*:堵时序 + 状态码 + message 三个泄露口。*Override if*:无(标准实践)。
- **D8**:契约冻结时机 = **「走通」= 客户端真实消费验证后才 frozen**,T2 内只 unstable + golden。*Why*:对齐 ROADMAP NFR6/R2 防过早冻结固化错误。*Override if*:无。
- **D9**:数据访问接缝 = **NewServer 把 db 传 auth handler 构造器,auth 消费侧声明自己 DB 窄接口(不 import internal/http),refresh 事务 `db.Begin`→`dbgen.WithTx`**。*Why*:`server.go:40` 当前 `_=db` 丢弃注入、sqlc `DBTX` 无 Begin、避免 modules→http 耦合。*Override if*:无。
- **D10**:Bearer 中间件 = **auth 导出构造器由 main 装配,nest 在 request-id/access-log 内、不包 livez**。*Why*:中间件放 http 层会撞依赖方向红线(http import modules);放 auth 由 main 装配保持方向 + 保证每个 401 有 requestId 进访问日志。*Override if*:无。
- **D11**:凭据内存包装 = **`config.Secret` 风格类型**(redact)。*Why*:堵 panic/error/body 三泄露口,复用 T0 脱敏范式。*Override if*:无。
- **D12**:token 熵 = **crypto/rand ≥256 bit base64url 无填充**。*Why*:OWASP ≥128 bit,取 256 留余量,复用 middleware crypto/rand 范式。*Override if*:无。
- **D13**:wire 契约形态 = **客户端 `LoginSession` 真相**——login/refresh 响应 `{userId, accessToken, refreshToken, expiresAt}`(全 camelCase、全必填、`expiresAt`=Unix 毫秒绝对时刻 int64);login 请求 `{username, password}`、refresh 请求 `{refreshToken}`。*Why*:W1 核实客户端 `~/Code/app-infra-toolkit` Swift/Kotlin 源码 + `fixtures/token-cases.json`,客户端 Decodable/Deserializer 全字段必填、缺字段抛错;never break userspace → 服务端适配客户端,不反向改客户端。*Override if*:无(客户端已固化)。
- **D14**:密码哈希依赖 = **`golang.org/x/crypto/argon2` 自写 PHC**(原 Q2,明博选定)。*Why*:Go 官方扩展库、可信、依赖极简,不引未审计第三方封装;PHC 编码/解码/严格输入校验自写,argon2id 参数(`m=19456/t=2/p=1` 起)实现时上 OWASP 复核 + 目标硬件本地校准,salt 16B/key 32B 出处 RFC 9106(非 OWASP,注释标对来源)。*Override if*:无。

---

## 13. Open Questions

> 本 PRD 已无未决 open question:原 Q1/Q2 经 W1 前置调研关闭。

- **Q1(已关闭)**:`expiresAt` 单位 / 语义 / 类型 → W1 核实客户端 `LoginSession.expiresAt` = **Unix 毫秒绝对时刻 int64**(+ `fixtures/token-cases.json` 实证),固化为 **D13**。
- **Q2(已关闭)**:密码哈希依赖选型 → 明博选定 **`golang.org/x/crypto/argon2` 自写 PHC**,固化为 **D14**。

> 威胁模型(D2)与用户来源(D6)虽影响大,但据明博已选「多用户密码」可推出务实 default,走 §12 可推翻 default、不占 open question 名额。

---

## 14. Implementation Tasks

> 每个 task 是 T2 实现 session 内一步。`deps` 即施工顺序,`done_when` 引用 §9 AC。**动手前置已全部解决**:expiresAt=Unix 毫秒(D13)、哈希=`x/crypto/argon2`(D14)、威胁模型 D2、用户来源 D6 均已定;实现时仍需 argon2 参数本地校准 + 上 OWASP 复核当前值(NFR3),seed 口令由明博注入。

- **T1**(deps: —;done: AC15, AC16):建 `internal/modules/auth` 骨架 + goose `00002` 迁移(users/refresh_tokens/access_tokens,最小固定字段集)+ sqlc query;**打通数据访问接缝**(NewServer→auth handler 构造器→消费侧 DB 窄接口→`db.Begin`→`dbgen.WithTx`)。owner: implementer
- **T2**(deps: T1;done: AC3, AC19):密码 argon2id 哈希(选型 Q2、参数查 OWASP + 本地校准、PHC 编码)+ seed 机制(migration/子命令,口令注入非硬编码)。owner: implementer
- **T3**(deps: T1, T2;done: AC1, AC2, AC4, AC13):login 端点(严格解析→验证→统一 401 + dummy hash 恒定工作量→颁发 token)。owner: implementer
- **T4**(deps: T1;done: AC5, AC6, AC7, AC15):refresh 端点(opaque token + split selector/verifier 哈希存储 + 滚动轮换 + token family 重放检测 + `Begin` 事务 + 行锁)。owner: implementer
- **T5**(deps: T1, T3;done: AC8, AC9, AC17):Bearer 中间件(auth 导出构造器、main 装配、opaque 查 DB、nest 位置不包 livez)+ token 校验器接口。owner: implementer
- **T6**(deps: T2, T3;done: AC10, AC11):防爆破(DB 账户失败计数 + 锁定[主防线]、argon2id 前置廉价拦截;`RateLimiter` facade 空接缝,T2 不实装通用限流)。owner: implementer
- **T7**(deps: T3, T4, T5;done: AC12, AC14, AC20):凭据脱敏(Secret 包装 + 禁记 body + 审 panic)+ 错误信封新增 slug + 用户状态联动。owner: implementer
- **T8**(deps: T3, T4, T5;done: AC1, AC5, AC8):整链 automated_test(seed 用户登录→refresh→Bearer 保护,经连接池 + 事务 + sqlc 跑通 dockerized PG)+ `verify.sh` 全绿(sqlc drift + migration round-trip)。owner: test-runner
- **T9**(deps: T3, T4, T8;done: AC18):契约固化(手写 login/refresh DTO + golden fixture[可复用客户端 token-cases.json],`CONTRACTS.md` 标 unstable + 冻结 checklist + expiresAt=毫秒 D13)。owner: implementer
- **T10**(deps: T3, T4, T5;done: AC5, AC7, AC12):实现后对高杠杆代码(token 生成 / refresh 轮换 / Bearer 中间件 / 密码哈希)做 Codex 异源 review。owner: Codex

**关键路径**:T1→T3/T4→T5→T7→T9。**并行点**:T2 ‖ T4(都依赖 T1);T5 ‖ T6(都依赖 T3)。**明博介入节点**:动手前置已解决(D13/D14/D2/D6);seed 口令需明博注入;T8/T9 涉及 dockerized PG 整链与契约固化,契约 frozen 待客户端真实消费验证。

---

## 15. 附:本 PRD 的锻造来源(L1 留痕)

L1 五件套(协调者单 message 并行启 sub agent,均 opus 只读)+ Codex 异源批判:

- **codebase-scan**(design-explorer):核实零 auth 脚手架、客户端 wire 契约未落地、login 输入凭据完全未定义、T1 接缝(`apphttp.DB.Begin` 预留但 `server.go _=db` 丢弃、sqlc `DBTX` 无 Begin)。
- **requirement / redteam / implementation / architecture**(design-reviewer)+ **Codex**(异源):五路高度收敛到——威胁模型未定(redteam 升级)、U1 access token 形态实为 P0 阻塞、进程内限流缩零失效、数据访问接缝从未打通(implementation 独家)、refresh 并发竞态/轮换未定、auth 模块 Go 导出接口面缺失(architecture 独家)。Codex 补充用户枚举时序、token 熵、split-token、bootstrap 权限、会话固定、日志泄露面。

**决策路由**:1 个实现层 R3(login 凭据,明博锻造前选定 = D1);其余 unknown 走 R1 default + audit(§12 D2-D14);2 个 open question 经 W1 前置调研关闭(Q1 expiresAt=毫秒 → D13、Q2 哈希=`x/crypto/argon2` → D14)。威胁模型据 D1 推出务实 default(D2),不升第 2 个 R3。

*本产物由 design-gate-lite(协调者模式)生成,仅做 PRD 锻造,未写任何实现代码。下游 T2 落地由明博显式授权后另起 session,建议实现后对高杠杆的 token/密码/中间件代码做 Codex 异源 review。*
