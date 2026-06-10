# PRD: W2d — T2 auth 收尾(账户锁定主防线 + 凭据脱敏 + 错误信封收敛 + login 状态门)

> design-gate-lite 产物 · mode **L1** · slug `t2-w2d-hardening`
> 上游:`product-requirements/t2-auth/prd.md`(T2 站 PRD,W2d 落地其 T6+T7,相关条款 FR8/FR11/FR13、D2/D5、E4/E5/E6、AC10/AC11/AC12/AC14/AC20)
> 前序代码:已提交 W2a(骨架+迁移)/W2b(argon2 哈希+login)/W2c(refresh 滚动轮换+Bearer 中间件)
> 配套机器可读文件:`handoff.json`(同目录)

---

## 1. Summary

为 server-infra-toolkit 第三推进站 T2「auth」的收尾阶段 W2d 锻造可落地 PRD(mode **L1**)。W2d **不是新推进站**,是 T2 站内 T6(防爆破)+ T7(凭据脱敏 + 错误信封 slug + 用户状态联动)两个 task 的落地展开,建立在已提交的 W2a/b/c 代码与已定的 T2 PRD(尤其 D2 威胁模型、D5 防爆破主防线)之上。范围:把 T2 PRD 停在需求层的防爆破/脱敏需求落到策略与参数层并实装——DB 层账户失败计数 + 锁定(原子计数、跨实例)作为唯一主防线、argon2id 前置省算、RateLimiter facade 空接缝、密码脱敏(token 不脱敏以守冻结 wire)、login 侧用户状态门,并补齐草稿漏掉的成功清零 / DB 时钟解锁 / 锁定事件审计。本 PRD 由协调者并行调度 L1 五件套(codebase-scan + requirement/redteam/implementation/architecture,均 opus 只读)综合而成;四路高度收敛,关键决策落 §12 可推翻 default,唯一升级项(账户锁定语义)由明博在锻造中选定(硬锁 + 接受 account-lockout DoS 残留 + 运维解锁,记 D1)。

---

## 2. Goal Alignment

- **目标用户**:复用 server-infra-toolkit 的服务端开发者(明博)+ 被这套 auth 保护的账户/凭据;次要消费者:W2d 落地的 implementer/test-runner/reviewer,以及后续 import auth 中间件的 T4/T5。
- **问题**:T2 的 login/refresh/Bearer(W2a/b/c)已落地,但防爆破(T6)与脱敏/slug/状态联动(T7)只停在 T2 PRD 需求层(FR8/FR11/FR13),未定到策略/参数层;且代码现状暴露了真缺口——主防线计数 `SetUserLock` 非原子(跨实例并发丢计数)、login 侧无用户状态门、锁定生命周期(清零/解锁判定)未定。不收尾 T2 站无法冻结契约进 T3。
- **成功长什么样**:账户失败锁定在 Cloud Run 多实例并发下计数无丢失(原子)且不破反枚举(锁定复用统一 401、走等价工作量);密码在日志/error/panic 全路径脱敏,token 明文进响应体不破冻结 wire;错误信封 append-only 合规演进;login 侧补 user status 门(与 refresh/Bearer 对齐);成功登录清零、DB 时钟解锁、锁定事件审计齐全;被 DoS 锁死的受害者有运维解锁救济;`bash scripts/verify.sh` 退出码 0。
- **范围边界**:范围 = T2 的 T6+T7 落地(见 §5 Goals)。非目标 = 通用/分布式限流实装、软锁、账户+IP 粒度、设备绑定、token 表清理治理、独立审计表、自助解锁端点(全部 defer,承 D2/NG9/NG10)。
- **首个验收信号**:对单账户并发提交 N 次错误密码,`failed_attempts` 准确达 N 且账户锁定(无丢计数,dockerized PG 集成测试);锁定账户登录返回与普通失败逐字节一致的 401;成功登录后计数归零;`verify.sh` 全绿。
- **status**:`clear` —— 目标层清楚(T2 PRD §2/§5/§12 直接点明 T6/T7 的用户、问题与成功标准,D2/D5 已定威胁模型与主防线)。A.5 目标对齐 gate 不触发(target_user 与 success_outcome 均可从 T2 PRD + 项目 context 推出单一可信值;指不出因解读不同而互斥的 P0 需求——方向已由 D5 钉死)。
- **confidence**:high

---

## 3. Background and Problem

W2a/b/c 已把 T2 auth 的核心闭环落地:用户名+密码登录(argon2id PHC,W2b)、refresh 滚动轮换 + token family 重放检测 + 事务/行锁/advisory-lock/db_now 跨时钟纪律(W2c)、Bearer 中间件 + 用户状态门(W2c)。但 T2 PRD 的两个 task 还挂着:

- **T6 防爆破**:FR8 定了"DB 层账户失败计数 + 锁定 = 唯一主防线 + argon2 前置廉价拦截 + RateLimiter facade 空接缝",但阈值/窗口/重置/粒度/接入点全是占位符,且未触及并发正确性。
- **T7 脱敏/slug/状态**:FR11(凭据脱敏)、FR6(错误信封新 slug)、FR13(用户/会话状态联动)只在需求层。

**codebase-scan(L1)核实的真实代码现状**(grounded,非规划语):
- `users.failed_attempts(int NOT NULL DEFAULT 0)` / `locked_until(timestamptz 可空)` / `status(text NOT NULL DEFAULT 'active')` 三列**已存在**(`db/migrations/00002_auth.sql`),`GetUserByUsername` 已返回这三值——W2d 读侧无需新查询、**无需新迁移**。
- `SetUserLock`(`sql/auth.sql`)存在但是**非原子整值覆写**(`SET failed_attempts=$2`),Go 侧读-改-写;Cloud Run 2 实例并发爆破下两实例同读同写会丢计数,使 D5"跨实例唯一主防线"在并发下失效。**无失败计数自增查询、无 `UPDATE...RETURNING`**。
- `login.go` 的 `verifyCredentials` **当前完全不查 status/locked_until、不动 failed_attempts**(注释明写"留给 T6");`DummyVerify(password)bool` 恒定工作量已接两条无匹配路径;`currentParams m=19456/t=2/p=1`。
- 错误 code 现集:HTTP 层(`internal/http/errors.go`)只有 `internal/bad_request/not_found`;auth 本地(`login.go`)有 `unauthorized/bad_request/internal`;**全 repo 无 429**。auth 不 import internal/http,本地复刻信封 + `envelope_test` 逐字节对账防漂移。
- `config.Secret` 四方法(`String/MarshalJSON/LogValue/Reveal`)齐全,只用于 `Config.DSN`;auth 凭据(密码/token)**全是裸 string**。`config.Secret` 无 `UnmarshalJSON`。
- 日志:access-log 中间件**不记 request body**(已满足脱敏一半);`platformlog.Panic` 用 `slog.Any("panic", value)` 直打 panic 值(潜在反射泄漏面,当前无实际泄漏)。
- `RateLimiter` 全 repo 零基础(无接口、无占位)。
- login **缺** user status 门(refresh/Bearer 已有 `user.Status != userStatusActive → 401`,常量 `userStatusActive` 已定义可复用)。

---

## 4. Users and Use Cases

- **主要用户**:复用 toolkit 的服务端开发者(明博)+ 被保护账户。
- **次要用户**:W2d implementer/test-runner/reviewer;运维(明博)解锁被 DoS 锁死的账户。
- 用例:
  1. 攻击者爆破单账户登录 → 失败计数原子累积达阈值 → 账户锁定 M 分钟(跨实例、重启仍生效)。
  2. 锁定期内任何登录(含正确密码)→ 返回与普通失败逐字节一致的 401(锁定态对外不可观测)。
  3. 正常用户偶发输错几次后用正确密码登录 → 成功且失败计数归零。
  4. 攻击者用受害者用户名错密码锁死对方(account-lockout DoS)→ 受害者等窗口过期,或明博用运维手段解锁。
  5. login handler 在持有明文密码时 panic → recover 日志不含明文密码/token。
  6. 客户端成功登录 → 响应体含明文 accessToken/refreshToken(token 不脱敏)。
  7. 禁用用户(status=disabled)登录 → 与普通失败一致的 401。

---

## 5. Goals and Non-goals

**Goals**(价值导向):
- **G1**:防爆破主防线在 Cloud Run 多实例并发下计数正确(DB 侧原子自增),成为真防线而非纸防线。*价值*:D5 主防线在并发爆破下不被绕过。
- **G2**:账户锁定不引入可枚举/可计时侧信道(锁定复用统一 401 + 走等价工作量),守 W2b 反枚举不变量 + D2 不降级。*价值*:防爆破不以泄露账户存在性为代价。
- **G3**:密码在日志/error/panic 全路径脱敏;token 明文进响应不破冻结 wire(D13)。*价值*:堵凭据泄漏面且不回归登录链路。
- **G4**:错误信封 append-only 合规演进 + login 侧用户状态门补齐(与 refresh/Bearer 对齐)。*价值*:契约不破 + 禁用用户立即失效覆盖 login 入口。
- **G5**:账户锁定生命周期完整(原子自增→锁定→DB 时钟到期→成功清零)+ 锁定事件可审计 + 被 DoS 锁死有运维救济。*价值*:不误锁正常用户、锁能自动解除、有审计与救济。
- **G6**:RateLimiter 留最小可替换接缝(遵 D5,不实装通用限流),T2 站收尾。*价值*:为将来接成熟限流库/网关留口,不自研(硬规则③④)。

**Non-goals**(逐条钉死):
- **NG1**:不实装通用/分布式限流(只留 RateLimiter facade no-op 空接缝;承 T2 D5/NG10)。
- **NG2**:不做软锁(明博 R3 选硬锁;软锁牺牲 argon2 前置省算 E5)。
- **NG3**:不做账户+IP 粒度锁定(需新 schema、依赖不可信的 Cloud Run X-Forwarded-For、属 D2 defer 的规模型增强;明博 R3 选按账户)。
- **NG4**:不做设备绑定/指纹(承 T2 NG9/D2 defer)。
- **NG5**:不做 token 表 TTL 清理/每用户行数上限治理(正交 DoS 面,defer;触发:token 表增长成本显著)。
- **NG6**:不建独立审计持久化表(锁定事件走 slog;承 NFR4 defer,触发:合规要求)。
- **NG7**:不做自助解锁端点/密码重置(承 T2 NG8;只提供运维侧解锁手段)。
- **NG8**:不为 status/locked_until 加索引、不新增迁移文件(列已就绪,锁定判定复用已取 user row,无范围扫描需求)。

---

## 6. Requirements

### 功能性(FR)

- **FR1 — 账户锁定原子计数**:登录失败时通过 DB 侧单语句原子操作累加失败计数并在达阈值 N 时置 `locked_until = DB now() + 窗口 M`(形如 `UPDATE users SET failed_attempts = failed_attempts + 1, locked_until = CASE WHEN failed_attempts + 1 >= $N THEN now() + $window ELSE locked_until END WHERE id = $1 RETURNING failed_attempts, locked_until`);杜绝读-改-写竞态,保证多实例并发下计数无丢失。阈值 N / 窗口 M 参数化、带 OWASP 出处注释,不内联魔数。映射 T2 FR8。
- **FR2 — 锁定前置拦截 + 等价工作量**:在 `GetUserByUsername` 之后、真实密码校验之前判定账户是否处于锁定(`locked_until` 未过期);锁定账户跑一次 `DummyVerify`(预算 dummyHash,不放大内存、不对真实用户 hash 执行 argon2)后返回与普通凭据失败逐字节一致的统一 401,**不进入真实用户 hash 的 argon2 计算**。映射 T2 FR8/FR10。
- **FR3 — 成功登录清零**:登录成功时清零 `failed_attempts` 并清除 `locked_until`(幂等覆写,可复用 `SetUserLock` 或新增 `ResetLoginFailures`),使正常用户偶发输错不被单调累积误锁。映射 T2 FR8。
- **FR4 — DB 时钟解锁判定**:锁定是否过期的判定全程在单一 DB 时钟域(`locked_until` 与 `now()` 都来自 DB),不用 app `time.Now()` 比较 `locked_until`;实现可选给 `GetUserByUsername` 加 `now()::timestamptz AS db_now` 列、或将锁定判定下推 SQL(`WHERE ... AND (locked_until IS NULL OR locked_until <= now())`)。映射 T2 FR8(沿用 W2c db_now 纪律)。
- **FR5 — 锁定响应复用 401**:账户锁定状态对外不可观测——锁定路径返回与"密码错/用户不存在/畸形 body"完全一致的统一 401 + 同 message,不返回 429、不带 `Retry-After`、不引入锁定专属对外 code。`rate_limited`/429 slug 仅保留给将来 RateLimiter 实装(本期 no-op 不产生 429)。映射 T2 FR6/FR10。
- **FR6 — login 侧 user status 门**:login 在密码校验之后补 `status != active → 统一 401`(复用 `userStatusActive` 常量);disabled 用户走完等价 argon2 工作量再返回(与 active-错密码计时一致),且 disabled 用户不累加锁定计数。映射 T2 FR13。
- **FR7 — 密码脱敏类型**:密码用 auth 本地脱敏类型(string 别名,实现 `String()`/`LogValue()`/`MarshalJSON` 三路返回 `[REDACTED]`,`UnmarshalJSON` 用默认 string),使 `slog.Any` 反射 panic 值时不泄漏明文;**不复用** `config.Secret`(避免 platform/config 承载业务凭据语义 + 其无 `UnmarshalJSON`);保证不破 `DisallowUnknownFields` 严格解析。映射 T2 FR11。
- **FR8 — token 脱敏边界**:token(accessToken/refreshToken)保持裸 string、明文序列化进 `LoginSession` 响应体(守 D13 冻结 wire,**绝不**套 redact 包装);token 不泄漏靠"全路径不写进 log/error"(现状已遵守,确立测试)。映射 T2 FR11。
- **FR9 — panic 审计不泄漏**:确立不变量 + 测试——recover/panic 路径(`platformlog.Panic` 的 `slog.Any`)输出不含明文密码/token/Authorization;通过密码脱敏类型(FR7)使反射输出 redact。映射 T2 FR11/NFR4。
- **FR10 — 错误信封演进合规**:任何新增 code slug 在 auth 本地与 internal/http 两处取值逐字一致(对账测试覆盖 slug 字符串值,不只信封形状),append-only 不改顶层结构;W2d 实际不新增对外 slug(锁定复用 `unauthorized`)。映射 T2 FR6。
- **FR11 — 锁定事件可观测**:账户锁定触发时打一条 slog JSON 事件(`event=account_locked`,含 `user_id` + `request_id`,绝不含明文密码/token);来源 IP defer(auth 不 import http 拿不到)。映射 T2 NFR4。
- **FR12 — RateLimiter facade 空接缝**:在 `internal/modules/auth` 内定义最小可替换 `RateLimiter` 接口(签名预留兼容未来进程内/分布式,如 `Allow(ctx, key) (allowed bool, retryAfter time.Duration)`)+ no-op 默认实现(恒放行);Handler 持有该接口默认装 no-op;login 流预留调用点(本期恒放行不拦);命名中性不暗示"已防护";不 import 任何限流库。映射 T2 FR8/D5/NG10。
- **FR13 — 运维解锁手段**:提供一条文档化的运维解锁手段(cmd 子命令或 CONTRACTS/README 记录的 SQL,清 `failed_attempts`/`locked_until`),给被 account-lockout DoS 锁死的受害者在窗口过期前救济。映射 account-lockout DoS 残留缓解(明博 R3 选定)。

### 非功能(NFR)

- **NFR1 — 依赖方向**:W2d 全部增量遵守 `internal/modules/auth` 不 import `internal/http`;脱敏类型/RateLimiter/锁定逻辑放 auth 内(platform 被依赖合法);`verify.sh` 依赖方向 gate 绿。承 T2 NFR2。
- **NFR2 — 零迁移 + sqlc 漂移**:W2d 不新增迁移文件(列已就绪);改 `sql/auth.sql` 后必须 `cd tools && go tool sqlc generate`,`verify.sh` sqlc 漂移 gate + 迁移 round-trip gate 绿。
- **NFR3 — 参数化非魔数**:锁定阈值 N / 窗口 M 查当前 OWASP、参数化传入 SQL、带出处注释,不内联凭记忆的魔数(承 T2 NFR3)。

---

## 7. User Flow / State Flow

### 7.1 登录流(W2d 接入点用 ★ 标)

```
POST /v1/auth/login {username, password}
  → 中间件链 recover → request-id → access-log(不记 body)
  → auth handler:严格解析 body(已有)→ username 归一 GetUserByUsername
      ★ 锁定前置拦截(FR2/FR4):locked_until 未过期(DB 时钟)→ DummyVerify 等价工作量 → 统一 401(不进真 argon2)
      → argon2id 校验(不存在用户走 dummy,恒定工作量,已有)
      ★ user status 门(FR6):status != active → DummyVerify/等价工作量后 → 统一 401(不累加计数)
          ├ 成功 → ★ 成功清零(FR3)→ 事务内颁发 access+refresh → LoginSession 200(token 明文 FR8)
          └ 失败 → ★ 原子自增失败计数(FR1);达阈值 → 置 locked_until + ★ slog account_locked(FR11)→ 统一 401
```

### 7.2 锁定状态机

| 状态 | 触发 | 转移/响应 |
|---|---|---|
| 未锁定 | failed_attempts < N | 正常校验 |
| 失败累加 | 一次失败 | 原子 +1(FR1);若新值 ≥ N → 置 locked_until = DB now()+M + 打 account_locked |
| 锁定中 | locked_until > DB now() | 前置拦截 → DummyVerify → 统一 401(FR2/FR5),不进真 argon2 |
| 自动解锁 | locked_until ≤ DB now() | 下次失败计数从 1 重计(FR4/D6) |
| 成功清零 | 任意时刻登录成功 | failed_attempts=0, locked_until=NULL(FR3) |
| 禁用 | status=disabled | 等价工作量后统一 401,不累加(FR6) |
| 运维解锁 | 明博执行解锁手段 | 清 failed_attempts/locked_until(FR13) |

---

## 8. Data, API, Permissions

### Data(schema 单一真相源 = `db/migrations`;**W2d 零迁移**,列已就绪)

- **users**(已存在,W2d 不改 schema):`failed_attempts(int NOT NULL DEFAULT 0)`、`locked_until(timestamptz 可空)`、`status(text NOT NULL DEFAULT 'active')`。
- **新增 sqlc 查询(改 `sql/auth.sql` + regenerate,非迁移)**:`RecordLoginFailure :one`(原子自增 + CASE 锁定窗口 + RETURNING)、`ResetLoginFailures :exec`(成功清零,或复用 `SetUserLock`);DB 时钟方案二选一(`GetUserByUsername` 加 `db_now` 列 或 锁定判定下推 SQL)。

### API

- `POST /v1/auth/login`:行为契约不变(请求/响应 DTO 冻结,D13);W2d 仅在失败/锁定/禁用路径内部新增锁定逻辑,**对外响应形态不变**(锁定复用 401)。
- **不新增对外 slug / 不新增端点 / 不新增 429**。

### Permissions / Secrets / Audit

- 不引入新 secret;运维解锁手段若用 cmd 子命令则复用 `NEON_DSN`(密码不经 AI、明博注入)。
- 锁定/解锁事件走 NFR4 既有 slog(带 user_id/request_id,无凭据),无独立 audit 表(defer)。

---

## 9. Acceptance Criteria

> 每条引用存在的 FR/NFR,description 不含模糊词黑名单。AC 编号为 W2d 独立命名空间。

| ID | 验收标准 | 验收方式 | 关联 |
|---|---|---|---|
| **AC1** | 对单账户并发提交 N 个(N≥阈值)错误密码请求,全部返回后 `failed_attempts` 等于 N(无丢失更新)且 `locked_until` 非空;由原子 `UPDATE...RETURNING` 实现,代码审查确认无读-改-写两段式。 | automated_test | FR1 |
| **AC2** | 单账户失败累计达阈值触发锁定后,另起一个新 Handler over 同连接池(模拟实例重启、内存态清空)对该账户仍返回锁定 401,证明锁定状态落在 DB 跨实例生效。 | automated_test | FR1 |
| **AC3** | 用户 N-1 次错误密码后 1 次正确密码登录返回 200,随后 `failed_attempts`=0 且 `locked_until` 为空;`locked_until` 过期后下次失败时计数从 1 起。 | automated_test | FR3, FR1 |
| **AC4** | 将某 user 的 `locked_until` 置为 `now()-interval '1 second'`,正确密码登录返回 200;置为 `now()+interval '5 minutes'` 返回锁定 401;判定不依赖测试进程的 `time.Now()`。 | automated_test | FR4 |
| **AC5** | 锁定账户、未锁定+错密码、不存在用户三类失败的 status code + `error.code` + `error.message` 逐字节一致;无任何字段或 header(含 `Retry-After`)透露账户处于锁定态。 | automated_test | FR5, FR2 |
| **AC6** | 锁定账户错密码、active 用户错密码、disabled 用户(对/错密码)、不存在用户四类 login 的 p50/p95 响应时间差小于设定阈值(如 10ms),均经过一次 argon2 等价计算;锁定路径不对真实用户 `password_hash` 执行 argon2(走 `DummyVerify`)。 | automated_test | FR2, FR6 |
| **AC7** | 单实例并发 N 个锁定账户错误密码请求,内存峰值低于实例上限;锁定判定命中后的请求不进入真实用户 hash 的 argon2 计算。 | automated_test | FR2 |
| **AC8** | disabled 用户用正确密码 login 返回与 active 用户错密码逐字节一致的 401;对 disabled 用户的多次失败不改变其 `locked_until`(已禁用不参与锁定计数)。 | automated_test | FR6 |
| **AC9** | 成功 login 响应体 JSON 的 `accessToken`/`refreshToken` 为非空明文(非 `[REDACTED]`),`accessToken` 解码字节长度 ≥32;同一请求的 slog 日志行 grep 不到该 token 明文。 | automated_test | FR8 |
| **AC10** | 构造含已知明文密码的 `loginRequest` 经 `slog.Any` 写入 buffer logger,输出 grep 不到该明文(密码类型 `LogValue`/`MarshalJSON` 返回 redacted);login 触发 401、触发 500、注入 panic 三路径的响应体与 slog 日志均不含明文密码/token。 | automated_test | FR7, FR9 |
| **AC11** | auth 本地与 internal/http 同名 slug 两处字符串值逐字节相等(对账测试,只改一处则失败);任何 W2d 引入的 code 其 401 响应体顶层字段集与现有 400/404/500 一致(`error.code`/`error.message`/`requestId`),现有 envelope/server 断言不被破坏。 | automated_test | FR10 |
| **AC12** | 触发锁定后 stdout slog 出现一条 `event=account_locked` 含 `user_id` + `request_id`,且该行 grep 不到明文密码/token。 | automated_test | FR11 |
| **AC13** | `RateLimiter` 接口存在、no-op 实现恒放行、Handler 默认装 no-op、login 流有调用点;`go build` 过;`verify.sh` 依赖方向 gate 绿(auth 未 import internal/http);grep 全 repo 无 `x/time/rate` 等限流库 import。 | automated_test | FR12 |
| **AC14** | 存在一条文档化的运维解锁手段(cmd 子命令或 CONTRACTS/README 记录的 SQL);对已锁账户执行后,该账户下次正确密码登录返回 200。 | code_review | FR13 |
| **AC15** | W2d commit 的 git diff 不含 `db/migrations/` 任何文件;`bash scripts/verify.sh` 退出码 0(sqlc 漂移 + 迁移 round-trip + 依赖方向 + 全测试)。 | automated_test | NFR2, NFR1 |

---

## 10. Edge Cases and Failure States

- **E1 — account-lockout DoS**:攻击者用受害者用户名错密码锁死对方。→ 接受为已知残留风险(明博硬锁选定),运维解锁手段(FR13)救济 + 文档明示 + 升级触发条件(D1)。
- **E2 — 跨实例并发失败计数竞态**:两实例同时 +1 丢更新。→ 原子 `UPDATE...RETURNING` 单语句行级串行,无丢失(FR1/AC1)。
- **E3 — 锁定态枚举/计时旁路**:据状态码/计时区分"账户被锁/存在"。→ 锁定复用逐字节一致 401 + 走 DummyVerify 等价工作量,不可观测不可利用(FR5/FR2/AC5/AC6)。
- **E4 — 时钟偏移**:`locked_until` 用 app 时钟比致提前解除/永不解除。→ 全程 DB 时钟域判定(FR4/AC4)。
- **E5 — 正常用户误锁**:偶发输错单调累积。→ 成功清零 + 锁过期重计(FR3/AC3)。
- **E6 — token 被 redact 破 wire**:套 Secret 致响应 `[REDACTED]`。→ token 保持裸 string 明文进体(FR8/AC9)。
- **E7 — panic 泄漏密码**:`slog.Any` 反射 `loginRequest`。→ 密码脱敏类型 `LogValue` redact(FR7/FR9/AC10)。
- **E8 — not-found argon2 洪水**:反枚举要求 not-found 也跑 dummy argon2 = DoS 放大,前置拦截拦不住(无账户可锁)。→ 已知风险,靠 `max-instances=2` 兜底账单,将来经 RateLimiter facade 接 IP/全局限流;W2d 不解决(D5 defer 通用限流)。
- **E9 — disabled 用户被爆破**:是否累加计数。→ disabled 走等价 argon2 后统一 401、不累加(FR6/AC8)。
- **E10 — RateLimiter no-op 假安全感**:命名暗示已防护。→ 命名中性 + 文档标"未实装通用限流、no-op 恒放行"(FR12/AC13)。

---

## 11. Risks and Mitigations

| ID | 风险 | 影响/概率 | 缓解 | owner |
|---|---|---|---|---|
| **R1** | account-lockout DoS 残留(攻击者用已知用户名锁死受害者) | medium/medium | 硬锁 + 运维解锁手段(FR13)+ 文档化 + 升级触发条件(公开自助注册/异常 lockout 投诉 → 重评软锁或账户+IP);明博 R3 选定接受 | Mingbo |
| **R2** | not-found 路径 argon2-DoS 放大(反枚举 vs DoS 固有冲突,前置拦截不覆盖) | medium/low | `max-instances=2` 兜底账单,将来经 RateLimiter facade 接 IP/全局限流;W2d 记为已知风险不实装(D5 defer) | engineering |
| **R3** | token 表无清理/行数上限,登录/刷新洪水撑表(正交 DoS 面) | medium/low | defer(超 W2d 范围,属规模型);触发:表增长成本显著 → 加 TTL 清理/每用户行数上限 | engineering |
| **R4** | 原子计数退回"先 SELECT 后 UPDATE"两段式,TOCTOU 跨实例不可修复 | high/low | PRD 钉死单 `UPDATE...RETURNING`(或 FOR UPDATE),禁两段式;AC1 并发测试守 | engineering |
| **R5** | 密码改脱敏类型碰严格解析路径致回归 | medium/low | 用 string 别名本地类型(decoder 级严格解析不受字段类型影响)+ 现有严格解析测试全 case 回归 | engineering |
| **R6** | RateLimiter facade 接口形状投机(将来接真限流可能破接口) | low/medium | 签名预留 `retryAfter` 兼容进程内/分布式;接口未被真实实现验证,显式标"接真实现时允许破坏"(尚未被依赖);或按 YAGNI 砍(见 D9 override) | engineering |

---

## 12. Default Decisions

> 协调者替明博做的可推翻决策。D1 是明博在 A.5 后的 R3 选定(reason 注明);其余按 L1 四路收敛默拍。

- **D1(明博选定)**:账户锁定语义 = **硬锁 + 接受 account-lockout DoS 残留 + 运维解锁手段**。*Why*:明博在锻造中的 R3 选择——硬锁防爆破最强、改动最小、保留 argon2 前置省算(E5)、与已实装反枚举不变量一致;软锁牺牲 E5,账户+IP 破"列已就绪"+依赖不可信 IP + 属 D2 defer 的规模型增强;作为 T2 D2 威胁模型边界的延续。*Override if*:命中升级触发条件(公开自助注册 / 客户规模增长 / 异常 lockout 投诉)→ 重评软锁或账户+IP。
- **D2**:锁定计数 = **DB 侧原子单语句**(`UPDATE...failed_attempts+1...CASE 算锁定窗口...RETURNING`),非 Go 侧读-改-写;保留 `SetUserLock` 仅用于成功清零幂等覆写。*Why*:四路收敛,`SetUserLock` 非原子跨实例丢计数使 D5 主防线在并发下失效;单语句在 DB 侧完成自增/置锁、杜绝 Go 侧读-改-写;实测需 CTE+FOR UPDATE(先 `SELECT...FOR UPDATE` 取旧值再 UPDATE)——裸 `UPDATE...FROM users` 自连接的 old 不参与 EvalPlanQual、读陈旧快照会丢计数(12 并发只数到 2),FOR UPDATE 触发行锁 + EvalPlanQual 重读最新已提交值才正确,与 refresh 的 `GetRefreshTokenBySelectorForUpdate` 同范式(W2d 落地实测修正原'无需 FOR UPDATE'判断)。*Override if*:无(正确性硬要求)。
- **D3**:锁定响应 = **复用统一 401(unauthorized)**,不返 429/不带 Retry-After。*Why*:返 429 让锁定态可枚举,破 W2b 反枚举不变量 + D2 不降级;客户端不消费 429(D13);改动面更小;`rate_limited`/429 留给将来 RateLimiter 实装。*Override if*:明博推翻 D2 反枚举不变量、接受锁定态可枚举换运维可观测性。
- **D4**:锁定路径等价工作量 = **走一次 DummyVerify**(预算 dummyHash)。*Why*:调和 E5(防 argon2 DoS:DummyVerify 用预算 hash 不放大内存、不对真实用户 hash 执行 argon2)与反枚举(计时与普通失败一致);DummyVerify 是现成原语。*Override if*:无。
- **D5**:锁定过期判定 = **单一 DB 时钟域**(`locked_until` 与 `now()` 都 DB 侧),不用 app `time.Now()`。*Why*:沿用 W2c db_now 跨时钟教训,防偏移致提前解除/永不解除;具体实现(`GetUserByUsername` 加 `db_now` 列 vs 锁定判定下推 SQL)留实现 session 实测,两者都满足约束。*Override if*:无。
- **D6**:计数生命周期 = **成功登录清零 + 锁定窗口过期后从 1 重计**(不做滑动时间窗)。*Why*:防正常用户单调累积误锁;成功清零是标准做法且"夹一次成功重置爆破"在单账户爆破场景不成立(攻击者无正确密码);锁过期重计避免永久累积;滑动窗口复杂度无明确收益(硬规则⑤)。*Override if*:出现共享账户 / 凭据部分泄露场景 → 评估滑动时间窗。
- **D7**:密码脱敏 = **auth 本地脱敏类型**(string 别名,`String`/`LogValue`/`MarshalJSON` 三路 redact,默认 `UnmarshalJSON`),不复用 `config.Secret`。*Why*:避免 platform/config 承载业务凭据语义膨胀 + `config.Secret` 无 `UnmarshalJSON` 会碰严格解析;string 别名不破 `DisallowUnknownFields`(decoder 级);三路 redact 挡 `slog.Any` panic 反射——实测 slog 的 JSON handler 反射嵌套 struct 字段走 `json.Marshaler` 不走 `LogValuer`,故 `MarshalJSON` 是堵 panic 泄漏的必需路径(W2d 落地实测修正原'不实现 MarshalJSON'判断),对冻结 wire 无害(`loginRequest` 不进响应)。*Override if*:无。
- **D8**:token 脱敏 = **保持裸 string 明文进响应**,靠"不写进 log/error"。*Why*:token 必须明文进 `LoginSession` 响应给客户端(D13 冻结 wire),套 redact 包装破 wire + 登录回归;现状已不写 token 进日志。*Override if*:无。
- **D9**:RateLimiter facade = **internal/modules/auth 内最小接口**(`Allow(ctx,key)(bool,time.Duration)`)+ no-op 恒放行,不实装通用限流。*Why*:遵 D5/NG10 + 明博 adapter 隔离外部服务原则;签名预留 retryAfter 兼容未来进程内/分布式;成本极小;命名中性不暗示已防护。*Override if*:明博认同 architecture/redteam 的 YAGNI 质疑 → 砍 facade,待真需要时按当时形态新建。
- **D10**:锁定事件可观测 = **slog `event=account_locked`**(user_id+request_id,无凭据);来源 IP defer。*Why*:NFR4 已列锁定事件,锁定是安全事件审计价值明确;IP 字段 auth 不 import http 拿不到,defer。*Override if*:合规要求独立审计表 → 建持久化 audit(承 NFR4 defer 触发)。
- **D11**:**零迁移 + 不加索引**。*Why*:列已就绪,锁定判定复用已取 user row 无范围扫描需求;W2d 纯查询层 + Go 逻辑增量,回滚 = git revert(无 schema 回滚风险)。*Override if*:将来加"列出所有锁定账户"运维查询 → 写 `00003` 新迁移加 `locked_until` 索引(前滚)。
- **D12**:产物路径 = **`product-requirements/t2-auth/w2d-hardening/`**(T2 站下子目录)。*Why*:W2d 属 T2 收尾、产物聚一处;符合明博 args 倾向;低成本可逆。*Override if*:明博要求与各站同级 → 移至 `product-requirements/t2-w2d-hardening/`。
- **D13(BLOCKER1,明博选定)**:四路失败 DB 写时序对齐 —— `no-user`/`locked`/`disabled` 三路各补一次 `TouchUserTiming` 等价主键 UPDATE,与 wrong-pw 的 `RecordLoginFailure` / success 的 `resetLoginFailures` 对齐,使四路失败均为 1 次 argon2 + 1 次主键 UPDATE;`no-user` 用零值 UUID(匹配 0 行)做 dummy 写。*Why*:Codex 异源 review 指出原实现仅 wrong-pw 路径写 DB、其余三路无写,响应时序差泄露用户名是否存在(反枚举信道);明博选「补等价工作量严格对齐」而非「接受 + 留痕」。实测残差:`no-user` 的 0 行 UPDATE 跳过 WAL fsync,比真实 1 行写快约 0.7ms,占 argon2(约 25ms)约 2.7%,远小于 AC6 的 10ms 阈值,被 argon2 量级吸收;未上 sentinel 行池(并发 `no-user` 争同一行锁会制造反向更慢信道 = 更糟)。*Override if*:argon2 成本大幅下调使 0.7ms 残差占比显著,或出现可精密统计数百万样本的离线计时攻击 → 重评(如 `no-user` 改写专用 sentinel 行换 WAL 等价,代价是并发争锁)。

---

## 13. Open Questions

> 本 PRD 无未决 open question。唯一升级项(账户锁定语义)已在锻造中由明博 R3 选定 → 固化为 **D1**。

---

## 14. Implementation Tasks

> 每个 task 是 W2d 实现 session 内一步。`deps` 即施工顺序,`done_when` 引用 §9 AC。**动手前置已全部解决**:锁定语义 D1、原子计数 D2、401 收口 D3、等价工作量 D4、DB 时钟 D5、生命周期 D6、脱敏 D7/D8、facade D9、事件 D10、零迁移 D11 均已定;实现时仍需查当前 OWASP 校准阈值 N/窗口 M(NFR3)。

- **T1**(deps: —;done: AC1, AC2, AC15):新增原子失败计数 sqlc 查询(`RecordLoginFailure :one`,`UPDATE...+1...CASE 锁定窗口...RETURNING`)+ 成功清零查询(`ResetLoginFailures` 或复用 `SetUserLock`)+ DB 时钟解锁方案(`GetUserByUsername` 加 `db_now` 或下推 SQL);`sqlc generate`;零迁移。owner: implementer
- **T2**(deps: T1;done: AC1, AC3, AC4):`verifyCredentials` 接入锁定逻辑——前置锁定拦截(FR2)+ 成功清零(FR3)+ DB 时钟判定(FR4)+ 失败原子自增(FR1);连锁改 store/fakeStore/测试桩。owner: implementer
- **T3**(deps: T2;done: AC5, AC6, AC7):锁定路径等价工作量(走 DummyVerify)+ 锁定/失败/禁用/不存在四路逐字节一致 401 + 计时一致 + 不读真实 hash;锁定响应复用 `unauthorized` 401(FR5)。owner: implementer
- **T4**(deps: T2;done: AC8):login 侧 user status 门(FR6,密码校验后判 status,disabled 等价工作量后统一 401、不累加计数)。owner: implementer
- **T5**(deps: —;done: AC9, AC10):凭据脱敏——密码 auth 本地脱敏类型(FR7,`LogValue` redact、不破严格解析)+ token 保持裸 string 明文进体(FR8)+ panic 审计不变量(FR9)。owner: implementer
- **T6**(deps: T5;done: AC10, AC11):panic/recover 审计测试 + 错误信封 slug 两处对账测试(FR9, FR10)。owner: implementer
- **T7**(deps: T2;done: AC12):锁定事件 slog(FR11,`account_locked`,user_id+request_id,无凭据)。owner: implementer
- **T8**(deps: —;done: AC13):RateLimiter facade 空接缝(FR12,接口 + no-op + Handler 装配 + login 调用点 + 命名中性)。owner: implementer
- **T9**(deps: T1, T2;done: AC14):运维解锁手段(FR13,cmd 子命令或文档化 SQL 清计数)+ 文档。owner: implementer
- **T10**(deps: T2, T3, T4;done: AC1, AC2, AC3, AC4, AC6, AC8):dockerized PG 集成测试(并发原子计数 + 跨实例/重启锁定生效 + DB 时钟解锁 + 四路计时一致 + status 门)+ 单元测试(fakeStore 锁定后行为)。owner: test-runner
- **T11**(deps: T1, T2, T3, T4, T5, T6, T7, T8, T9;done: AC15):`verify.sh` 全绿(sqlc 漂移 + 迁移 round-trip + 依赖方向 + 全测试)+ 零迁移确认。owner: test-runner
- **T12**(deps: T2, T3, T5;done: AC1, AC5, AC10):实现后对高杠杆代码(原子计数 / 锁定等价工作量 / 脱敏类型 / panic 审计)做 Codex 异源 review。owner: Codex

**关键路径**:T1→T2→T3/T4→T10→T11。**并行点**:T5 ‖ T8(都 deps none);T3 ‖ T4 ‖ T7(都依赖 T2)。**明博介入节点**:动手前置已解决(D1-D12);阈值 N/窗口 M 需实现时查 OWASP 校准;运维解锁手段形态(cmd vs 文档化 SQL)实现时定。

---

## 15. 附:本 PRD 的锻造来源(L1 留痕)

L1 五件套(协调者单 message 并行启 sub agent,均 opus 只读):

- **codebase-scan**(design-explorer):核实列已就绪、`SetUserLock` 非原子、login 缺三道门(status/锁定/计数)、错误 code 现集无 429、`config.Secret` 只用于 DSN、`platformlog.Panic` 用 `slog.Any` 直打 panic 值、RateLimiter 零基础。
- **requirement / redteam / implementation / architecture**(design-reviewer):四路高度收敛到——张力A(原子计数,D5 主防线正确性硬要求)、张力B(429 破反枚举 → 收敛 401)、张力C(前置省算计时旁路 → 走 DummyVerify 调和)、张力D(token 不套 Secret 守 wire);并独家挖出草稿漏掉的成功清零、DB 时钟解锁、锁定事件审计三项隐含需求,以及 account-lockout DoS(升 R3)、not-found argon2 洪水、token 表增长三项风险。

**决策路由**:1 个 R3(账户锁定语义,明博 R3 选定 = D1);其余 unknown 走 R1 default + audit(§12 D2-D12);0 个 open question 残留。

*本产物由 design-gate-lite(协调者模式)生成,仅做 PRD 锻造,未写任何实现代码。下游 W2d 落地由明博显式授权后另起 session,建议实现后对高杠杆代码做 Codex 异源 review(T12)。*
