# T5 客户端交接说明(app-infra-toolkit 侧接入前置活)

> T5 事件接收的客户端接入交接物(PRD FR11/AC14,owner 明博)。本文档只交接,不实现——客户端拿本文档 + 真 schema 自行评估接入节奏。
> 服务端半边已完整落地(收-验-存-幂等-硬上限-限流接缝);**端点当前挂在 feature flag 后默认不公网暴露**(PRD D2 接缝先行),客户端接入时再翻开;认证已定稿,见 §1。

## 1. 端点与开关

- **端点**:`POST /v1/events`(`/v1/` 业务前缀下)。
- **当前状态**:服务端环境变量 `EVENTS_INGEST_ENABLED` 默认关 → 路由不注册 → 公网请求 404。这是有意状态:客户端 0.1.0 未发布,无正当流量的公网写库端点是纯攻击面。接入联调时由明博翻开。
- **认证(已定稿,T5 认证站 `product-requirements/t5-events-ingest-auth/`)**:静态共享 ingest token,专用 header **`X-Ingest-Token`**。
  - **凭据形态**:64 字符小写 hex(明博侧 `openssl rand -hex 32` 生成);服务端 env 只存其 SHA-256 哈希(`EVENTS_INGEST_TOKEN_SHA256S`),不存明文。
  - **分发**:明博带外分发给客户端(明文不进任何 git 仓库);客户端构建时注入或配置下发,存系统安全存储(Keychain / Keystore),不打日志、不进崩溃上报。
  - **哈希口径**(双端锚定,服务端有同对锚定测试):`hex(sha256(utf8_bytes(token)))`,小写 hex。样例对(仅锚定算法与编码,刻意非真实 token 形态):
    - token:`sample-ingest-token-for-hash-anchoring-not-a-credential`
    - sha256:`828317362e384876e0e300262ec1c0e05c1d77ee3f5bf15763f647b67f64c84b`
  - **校验语义**:header 值不做 trim / 大小写规范化,原样字节哈希后比较;缺 header 与错 token 均 401 且响应逐字节一致(不泄露区别)。
  - **轮换纪律**:服务端支持双哈希(`current[,previous]` 逗号分隔)零丢失轮换;**previous 保留至所有已发布客户端版本完成迁移**后才移除。
  - **注入规则(客户端拦截器)**:`/v1/events` 请求注入 `X-Ingest-Token`,**绝不注入 `Authorization`**;ingest token 的来源独立于 LoginManager / 登录态(登录前崩溃、匿名遥测也能上报),不得进 access token 自动刷新路径。

## 2. wire 形状(请求)

请求体 = **裸 JSON 数组**(不是 wrapper 对象),每个元素是一条事件:

```json
[
  {
    "eventId":     "string — 事件级稳定 id,幂等键成分,见 §4(服务端新增要求)",
    "kind":        "string — 闭集枚举:log | crash | telemetry",
    "traceId":     "string — 不透明值,服务端不校验 UUID 格式,只校非空 + 长度",
    "timestampMs": "integer — int64 epoch 毫秒(对齐客户端 Envelope.timestampMs)",
    "source":      "string — 产出子系统名",
    "name":        "string — 事件名",
    "attributes":  "object(可省略)— map<string, string|integer|number|boolean|null>"
  }
]
```

- 字段名 camelCase,与客户端 `EventModels.kt` / `EventModels.swift` 的 Envelope 六字段一一对应;`eventId` 是服务端新增的第七个字段(见 §4)。
- `additionalProperties: false`:多发任何未知字段整批被拒。
- `attributes` 值是 AttributeValue 闭集的 JSON 自然映射(string/int/double/bool/显式 null),**禁数组与嵌套对象**;客户端省略 attributes 与发空对象等价。
- **schema 真相源**(机器可读,客户端 CI 可直接消费,git-commit-pin 方式同 T3 交接物 §4):
  - 单条事件:`internal/modules/observability/contract/event.schema.json`
  - 批量数组级:`internal/modules/observability/contract/batch.schema.json`

## 3. 硬上限(超限整批拒)

| 维度 | 上限 | 超限响应 |
|------|------|---------|
| 请求体字节数 | 1 MiB | 413 `payload_too_large` |
| 单批条数 | 500 条(至少 1 条,空批 400) | 413 `payload_too_large` |
| `eventId`/`kind`/`traceId`/`source`/`name` 长度 | 128 字符 | 400 `bad_request` |
| `attributes` 键数 / 键长 / 字符串值长 | 64 个 / 128 / 1024 字符 | 400 `bad_request` |

客户端 uploader 切批时按"≤500 条且序列化后 ≤1 MiB"双约束取小。

## 4. 幂等键约定(客户端要补一个字段)

- **服务端幂等键 = `(source, eventId)` 复合唯一约束**。同键重发不重复落库(静默跳过),所以 hold-and-retry 重发整批是安全的。
- **客户端现有 Envelope 模型没有事件级稳定 id**——这是接入前必须补的缺口:事件**入队时**生成一个稳定 id(如 UUIDv4)并随事件持久化,**重试时复用同一个 id**(不能每次发送重新生成,否则幂等失效、重发变重复数据)。
- 去重窗口 = 事件保留周期(同表约束的副产品,无独立去重表)。

## 5. 响应与重试语义(状态码就是契约)

客户端 Uploader 的 hold-and-retry 二分(5xx/网络错 = 保批重试;4xx = 永久丢弃)与服务端语义已对齐——**唯一例外是 401(认证失败),走有界 hold 而非永久丢**:

| 响应 | 含义 | 客户端动作 |
|------|------|-----------|
| 200 `{"accepted":N,"duplicate":M,"rejected":0,"requestId":"..."}` | 整批落库(duplicate = 幂等跳过的重发) | 出队,完成 |
| 400 `bad_request` | 解析失败 / schema 违规(message 文本含 `"X of N events rejected"` 计数,仅诊断用)/ 空批 | **永久丢弃该批**(重发也不会变合法) |
| 401 `unauthorized` | 认证失败:缺 `X-Ingest-Token` / token 不在服务端哈希窗口(轮换过渡期外或配置错) | **有界 hold**:保批重试至多 3 次 + 指数退避,仍 401 才丢弃(给轮换/配置修复留恢复窗口;防无界重投,也防按 4xx 静默全丢) |
| 413 `payload_too_large` | 超体积或超条数 | 永久丢弃或**切小批后重发** |
| 429 `rate_limited`(带 `Retry-After`) | 限流(当前 noop 不会真返回,接入后可能启用) | 按 `Retry-After` 退避重试 |
| 5xx `internal` | 服务端/DB 临时故障 | **保批退避重试**(幂等键保证重发安全) |

- 任一条事件违规 → **整批 400 零落库**(不做部分接受/207 逐条),客户端不要依赖部分成功。
- 错误响应统一标准信封 `{"error":{"code","message"},"requestId"}`。

## 6. wire 稳定性声明

T5 的 Envelope 七字段与批量请求/响应 wire **暂标 unstable,不在 CONTRACTS frozen 集**(`docs/CONTRACTS.md` §7,roadmap NFR6:对端真实消费验证后才冻结)。客户端接入联调期间如发现形状不合理(如需要批级 wrapper、逐条结果),是最后的低成本修改窗口;客户端 0.1.0 发布并真实消费后冻结。

## 7. 接入清单(客户端侧开站范围,本 PRD 不实现)

1. Envelope 模型补 `eventId`(入队生成、持久化、重试复用,§4)。
2. 序列化器落地(camelCase 字段名已有 fixture 背书;AttributeValue 按 §2 闭集映射)。
3. uploader 切批逻辑对齐 §3 双上限;重试二分对齐 §5。
4. 客户端 CI 消费服务端 schema(git-commit-pin,同 T3 交接物 §4 的 pin/bump 纪律)。
5. 认证已定稿(§1):实现 `X-Ingest-Token` 注入(独立 token 源,不走登录态)+ 401 有界 hold(§5)→ 向明博取 token → 服务端翻 `EVENTS_INGEST_ENABLED`(前置红线:边缘层真实限流先落地,见服务端 `docs/DEPLOY.md §15`)→ 联调。
