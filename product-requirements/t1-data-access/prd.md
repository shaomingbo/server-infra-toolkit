# PRD: T1 数据接入层(server-infra-toolkit 第二推进站)

> design-gate-lite 产物 · mode **L1** · slug `t1-data-access`
> 上游:`product-requirements/server-infra-roadmap/prd.md` 的 T1 站(ROADMAP task `T1`,deps `T0`)
> 前序:`product-requirements/t0-online-minimal-loop/prd.md`(T0 线上最小闭环)
> 配套机器可读文件:`handoff.json`(同目录)

---

## 1. Summary

为 server-infra-toolkit(Go 服务端基础设施,独立新 repo)的第二推进站 T1「数据接入层」起草可落地 PRD(mode **L1**)。在 T0 线上最小闭环(HTTP 壳 / 错误信封 / 结构化日志 / 配置密钥注入已就绪)之上,T1 建立服务端**第一个有状态地基**,范围 5 项:① pgxpool 连接池(替换 T0 运行时的裸连接,**保留 `-smoke` 一次性裸连接不动**)② sqlc schema-first 代码生成工作流 ③ 版本化、可回滚(含数据安全边界)的迁移规范 ④ 双缩零(Cloud Run 实例缩零 + Neon 计算缩零)连接池适配 ⑤ CI 增量 gate(sqlc 无漂移 + migration 可跑,只对已落地生效)。落点 `internal/platform/db`,复用 go.mod 既有 `pgx/v5`、`config.Secret`、`NEON_DSN` 约定。明博已确认范围 = **纯 T1**,T0 部署冒烟 + CI 真 runner 跑绿作为 T1 动手前置(待完成,不在本 PRD 实现)。本 PRD 由协调者并行调度 5 个 opus sub agent(1 扫描 + 4 视角批判,redteam 重试一次)综合而成,关键实现细节落 §12 可推翻 default。

---

## 2. Goal Alignment

- **目标用户**:维护者(单人开发 + AI 协作),即 server-infra-toolkit 的规划者兼唯一实现者。次要消费者:T1 落地的 implementer/reviewer;**首个数据层消费者 = T2 auth**(它要用连接池 + 事务写 refresh token)。
- **问题**:ROADMAP 把 T1「数据接入层」排为 T0 之后的下一站,但只有一行表格;需展开成可直接落地的 PRD。同时 codebase-scan 发现 T0 的「真实 Cloud Run/Neon 部署冒烟 + CI 真 runner 跑绿」尚无执行痕迹,需明确它与 T1 的依赖关系(T1 的双缩零适配验收恰恰依赖真实线上环境)。
- **成功长什么样**:T1 有一份可另起 session 直接落地的 PRD——后续任一有状态模块(以 T2 auth 为首个消费者)能 `import internal/platform/db` 拿到一个配置正确、缩零下能自愈重连的连接池,并用 sqlc 生成的类型安全函数查库,不再手写连接管理和 `rows.Scan` 样板;迁移有版本化、可回滚、数据安全的规范;CI 增量 gate 拦住 schema 漂移。T0 部署冒烟 + CI 绿作为动手前置(待完成)。
- **范围边界**:范围 = 纯 T1 数据接入层单站 PRD;**非目标**见 §5(逐条钉死防超做)。不做 T0 部署收尾、不实现业务表、不碰客户端、不改 T0 顶层目录第一层。
- **首个验收信号**:基础设施自验载体经连接池跑通「迁移建表 → sqlc 生成 query → 连接池执行 → 返回预期结果」整链(AC8),且 handoff 校验通过。
- **status**:`selected`——目标层本身清楚(把 ROADMAP 下一站 T1 锻造成 PRD),但 codebase-scan 发现 T0 部署/CI 尾巴悬空,使「下一站对象」存在边界分歧(纯 T1 / T0 已就绪当完整 / 先收 T0)。协调者出 3 选项,明博选「纯 T1·T0 部署作前置」。三张解读卡见 `handoff.goal_alignment.alternatives_considered`,锁定记 §12 D1。

---

## 3. Background and Problem

T0(`product-requirements/t0-online-minimal-loop/prd.md`)已把 server 从空仓库带到「能真实部署上线的最小骨架」:HTTP 壳(`cmd/api/main.go`,含 SIGTERM graceful shutdown)、统一错误信封(`internal/http/errors.go` 的 `WriteError` 单一出口)、结构化日志(`internal/platform/log`,slog JSON,字段 `request_id/method/path/status/latency_ms/version`)、配置与密钥注入(`internal/platform/config`,单一 `Config{Port,Version,DSN}`,`os.Getenv` 唯一读取面,`Secret` 类型三重脱敏 `String()/MarshalJSON()/LogValue()`)。

**T0 的现状(codebase-scan 核实)**:代码维度 ①②③④ 四项 + 本地测试全部到位;但 ⑤「CI 在 GitHub Actions 真 runner 跑绿一次」与 ⑥「一次真实 Cloud Run/Neon 部署冒烟」**只有脚手架**(`scripts/smoke.sh`、`docs/DEPLOY.md` runbook、`-smoke` 子命令、`Dockerfile`)、**无任何执行痕迹**(git 仅 3 个 commit)。明博已确认:T1 照常锻造,把 ⑤⑥ 作为 T1 动手前的待完成前置。

**为什么 T1 是数据接入层而非直接做某对端**:ROADMAP §7.1 的 ROI 排序——数据接入层是「所有有状态模块前置」,T2 auth / T4 offline / T5 observability 都要落库,连接池/sqlc/迁移是它们共同的地基。先建地基再建对端,避免每个对端各自搭一套连接管理。

**当前 T1 起点(codebase-scan)**:`pgx/v5 v5.10.0` 已在 go.mod,但只用于 `internal/platform/db/db.go` 的 `Smoke(ctx,dsn)` 一次性裸连接(包注释自标 "T0 smoke only; pooling/codegen/migrations/schema all live in T1");`db/migrations/`、`sql/`、sqlc 全是空白,T1 从零建。

---

## 4. Users and Use Cases

- **主要用户**:明博(按本 PRD 把数据接入地基从零建出来,作为 T2+ 所有有状态站的复用基础)。
- **次要消费者**:T1 落地的 implementer/reviewer/test-runner;**首个真实消费者 T2 auth**(本 PRD 的接口设计以「T2 能否平滑接入且不被逼破接口」为校验基准)。
- 用例:
  1. 明博照本 PRD 的 task 序列,从零建连接池 + sqlc 工作流 + 迁移规范,用基础设施自验载体打通整链。
  2. T2 起每个有状态站 `import internal/platform/db` 拿连接池(经窄接口注入)、写 `sql/` 查询经 sqlc 生成类型安全代码、加迁移文件走既定规范,不重搭连接管理。
  3. schema 演进时:写迁移(单一真相源)→ sqlc generate → CI 拦漂移;破坏性变更按 down 数据安全规范分步处理。
  4. 部署时:迁移作为部署前独立步骤执行(与服务进程解耦),服务进程只连不迁。

---

## 5. Goals and Non-goals

**Goals**(见 `handoff.goals`,价值导向——回应 requirement「勿把手段当目标」):
- **G1**:服务端获得配置正确、缩零下能自愈重连的 pgxpool 连接池,替换 T0 运行时裸连接,经窄接口供后续模块(首个 = T2 auth)消费。*价值*:T2+ 不再各自管连接生命周期。
- **G2**:建立 sqlc schema-first 工作流,写 SQL 即得类型安全 Go 查询代码,编译期捕获列名/类型错配。*价值*:免手写易错的 `rows.Scan` 样板。
- **G3**:建立版本化、可回滚(含数据安全边界)的迁移规范 + 目录布局,作为 T2+ schema 演进地基。*价值*:schema 变更有据可查、可控撤销。
- **G4**:连接池在双缩零下,首请求经有限重试返回成功响应,不抛连接错误、不连接风暴。*价值*:缩零省成本的同时不牺牲首请求可用性。
- **G5**:CI 增量加 sqlc 无漂移 + migration 可跑 gate(只对已落地生效、不空跑)。*价值*:schema 与生成代码的不一致在合并前被拦。

**Non-goals**(逐条钉死;`requirement`/`redteam` 共识:T1 真正高发的是**超做** + **范围与 T2/T6 串味**):
- **NG1**:不实现业务域表/查询(auth→T2、offline→T4、observability→T5);仅建数据接入能力本身 + 一张**基础设施自验载体**(非业务表,见 §12 D4)。
- **NG2**:不碰客户端 `app-infra-toolkit`。
- **NG3**:不改 T0 已落地的顶层目录第一层(`cmd/`、`internal/` 现有子目录);新建 ROADMAP §7.3 已冻结画出的 `db/migrations/`、`sql/` **不算违反**(它们在顶层目录契约里已规划),sqlc 生成物落 `internal/platform/db/` 子树内、不新开 `internal/` 顶层子目录。
- **NG4**:不给 `/livez` 加 DB 探测/ping(继承 T0 AC11;Neon 缩零下探 DB 会让健康检查失败 → Cloud Run 杀实例)。
- **NG5**:不做 ORM(坚持 pgx + sqlc 显式 SQL)。
- **NG6**:不做契约层 spec→Go DTO 生成器(属 **T3**);**sqlc 是 SQL→Go 查询代码生成器,与 T3 不同物,在范围内**(消歧:`internal/platform/db/gen/` 是 SQL→Go,来源本 repo `sql/`;`internal/contracts/generated/` 是 spec→Go,来源客户端 spec,属 T3)。
- **NG7**:**不在服务进程启动路径自动跑迁移**(Cloud Run 缩零会并发拉起多实例 → 迁移竞争 + 首请求被迁移阻塞);迁移与服务进程解耦。
- **NG8**:不通过 keep-alive/连接预热对抗缩零(那会阻止 Neon 计算缩零、增加账单);冷启动靠首请求重连吸收。
- **NG9**:不做连接 draining / in-flight 查询排空编排 / readiness 探针(属 **T6**);T1 关池 = HTTP Shutdown 完成后 `pool.Close()` 最小接入。

---

## 6. Requirements

### 功能性(FR)

- **FR1 — 连接池替换运行时裸连接**:用 `pgxpool` 在 `internal/platform/db` 建连接池,替换服务运行时(HTTP 壳路径)的连接获取;**保留 T0 的 `-smoke` 子命令及其 `db.Smoke` 一次性裸连接不变**(它是部署冒烟基础设施,非「待替换的业务用法」;删它破坏 T0 AC10/AC12 与 `never break userspace`)。两条路径在代码注释中区分用途。
- **FR2 — 池参数经 DSN query string 注入**:连接池参数(`pool_max_conns` / `pool_min_conns` / `pool_max_conn_lifetime` / `pool_max_conn_idle_time` / `pool_health_check_period` 等,**确切参数名实现时核实 pgxpool 当前文档**)通过 `NEON_DSN` 的 query string 配置(pgxpool `ParseConfig` 原生解析),不新开 env 读取面、不破坏 T0 `Config` 三字段克制设计(`config.go` 注释明示「intentionally three fields」);DSN 仍是 `config.Secret`(NFR3);DSN 未指定的参数在 db 包内取缩零友好默认值。
- **FR3 — 窄接口,具体类型不穿模块边界**:db 包对外只导出最小接口(对齐 sqlc 的 `DBTX`:`Exec`/`Query`/`QueryRow`,并预留事务入口 `Begin` 供 T2);`*pgxpool.Pool` 具体类型只在 db 包内部与 `main.go` 组装处出现,**不进 `internal/modules/*` 的构造签名**(否则 T2-T5 接入后换连接实现即破 userspace);连接池所有权归 `main.go`(建池/关池),上层注入消费。
- **FR4 — 连接数护栏防风暴**:`pool_max_conns` 有显式保守默认值,**远小于 Neon 套餐连接上限**(防缩零唤醒瞬间并发建连打爆 Neon — ROADMAP R5);PRD/代码给出「查 Neon 当前套餐连接上限」的步骤,默认值 × 预期最大 Cloud Run 实例数 ≤ Neon 上限。
- **FR5 — sqlc 工作流**:`sqlc.yaml`(`schema` 路径指向迁移文件目录,使迁移成为 sqlc 的**单一 schema 真相源** — NFR6)+ `sql/` 源查询 + 生成代码落 `internal/platform/db/gen/`(带 sqlc 标准生成标记 `// Code generated by sqlc. DO NOT EDIT.`);sqlc 版本经 Go 1.25 `go tool` 机制锁定(go.mod `tool` 指令,**Go 1.25 是否支持该机制实现时核实**;不支持则回退 tools.go 模式),`verify.sh` 用 `go tool sqlc`(或等价锁版本调用)而非全局安装的 `sqlc`;sqlc 配置 `sql_package` 指向 `pgx/v5` 且与已锁 `v5.10.0` 兼容(实现时核实)。
- **FR6 — 迁移规范**:选定迁移工具(默认 golang-migrate,见 Q1)+ `db/migrations/` 目录布局 + 命名约定 + up/down;**每个迁移在事务内执行**(Postgres 支持事务 DDL)以保证半途失败可原子回退,并写明所选工具的「脏状态(dirty)」恢复步骤;**down 适用边界明确**——down 用于本地/未发布迁移的撤销,生产回滚默认走「前滚修复」而非跑 down;**破坏性 down(`DROP COLUMN`/`DROP TABLE`)须标 `IRREVERSIBLE` 注释且不入常规回滚流程**(防数据不可逆丢失)。
- **FR7 — 基础设施自验载体**:定义一张自验载体打通「迁移建 schema → sqlc 消费 → 连接池执行」整链——复用迁移工具自带的版本表(如 `schema_migrations`),或一张明确标注「基础设施自验、非业务表」的自检表;配一条 sqlc query 对它生成类型安全代码,经连接池执行返回预期结果。**此载体不违反 NG1**(非业务域表;若用迁移版本表则零额外 schema 残留)。**禁止用 `SELECT 1`/系统目录 query 替代**(那切断「迁移产出 schema → sqlc 消费 schema」的验证闭环)。
- **FR8 — 迁移触发与服务进程解耦**:T1 交付迁移触发**机制设计**(部署前独立步骤 / 手动命令,嵌入 `docs/DEPLOY.md` 的 build→deploy 流程之前)+ 在本地与 CI dockerized Postgres 上**可跑**;**真实线上实跑验证 defer 到 T0 部署链通之后**(T1 动手时线上环境可能尚未就绪)。服务镜像不承担 schema 演进职责(NG7)。迁移凭据复用 `NEON_DSN`,部署期在 CI/runner 经受控 secret 注入,遵守 `config.Secret` 脱敏纪律。
- **FR9 — 连接池生命周期接入 main.go**:启动时建池,采用 **lazy 连接**(不在启动路径阻塞等待 DB 醒来,避免缩零唤醒拖慢冷启动 + 把 DB 可用性耦合进 liveness);SIGTERM 时,**在 `srv.Shutdown(ctx)` 返回之后才 `pool.Close()`**(先停收新请求并排空在途,再关池,避免在途查询拿到已关闭的池),总时长受 Cloud Run SIGTERM grace 约束(**具体 grace 值实现时核实**)。
- **FR10 — 双缩零首请求重连重试**:缩零唤醒后首请求经**有限**重试返回成功响应。重连归属分层——「首请求冷启动重连」靠 pgxpool 连接获取层 + 合理连接超时;「业务路径瞬时故障」最多一次有界重试 + 短退避,**封装在 db 包内部、不暴露于接口签名、不下沉到每个业务 handler**;重试逻辑**不得**放在 `/livez` 或服务启动阻塞路径(继承 NG4)。重试次数/退避/总超时有文档化数值(总重试窗口 > Neon 典型唤醒延迟且 < Cloud Run 请求超时,**具体数值实现时核实当前文档**);连接彻底不可用时经 T0 `WriteError` 信封返回 5xx 而非 panic。
- **FR11 — CI 增量 gate**:`scripts/verify.sh` 增量加两 gate,用**配置存在性门控**(`[ -f sqlc.yaml ]` 才跑 sqlc gate、`db/migrations/` 非空才跑 migration gate;跳过时在 CI 日志**显式打印跳过原因**防静默假绿):① **sqlc 无漂移** = `go tool sqlc generate` 后 `git diff --exit-code` 生成落点,非空即 fail;② **migration 可跑** = 在 **CI 内 dockerized Postgres**(GitHub Actions service container)上跑 `up` 后 `down`,断言双向成功,**不连 Neon**(对齐 T0「PR gate 不连远端 flaky」)。新 gate 对 T1 自身落地的产物在真 CI runner 跑绿一次(对齐 ROADMAP「加 gate 即绿」)。

### 非功能(横切约束,T1 适用项)

- **NFR1 — 迁移可回滚 + 无常驻进程**:迁移可回滚(含 §FR6 数据安全边界);**服务进程启动路径不执行迁移**(对齐 ROADMAP NFR1 缩零无常驻进程 / NFR3 缩零迁移触发明确)。
- **NFR2 — 包依赖方向**:`internal/platform/db` 及其新增依赖闭包不得 import `internal/http` 与 `internal/modules`(继承 T0 NFR3,`verify.sh` 依赖方向检查 CI 强制);连接池消费方向为「上层(http/main 注入)调用 db」,据此约束 FR3 接口设计。
- **NFR3 — 密钥纪律**:连接池/DSN 中的敏感项复用 `config.Secret` 类型脱敏,不新增明文 secret 读取面(继承 T0 NFR2)。
- **NFR4 — 连接池可观测**:经 T0 既有 slog JSON 输出——连接池关键事件(重连发生 / 重试发生含次数 / 获取连接失败)+ 必要时 `pool.Stat()` 指标(如 `AcquiredConns`/`IdleConns`/`MaxConns`,**确切字段名实现时核实 pgxpool API**),沿用既有字段命名风格(注意实际日志字段名是 `latency_ms`);**不得以 `/livez` 探 DB 实现可观测**(继承 NG4)。
- **NFR5 — livez 不探 DB**:`/livez` 保持不发起 DB 连接(继承 T0 AC11;连接池引入后尤其要钉死这条,否则破 AC11 + Neon 缩零挂健康检查)。
- **NFR6 — schema 单一真相源**:迁移文件为唯一 schema 来源,sqlc 从它读、迁移向它写,**不引入第二份 schema 定义**(排除声明式 schema-as-code 工具如 atlas — 会与 sqlc 的 schema 文件形成两个真相源、长期必漂移)。
- **NFR7 — 训练数据时效性闸门**:所有 pgxpool 参数默认值 / DSN 参数名 / Neon 限额与缩零行为 / Cloud Run 冷启动与 grace 数值,**实现时以当前官方文档核实,PRD 与代码不写死凭记忆的魔数**(呼应 ROADMAP R1「AI 生成 Go 难严审」+ 明博「训练数据有时效性」纪律)。
- **NFR8 — 双缩零分层验收**:G4/FR10 的验收拆「可立即验」(重试逻辑单测,mock 首次失败→重试→成功)与「必须真实线上验」(真缩零唤醒后首请求成功);**线上部分在 T0 部署链未就绪时显式标 deferred,禁止用本地/单测绿冒充线上绿声称双缩零已验**。
- **NFR9 — 独立验收/回滚 + 回滚语义澄清**:本站 PRD 可独立验收/回滚/合并;PRD 显式记录「**代码回滚 ≠ schema 回滚**」——`git revert` T1 代码不会撤销已在 Neon 跑过的迁移,schema 与代码会脱节,需独立 down 处理。

---

## 7. User Flow / State Flow

### 7.1 服务运行时连接获取流(每个有状态请求,T2 起真实使用)

```
入站请求(经 T0 中间件链 recover→request-id→access-log)
  → 业务 handler(T2+) 经注入的窄接口(DBTX)向 db 包请求执行
      → pgxpool.Acquire 拿连接
          ├ 池中有活连接 → 直接用
          ├ 缩零唤醒/死连接 → pgxpool 连接层重连(+ 连接超时)
          │     └ 首请求有界重试 + 短退避(封装在 db 包内,对 handler 透明)
          │           ├ 重连成功 → 执行查询 → 返回
          │           └ 超出重试窗口/彻底不可用 → 经 WriteError 返 5xx(不 panic)
      → 用完连接归还池
  /livez 永不走此流(不探 DB,NFR5)
```

### 7.2 schema 演进 / 迁移状态流

| 状态 | 触发 | 下一步 |
|---|---|---|
| 写迁移 | 在 `db/migrations/` 加 up/down 文件(命名约定),每个迁移事务内 | sqlc generate |
| sqlc generate | `go tool sqlc generate`(schema 指向迁移文件) | 本地编译 + CI 漂移检查 |
| CI sqlc gate | `sqlc generate` 后 `git diff --exit-code` | 非空 → 红(漂移);空 → 过 |
| CI migration gate | dockerized Postgres 跑 `up` → `down` | 双向成功 → 过;失败 → 红 |
| 部署期迁移 | 部署前独立步骤(非服务进程启动),连 Neon `up` | 成功 → 部署服务镜像;失败 → 停部署 |
| 迁移半途失败 | 某条 DDL 失败 | 事务回滚到该迁移前;按工具 dirty 恢复步骤处理 |
| 回滚 schema | 本地/未发布:`down`;生产:**前滚修复**(默认不跑 down);破坏性变更:标 IRREVERSIBLE 不入常规回滚 | — |

---

## 8. Data, API, Permissions

### Data

- T1 **不定义业务域持久化实体**。唯一 schema 产物 = **基础设施自验载体**(FR7):迁移工具版本表(如 `schema_migrations`)或一张标注「基础设施自验、非业务、T2 起可替换」的自检表,+ 一条对它的 sqlc query。
- schema 单一真相源 = `db/migrations/` 累积的 DDL(NFR6);sqlc `schema` 配置指向它,不维护第二份 schema.sql。

### API

- T1 **不新增 HTTP 端点**。连接池是内部能力,经窄接口被 T2+ handler 消费,不直接对外。
- `/livez` 行为不变(不探 DB,NFR5)。

### Permissions / Secrets

- 连接复用 `NEON_DSN`(T0 已定:env 名 `NEON_DSN`,格式 `postgres://USER:PASS@HOST.neon.tech/DB?sslmode=require`,生产 Secret Manager `--set-secrets` 注入)。**pooler 端点 vs 直连端点二选一**(影响连接数语义,见 Q2,T0 §8 已留此坑)。
- **迁移凭据**(FR8):部署期在 CI/runner 跑迁移连 Neon,复用 `NEON_DSN`,经受控 secret(如 GitHub Actions secret)注入,遵守 `config.Secret` 脱敏纪律。CI 的 migration gate 用 dockerized Postgres,**不需要** Neon 凭据。
- 池参数中无新增 secret(参数走 DSN query string,DSN 整体已是 `config.Secret`)。

---

## 9. Acceptance Criteria

> 来源 A = sub agent 标 `Verifiable: yes` 的 finding 转 AC;来源 B = 协调者补的核心 done criteria。每条引用存在的 FR/NFR,description 不含模糊词黑名单。本表 AC 编号是 T1 PRD 独立命名空间(引用 T0 约束时显式标「T0 AC*」)。

| ID | 验收标准 | 验收方式 | 关联 |
|---|---|---|---|
| **AC1** | 服务运行时路径(`run()` / HTTP 壳)经 `grep` 不出现裸 `pgx.Connect`,只经连接池获取连接;`-smoke` 子命令仍走一次性裸连接 `SELECT 1`、行为与 T0 一致;两条路径在代码注释中区分用途。 | code_review | FR1 |
| **AC2** | 连接池参数通过 `NEON_DSN` 的 query string 传入(代码经 pgxpool `ParseConfig` 解析);`config.Config` 字段集相对 T0 不新增(仍为三字段)或新增字段在 PRD 有显式记录理由;DSN 未指定的池参数在 db 包内有默认值;DSN 仍为 `config.Secret` 类型。 | code_review | FR2, NFR3 |
| **AC3** | `grep -r 'pgxpool.Pool' internal/modules/` 为空(具体类型不穿模块边界);db 包导出的连接获取接口含 `Exec`/`Query`/`QueryRow`(并预留 `Begin`);`*pgxpool.Pool` 仅出现在 db 包内部与 `main.go`。 | code_review | FR3 |
| **AC4** | `pool_max_conns` 默认值显式设置且小于 Neon 套餐连接上限(不依赖 pgxpool 库默认值);PRD/代码注释写明查 Neon 当前套餐上限的步骤,并标注「默认值 × 预期最大实例数 ≤ 上限」;涉及的 Neon 限额数值标「实现时核实当前官方文档」。 | code_review | FR4, NFR7 |
| **AC5** | sqlc 生成代码落在 `internal/platform/db/` 子树内、含 `// Code generated by sqlc. DO NOT EDIT.` 标记;`internal/` 顶层子目录集合相对 ROADMAP §7.3 树无新增;`sqlc.yaml` 的 `schema` 路径指向迁移文件目录,不存在独立于迁移的第二份 schema DDL。 | code_review | FR5, NFR6 |
| **AC6** | sqlc 版本锁定在 go.mod(`tool` 指令或等价 tools.go);`verify.sh` 中调用 `go tool sqlc`(或等价锁版本方式)而非全局安装的 `sqlc`,CI 与本地用同一 sqlc 版本。 | code_review | FR5 |
| **AC7** | 迁移规范文档写明:每个迁移在事务内执行(除显式标注不可事务化的 DDL)+ 所选工具的 dirty 状态恢复步骤;down 适用边界(本地/未发布撤销 vs 生产前滚修复);破坏性 down 须标 `IRREVERSIBLE` 且不入常规回滚流程。 | code_review | FR6, NFR1 |
| **AC8** | 存在一对 up/down 迁移建立自验载体;存在一条 sqlc query 引用该载体并生成可编译 Go 代码;存在测试经连接池执行该 query 返回预期结果(可在本地/CI dockerized Postgres 跑);该载体注释标明为基础设施自验用途。 | automated_test | FR7 |
| **AC9** | 服务进程启动路径(`main.go`/`run()`)经 `grep` 不含任何迁移调用(`migrate`/`Up()`/`goose up` 等);迁移触发为部署流程中独立于应用启动的一步,体现在 `docs/DEPLOY.md` 的部署前步骤。 | code_review | FR8, NFR1 |
| **AC10** | 进程启动不阻塞等待 DB 连接成功(连接池 lazy 连接,启动期不强制 ping);shutdown 路径中 `pool.Close()` 在 `srv.Shutdown(ctx)` 返回之后调用;池实例经构造参数注入,不经全局变量。 | code_review | FR9 |
| **AC11** | `/livez` handler 依赖闭包不含 db 包(`go list -deps` 校验,继承 T0 AC11);双缩零重连/重试逻辑经 `grep` 不出现在 livez 或启动阻塞路径;Neon 缩零时 `/livez` 仍返回 200(后者记 manual,见 AC15 分层)。 | code_review | FR10, NFR5 |
| **AC12** | 删除 `sqlc.yaml`/`db/migrations` 后两 gate 被跳过且 CI 仍绿、日志显式打印跳过原因(证明不空跑);手改一处 sqlc 生成物或改 SQL 不重跑 generate → CI 红(证明 gate 通电);migration gate 对 dockerized Postgres 跑 `up`+`down` 双向断言成功。 | automated_test | FR11 |
| **AC13** | `verify.sh` 及 `.github/workflows/*.yml` 经 `grep` 不出现 `*.neon.tech` 或 Neon DSN;CI 的 migration 校验使用 workflow 内 dockerized Postgres(service container),`verify.sh` 连 `localhost` PG。 | code_review | FR11 |
| **AC14** | db 包及其新增依赖闭包经 `verify.sh` 的依赖方向检查通过(不含 `internal/http`/`internal/modules`);对故意让 db 包 import `internal/http` 的改动,`verify.sh` 报红。 | automated_test | NFR2 |
| **AC15** | FR10 重试逻辑有单元测试覆盖「首次连接失败 → 重试 → 成功」路径,断言重试次数上限与退避;真实 Cloud Run+Neon 缩零后首请求成功标为 manual_check,且在 T0 部署链未就绪时显式标 deferred,不用单测绿代替线上绿声称双缩零已验。 | automated_test | FR10, NFR8 |
| **AC16** | 连接池重连/重试/获取失败事件产生 slog JSON 日志(字段命名对齐 T0 既有风格,如含事件类型与重试次数);该日志走 T0 既有 slog 出口,不新增 metrics HTTP 端点;`/livez` 仍 `grep` 不到 db/pool import 或 Ping。 | code_review | NFR4 |
| **AC17** | PRD 含「代码回滚 ≠ schema 回滚」说明(git revert 代码不撤已跑迁移,需独立 down);本站交付物可独立 `git revert` 且不破坏 T0 既有测试(回滚后 `verify.sh` 仍绿)。 | manual_check | NFR9 |

---

## 10. Edge Cases and Failure States

- **E1 — 缩零唤醒连接风暴**:Cloud Run 缩零唤醒瞬间多请求并发建连 → 若 `pool_max_conns` 设成教程默认的几十上百,直接超 Neon 套餐上限 → Neon 拒连、首请求重试也连不上。缓解:FR4 保守 MaxConns 护栏 + FR10 退避;NG8 不做保活。
- **E2 — 迁移多实例并发竞争**:若迁移挂在服务启动路径,缩零并发拉起 N 个实例 = N 个迁移并发跑同一套 up → 竞争/部分失败/启动阻塞。缓解:NG7 启动路径不跑迁移 + FR8 触发解耦。
- **E3 — 迁移半途失败/脏状态**:一个迁移含多条 DDL,跑到第二条失败 → schema 半应用、版本表标 dirty。缓解:FR6 每迁移事务内执行(原子)+ 文档化所选工具的 dirty 恢复步骤。
- **E4 — down 迁移破坏性丢数据**:`DROP COLUMN` 的 down 语法「可回滚」但数据不可逆丢失;或生产回滚部署时顺手跑 down。缓解:FR6 破坏性 down 标 `IRREVERSIBLE` + 生产默认前滚修复不跑 down。
- **E5 — 本地无缩零环境致双缩零假绿**:FR10 在本地(无 Cloud Run/Neon 缩零)跑永远绿,但线上可能照炸。缓解:NFR8 分层验收,线上部分 deferred、不用本地绿冒充。
- **E6 — SIGTERM 关池打断 in-flight 查询**:若 `pool.Close()` 在 HTTP drain 之前或并发,在途请求拿到已关闭的池。缓解:FR9 关池在 `srv.Shutdown` 返回之后。
- **E7 — 代码回滚但 schema 已迁移**:`git revert` T1 代码不会撤销已在 Neon 跑过的迁移 → schema 与代码脱节。缓解:NFR9 显式记录该区分,schema 回退走独立 down。
- **E8 — CI gate 空跑假绿**:T1 落地前 sqlc/迁移为空时 gate 形同虚设。缓解:FR11 配置存在性门控 + 跳过打印原因 + 通电测试(改坏即红);T1 落地后 gate 对自身产物激活。
- **E9 — sqlc query 引用尚不存在的表(工作流顺序搞反)**:先写 sqlc 查询引用还没迁移建的表 → generate 失败。缓解:迁移规范明确「先迁移建 schema 再 sqlc generate」的工作流顺序。

---

## 11. Risks and Mitigations

| ID | 风险 | 影响/概率 | 缓解 | owner |
|---|---|---|---|---|
| **R1** | Go 新手 + AI 生成在连接池/迁移/缩零等高杠杆环节判断不足,易凭过时记忆写错 pgxpool 默认值/Neon 限额(ROADMAP R1) | high/medium | NFR7 训练数据时效性闸门(实现时查文档不写魔数)+ FR3 窄接口 + NFR8 分层验收 + 实现后异源 review(Codex)+ FR7 自验载体打通真实链路 | Mingbo |
| **R2** | 连接风暴打爆 Neon 套餐连接上限 | high/medium | FR4 保守 MaxConns 护栏 + 查套餐上限步骤 + FR10 退避;NG8 不保活 | engineering |
| **R3** | 双缩零假绿(本地/单测绿冒充线上验证) | medium/high | NFR8 分层验收,线上部分显式 deferred;AC15 禁单测代替线上 | engineering |
| **R4** | 迁移 down 误跑致生产数据不可逆丢失 | high/low | FR6 破坏性 down 标 IRREVERSIBLE + 生产前滚默认 + 每迁移事务执行 | engineering |
| **R5** | 迁移工具选型 lock-in(被 T2+ 继承,改它返工所有迁移) | medium/low | T1 阶段迁移文件极少切换成本低 + NFR6 排除 atlas + Q1 给 default 留推翻口 | Mingbo |
| **R6** | schema 漂移(sqlc 生成与实际 schema 不一致致运行时错) | medium/medium | NFR6 schema 单一真相源 + FR11 sqlc 无漂移 gate 通电(改坏即红) | engineering |

---

## 12. Default Decisions

> 协调者替明博做的可推翻决策。D1 是明博在范围选择题里选定的(扫描发现 T0 ⑤⑥ 悬空),其余是协调者按 Brief + 五路调研 + T0 既有约定默拍。

- **D1**:下一站范围 = **纯 T1 数据接入层**,T0 部署冒烟 + CI 真 runner 跑绿作为 T1 动手前置(待完成,不在本 PRD 实现)。*Why*:codebase-scan 发现 T0 ⑤⑥ 仅有脚手架无执行痕迹,协调者出 3 选项,明博选「纯 T1·T0 部署作前置」。*Override if*:明博改为先收口 T0 线上闭环,则重锻造为 T0 收尾 PRD、T1 后排。
- **D2**:迁移触发**时序解耦**——T1 交付迁移规范 + 本地/dockerized 可跑 + 触发机制设计(部署前独立步骤),真实线上实跑 defer 到 T0 部署链通后。*Why*:五路共识 + redteam 洞见——T0 线上未就绪时,四个触发选项(启动自动/部署前CI/单独Job/启动加锁)集体受限;时序解耦让 T1 不被线上环境阻塞。*Override if*:T0 部署链已通,则迁移实跑验证并入 T1。
- **D3**:池参数走 `NEON_DSN` query string(pgxpool 原生解析),不扩 `config.Config` 三字段。*Why*:architecture 推荐——保 T0 config 克制设计 + 不新开 env 面,参数与连接串同源、Secret 包裹不变。*Override if*:某参数无法经 DSN 表达,则扩 Config 字段并记录突破理由。
- **D4**:基础设施自验载体复用迁移工具版本表(如 `schema_migrations`)或一张标注的自检表,配一条 sqlc query。*Why*:综合 implementation/requirement(要真实表才能验「迁移→sqlc」闭环)+ architecture(无业务 down 债);用版本表则零额外 schema 残留。*Override if*:所选工具版本表不适合做 sqlc 样本,则建标注的临时自检表 + 在 T2 前置清理。
- **D5**:sqlc 生成代码落 `internal/platform/db/gen/`。*Why*:复用既有 `platform/db` 路径、不新开 `internal/` 顶层子目录(对齐 ROADMAP §7.3 树 + T0 NG3);与 T3 `internal/contracts/generated/`(spec→Go)按来源区分。*Override if*:多 schema 需不同布局。
- **D6**:CI 的 migration 校验跑 dockerized Postgres,不连 Neon。*Why*:对齐 T0「PR gate 不连远端 flaky」原则(T0 §15);Neon 冷启动 + 凭据会拖垮确定性。*Override if*:无(远端验证作独立/部署期 job,不进 PR gate)。
- **D7**:sqlc 版本经 Go 1.25 `go tool` 机制锁定(go.mod `tool` 指令)。*Why*:与 ROADMAP/T0 的 Go 版本三处锁定可复现哲学一致,CI 与本地同版本、无需全局安装。*Override if*:实测 Go 1.25 不支持 `go tool` 锁 sqlc,则回退 tools.go + `go run` 模式。
- **D8**:保留 T0 的 `-smoke`/`db.Smoke` 一次性裸连接不动,FR1 只替换服务运行时的连接获取。*Why*:`-smoke` 是 T0 部署冒烟基础设施(T0 AC10/AC12 依赖),`never break userspace`;冒烟要「最便宜证明 DSN+网络通」,起池子反而更重。*Override if*:无。

---

## 13. Open Questions

> ≤3 条,仅留真正 Mingbo-owned(明博偏好 / 明博云账号实际状态),均给推荐默认、不打断锻造。

- **Q1**:迁移工具最终选型?
  - *为什么是明博的*:这是会被 T2-T8 所有有状态站继承的长期工具链 lock-in;且维护者可能有 Prisma 声明式 / knex 命令式的迁移心智偏好,工具的「手感」是个人运维偏好。
  - *推荐默认*:**golang-migrate**(纯 SQL up/down、生态最大、CLI+库两用、Neon 兼容,与 sqlc 的纯 SQL 心智一致);**已排除 atlas**(声明式会引入第二份 schema 真相源,违 NFR6)。候选 goose/tern(jackc,与 pgx 同源)亦可。此项在 T1 动手时定。
- **Q2**:Neon 当前套餐的连接数上限 + 用 pooler 端点还是直连端点?
  - *为什么是明博的*:套餐连接上限是明博云账号的实际状态,协调者无法查;pooler vs 直连端点的连接语义不同(影响 MaxConns 取值),T0 §8 已留此二选一坑。
  - *推荐默认*:实现前确认当前套餐连接上限,`pool_max_conns` 取保守值(使 × 预期最大实例数 ≤ 上限);端点 pooler/直连二选一(缩零场景下二者唤醒/连接复用语义有别,实现时核实当前 Neon 文档)。并入 T1 动手前置 checklist。

---

## 14. Implementation Tasks

> 每个 task 是 T1 实现 session 内的一步。`deps` 即施工顺序,`done_when` 引用 §9 的 AC 作为验收闸。owner 主力 `implementer`(opus),整链测试 `test-runner`。**动手前置**:确认 T0 部署冒烟+CI绿状态(D1)、Q1 迁移工具、Q2 Neon 套餐上限+端点。

- **T1**(deps: —;done: AC7):选定迁移工具(Q1)+ 建 `db/migrations/` 布局与命名约定 + 写基础设施自验载体的首对 up/down 迁移(事务内执行;down 数据安全规范:IRREVERSIBLE 标注 + 生产前滚)。
- **T2**(deps: T1;done: AC5, AC6):配 `sqlc.yaml`(`schema` 指向迁移文件,单一真相源)+ `go tool` 锁 sqlc 版本 + 写自验 query + 生成代码到 `internal/platform/db/gen/`(带生成标记)。
- **T3**(deps: T1;done: AC1, AC2, AC3, AC4, AC10):pgxpool 连接池替换运行时裸连接(**保留 `-smoke`**)+ 池参数走 DSN query string + MaxConns 护栏 + 窄接口(DBTX+预留 Begin)+ `main.go` lazy 建池 + 关池在 `srv.Shutdown` 后。
- **T4**(deps: T3;done: AC11, AC15, AC16):双缩零首请求重连重试(封装在 db 包,不入 livez/启动)+ 重试逻辑单元测试(mock 失败→重试→成功)+ 连接池可观测 slog 事件 + livez 不探 DB 守护断言。
- **T5**(deps: T1, T2, T3;done: AC12, AC13, AC14):`verify.sh` 加 sqlc/migration 增量 gate(配置存在性门控 + sqlc diff + dockerized Postgres up/down + 不连 Neon)+ workflow 加 service container + db 包依赖方向自验。
- **T6**(deps: T3, T5;done: AC8):整链 automated_test——自验载体经连接池执行 sqlc query 返回预期结果;跑 `verify.sh` 全绿(新 gate 对 T1 自身产物激活并通过)。
- **T7**(deps: T3, T4;done: AC9, AC17):`docs/DEPLOY.md` 加迁移触发步骤(部署前独立步骤)+ 双缩零 L2 manual 验收 runbook + 「代码回滚 ≠ schema 回滚」说明;标注 deferred 项(线上实跑验证依赖 T0 部署链通)。

**关键路径**:T1→T3→T4→T7(连接池 + 双缩零 + runbook 是长尾)。**并行点**:T2 ‖ T3(都依赖 T1);T4 ‖ T5(都依赖 T3)。**明博介入节点**:动手前置确认(D1/Q1/Q2);T6/T7 涉及 dockerized PG CI 首跑与 DEPLOY 文档,线上实跑 defer。

---

## 15. 附:本 PRD 的锻造来源(L1 留痕)

L1 五件套并行锻造(协调者单 message 启 sub agent,均 opus 只读):
- **codebase-scan**(design-explorer):核实 T0 现状——①②③④ 代码+测试到位、⑤⑥(CI 真跑绿 + 部署冒烟)仅脚手架无执行痕迹;pgx 已在 go.mod、sqlc/migrations/sql 空白;依赖方向 CI 强制、livez 不探 DB、`-smoke` 依赖。**这促成了范围选择题(D1)**。
- **architecture** / **implementation** / **requirement** / **redteam**(design-reviewer,redteam 重试一次):四视角批判协调者归一化的 T1 Brief,产出窄接口(具体类型不穿边界)、缩零迁移触发解耦、自验载体化解 NG1 张力、双缩零分层验收、down 数据安全、训练数据时效性闸门、schema 单一真相源、CI gate 通电防假绿等。

**升 R3 的决策**:0 个新 AskUserQuestion 打断实现层——唯一一次 AskUserQuestion 用于**范围层确认**(纯 T1 / T0 已就绪 / 先收 T0,明博选纯 T1,落 D1,属目标边界不计实现层 R3 预算)。两个 Mingbo-owned 实现层问题(迁移工具、Neon 套餐上限)进 §13 Open Questions 给推荐默认、不打断。其余开放点全部 R1(给 default + §12 audit)。

*本产物由 design-gate-lite(协调者模式)生成,仅做 PRD 锻造,未写任何实现代码。下游 T1 落地由明博显式授权后另起 session,建议实现后对高杠杆的连接池/迁移代码做 Codex 异源 review。*
