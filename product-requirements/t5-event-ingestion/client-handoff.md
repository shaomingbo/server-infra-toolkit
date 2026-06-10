# T5 客户端交接说明(app-infra-toolkit 侧接入前置活)

> T5 事件接收的客户端接入交接物(PRD FR11/AC14,owner 明博)。本文档只交接,不实现——客户端拿本文档 + 真 schema 自行评估接入节奏。
> 服务端半边已完整落地(收-验-存-幂等-硬上限-限流接缝);**端点当前挂在 feature flag 后默认不公网暴露**(PRD D2 接缝先行),客户端接入时再翻开并定认证。

## 1. 端点与开关

- **端点**:`POST /v1/events`(`/v1/` 业务前缀下)。
- **当前状态**:服务端环境变量 `EVENTS_INGEST_ENABLED` 默认关 → 路由不注册 → 公网请求 404。这是有意状态:客户端 0.1.0 未发布,无正当流量的公网写库端点是纯攻击面。接入联调时由明博翻开。
- **认证:待定**。当前端点后无认证(也无公网暴露)。客户端真实接入前必须定最后一公里,候选:共享 ingest token(header)或复用 T2 Bearer。定下后本文档与 CONTRACTS 同步更新。

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

客户端 Uploader 的 hold-and-retry 二分(5xx/网络错 = 保批重试;4xx = 永久丢弃)与服务端语义已对齐:

| 响应 | 含义 | 客户端动作 |
|------|------|-----------|
| 200 `{"accepted":N,"duplicate":M,"rejected":0,"requestId":"..."}` | 整批落库(duplicate = 幂等跳过的重发) | 出队,完成 |
| 400 `bad_request` | 解析失败 / schema 违规(message 文本含 `"X of N events rejected"` 计数,仅诊断用)/ 空批 | **永久丢弃该批**(重发也不会变合法) |
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
5. 与明博定认证方案(§1)→ 服务端翻 `EVENTS_INGEST_ENABLED` → 联调。
