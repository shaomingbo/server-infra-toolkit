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
- **客户端校验(跨 repo)**:客户端仓库 `app-infra-toolkit` 在它自己的 CI 里消费此 schema 校验其解码器(跨 repo pin 由客户端侧负责维护,本仓库不承担客户端 pin 的同步)。
- **真相源归属**:真相源留在服务端(本仓库 `internal/modules/auth/contract/`),客户端为消费方。本次变更为纯 append、非 migration note 级变更(未改写/删除任何既有冻结项,仅新增 schema 与人类摘要的主从约定)。
- **新增测试依赖申报**:conformance 测试引入 `github.com/santhosh-tekuri/jsonschema/v6`(v6.0.2)做 schema 校验,**仅测试引用、不进生产二进制**。实测 `go mod tidy` 后本仓库 go.mod 仅新增这一个直接依赖;其模块图中的传递依赖未被本仓库引用面触及——`golang.org/x/mod`/`x/sys`/`x/text`/`x/tools` 均未因本次进入 go.mod/go.sum(go.mod 既有的 `x/sys`/`x/text` indirect 来自 pgx 链,与本次无关),仅 `github.com/dlclark/regexp2 v1.11.0` 因 jsonschema 自身测试引用它而落了两行 go.sum 校验和,不进 go.mod、不参与本仓库任何构建或测试二进制。
