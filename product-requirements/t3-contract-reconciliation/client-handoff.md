# T3 客户端交接说明(app-infra-toolkit 侧前置活)

> T3 全双向契约对账的客户端半边交接物(PRD §12 D10:服务端 schema 落地后产出)。
> owner:明博。本文档只交接,不实现——客户端仓库拿真 schema + T3 PRD/handoff 自跑 design-gate 决定开站(D10)。
> 服务端半边已落地;**客户端未开站期间,双向未闭合,只有服务端单向自检生效**(PRD E5/R1)。

## 1. 契约真相在哪(schema 路径)

服务端仓库 `github.com/shaomingbo/server-infra-toolkit` 持有 wire 契约真相(S5 机制,真相源不外移):

| 端点 | schema 文件 |
|------|------------|
| `POST /v1/auth/login` 成功响应 | `internal/modules/auth/contract/login.schema.json` |
| `POST /v1/auth/refresh` 成功响应 | `internal/modules/auth/contract/refresh.schema.json` |

两份独立锚定(当前内容相同,未来可分化,客户端不要合并消费)。JSON Schema draft 2020-12,`additionalProperties: false` + 全字段 `required`——schema 严格度 ≥ 客户端严格解码器,服务端 append 字段不会被静默放过(NFR1)。

`docs/CONTRACTS.md` §6 的自然语言自 T3 起降为人类摘要,**机器可读规范以 schema 文件为准,二者冲突以 schema 为准**。

## 2. wire 形状与值语义

```json
{
  "userId":       "string(UUID)",
  "accessToken":  "string",
  "refreshToken": "string",
  "expiresAt":    "integer — access token 过期的 Unix 毫秒绝对时间戳,不是秒"
}
```

- `expiresAt` 单位是**毫秒**:schema 的 `description` 写明,服务端另有 AST 源码守卫(`expires_unit_guard_test.go`)锁死 `UnixMilli()` 调用,单位漂移在服务端 CI 红。
- 形状 schema 抓不到的残留盲区(诚实标注,PRD R4):token 内部编码格式、`null` ↔ 缺字段的语义差。客户端解码器对此自行保持严格。
- 服务端编码经 `json.NewEncoder`(尾随 `\n` + HTML 转义),客户端按标准 JSON 解析即可,不要做字节级比对。

## 3. 客户端固定样例值(可选对齐,PRD D9)

服务端 conformance 测试用的确定性字面量,客户端测试如需 magic 值可直接对齐:

```
userId:       11111111-2222-4333-8444-555555555555
accessToken:  fixed-access-token-AAAAAAAAAAAAAAAAAAAAAAAA
refreshToken: fixed-refresh-token-BBBBBBBBBBBBBBBBBBBBBBBB
expiresAt:    1748563200123   (尾数非零,毫秒↔秒可判别)
```

## 4. pin 方式与 bump 纪律

- **pin 什么**:客户端 0.1.0 未发布,跨 repo 只能 git-commit-pin(PRD A4)——pin 服务端仓库某个 commit,读上述两个 schema 文件(submodule pin 整 repo、sparse checkout、或 CI 里按 commit 拉文件均可,客户端自选;客户端只读文件,不 import Go 代码)。
- **bump 纪律**:服务端发新 schema 后,客户端 bump pin 才消费到新契约。**残留窗口**(PRD R5):服务端发新 schema → 客户端 bump 之间,客户端校验的是旧契约——这是 pin 天花板,设计上接受;服务端自己的漂移不受此影响(服务端 CI 实时抓)。
- **何时必须 bump**:服务端 wire 有意变更(append 可选字段等)会同步改 schema + conformance 断言;客户端看到服务端 `contract/` 目录变更即应 bump。

## 5. 客户端 CI 要做什么(开站范围)

客户端半边的验收目标(对应 PRD FR9/GA2):**客户端改坏解码器期望,在客户端自己的 CI 红**。

1. pin 服务端 schema(上节方式)。
2. 客户端 CI 校验其 Kotlin/Swift 解码器接受 schema 声明的形状:全字段 required、无额外字段、`expiresAt` 为整数毫秒。实现方式客户端自定(例:从 schema 生成合法/非法样例喂解码器;或断言解码器的字段集/类型与 schema 一致)。
3. 反例自验(对齐服务端 mutation 标准):把解码器某字段期望改名/把 expiresAt 当秒读 → 客户端 CI 必须红。

## 6. 开站方式

客户端仓库拿以下三件套自跑 design-gate 决定开站(D10):
- 真 schema(本文档 §1 路径)
- T3 PRD:`product-requirements/t3-contract-reconciliation/prd.md`
- T3 handoff:`product-requirements/t3-contract-reconciliation/handoff.json`(FR9 = 客户端侧需求声明)
