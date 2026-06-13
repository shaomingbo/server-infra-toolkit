# PRD: T4 离线包 v2 扩展签名生成器(empty-tail 主形态,bit-exact 复刻 app 契约)

> 由 design-gate-lite(L1)从 GitHub issue #1 锻造。source_type = execution_plan。
> 真相源:`https://github.com/shaomingbo/server-infra-toolkit/issues/1`

## 1. Summary

为 server-infra-toolkit 实现 T4 离线包的 **v2 扩展签名生成器**:一个确定性 canonical payload builder + Ed25519 签名器,bit-exact 复刻 app-infra-toolkit 已定稿的 v2 签名契约(contractVersion 1.0.0,sha256 pin `044a0e96…`),使 server 产出的 `active.json.signatureV2` 能通过 app 双端(Android/iOS)v2 验签器。主形态是 **empty-tail**(`fileManifestHash` 与 `rollbackFloor` 均为空字符串)。给谁用:app 客户端验签器(直接 operator 是明博/server 维护者)。为什么现在:server roadmap 当前记的是旧 **v1**(`UTF8(version + "\n" + digest)`)格式,v1 在 v2 验签器上结构性失效,且 app 端 v2 契约 + 守卫已上 CI(app 仓 PR #1),server 需照契约复刻才能联调。**Mode: L1**(触及安全边界/密钥/签名 + 对外跨仓契约 + 实现细节与需求混杂,但方向单一,不升 L2)。

## 2. Goal Alignment

> status = **clear**(issue 直接点明目标,A.5 目标层 gate 未触发,零打扰)。

- **目标用户/operator**:app 双端(Android/iOS)v2 验签器消费 server 产出的 `signatureV2`;直接 operator 是明博/server 维护者。
- **问题**:server T4 离线包当前 roadmap 记的是旧 v1 签名格式(`version + "\n" + digest`),v1 在 app v2 验签器上**结构性失效**(域分隔 tag);server 缺一个 bit-exact 复刻 app v2 契约的签名生成器。
- **成功长什么样**:payload builder 产出 empty-tail canonical payload 字节 **byte-for-byte 匹配 app 黄金向量**,完整 `signatureV2` 能通过本地 Ed25519 验签,且有防漂移 sha256 pin + CI round-trip 自检。
- **范围边界**:仅 empty-tail 签名生成器 + 自检 + 离线签发能力;**不含** `/v1/manifest`·`/v1/package` 端点、populated-tail 形态、客户端验签、完整密钥管理(KMS/轮换)、生产发布编排。
- **首个验收信号**:emptyTail 向量的 input tuple → builder 输出 hexdump byte-for-byte == 向量 `expectedPayloadHex`(AC1)。

## 3. Background and Problem

`app-infra-toolkit` 的 offline-package **v2 扩展签名契约**已定稿落地(app 仓 PR #1,CI 全绿)。契约真相源(合并到 app 仓 main 后在以下路径):

| 项 | 位置(app 仓) |
|---|---|
| 字节规格文档 | `modules/offline-package/docs/v2-signature-contract.md` |
| sha256 pin(复刻基线) | `044a0e96eea8a8e0fcea05368e6b1ba152dd7413752a1614b1107a55bde7a56a`(contractVersion 1.0.0) |
| server 实现规格全文 | `product-requirements/offline-package-v2-signature-contract/cross-repo-handoff.md` |
| 黄金向量 | `modules/offline-package/fixtures/canonical-payload-vectors.json` |

**现状证伪(codebase-scan + 第一手 grep 确认)**:server 仓 `.go` 文件里 offline-package / active.json / ed25519 / v1 签名 **零代码落地**;v1 格式只出现在 `product-requirements/server-infra-roadmap/prd.md:141` 与 `handoff.json` 的文字描述里。因此 issue 所说"替换 v1 → v2"实质是**契约描述层面的 supersede**,server 端是**首次实现 v2 生成器**,不存在要改的 v1 代码(下游 implementer 找不到 v1 代码属预期,不是缺漏)。

**为什么现有行为不足**:v1 单字段签名在 v2 验签器上因缺少域分隔 tag 而结构性失效;且 server 端目前压根没有签名生成能力。

## 4. Users and Use Cases

- **主要用户**:app 双端 v2 验签器(消费 `active.json.signatureV2`)。
- **次要用户/operator**:明博 / server 维护者(执行签发命令、管理密钥、维护 vendored 向量)。
- **用例**:
  1. CI 自检:对 emptyTail 向量验证 builder 字节输出与 app 黄金向量逐字节一致(确定性、不需私钥)。
  2. 离线签发:operator 用一次性 CLI 子命令 + 注入的私钥,产出带 `signatureV2` 的 `active.json`。
  3. 防漂移守卫:vendored 向量/契约文件被本地篡改时 CI fail-closed 变红。
  4. 本地端到端自验:用代码内固定测试密钥对签 empty-tail payload 并本地 Ed25519 验签,篡改字节即失败。

## 5. Goals and Non-goals

**Goals**
- server 产出的 `active.json.signatureV2` 能通过 app 双端 v2 验签器(跨仓联调验收)。
- payload builder 字节输出 byte-exact 匹配 app 黄金向量(server CI 内可自检)。
- 防契约漂移:sha256 pin + CI round-trip + mutation 反例 gate。

**Non-goals**(防 scope creep)
- `/v1/manifest`·`/v1/package` 端点(后续站;本站只产签名生成器 + 离线签发)。
- populated-tail 形态(非空 `fileManifestHash`/`rollbackFloor`)——前提见 D10/Q2;builder 把 tail 段参数化预留,当前唯一 case 是 empty-tail。
- 客户端验签实现(app 仓已落地)。
- 完整密钥管理基础设施(KMS / 自动轮换 / 多 key 编排)——但**私钥安全落点不是 non-goal**(见 NFR2,roadmap NFR2/R6 已定死)。
- 生产发布编排(把 `signatureV2` 写进真实生产 `active.json` 并上线)——gated 动作,前置见 Q1/D9;**触发条件**:app PR #1 merge + app 双端 v2 验签器全量铺达。

## 6. Requirements

**Functional**
- **FR1**:canonical payload builder 是确定性纯函数,输出 6 字段(`sigV2Tag` / `version` / `digest` / `minAppVersion` / `fileManifestHash` / `rollbackFloor`)按固定顺序、单 `\n`(0x0A)分隔、无尾换行、无 BOM、UTF-8 编码的字节串。
- **FR2**:主形态 empty-tail(`fileManifestHash` 与 `rollbackFloor` 均为空字符串),输出恰含 5 个 `0x0A`、末尾两字节为 `0a 0a`。
- **FR3**:`sigV2Tag` 固定为 `offline-package-sig-v2`,作为 payload 第一行;该字面量在仓内仅出现于被 pin 的契约常量一处(反同源漂移)。
- **FR4**:`digest` 以 `sha256:<64 位 lower-hex>` 形态进入 payload;builder 对输入做 lower-hex 规范化,对非 lower-hex 输入直接 reject(消除原契约 "verbatim" 与 "lower-hex 强制" 的歧义——大写会让 digest 校验通过而签名校验失败,极难排查)。
- **FR5**:字段值卫生——builder 拒绝任何含 `\r`、裸 `\n`、BOM 或控制字符的字段值(empty-tail 测不到字段内脏字节)。
- **FR6**:签名用 `crypto/ed25519`(PureEd25519,RFC 8032,Go 标准库)对 canonical payload 签名 → RFC 4648 **标准 base64(带 padding,非 URL-safe)** → 加 `base64:` 前缀。
- **FR7**:公钥发布为 raw 32 字节标准 base64(带 padding),**禁止** SPKI/X.509/DER 包装(SPKI 是 44 字节,会令 app 解出错误公钥)。
- **FR8**:`keyId` 大小写敏感;签发路径强制只用状态为 `active` 的 keyId,用 `minted`(未铺达)keyId 签发 → fail-closed 拒绝且不产出 `signatureV2`(把跨仓发布顺序不变量降解为本仓可验收的状态机)。
- **FR9**:私钥/keyId 一致性——签发前校验"私钥派生的公钥 == keyId 映射的已发布公钥",不等则 fail-closed 拒签(防轮换时私钥换了 keyId 没换)。
- **FR10**:所有 canonical 字节构造的唯一出口是 builder 包;签名器、测试、工具一律调 builder,不在别处重复拼接 tag/字段/分隔符。
- **FR11**:黄金向量从 app 仓 PR #1 权威产物 **vendor(copy)** 进 server 仓并 `go:embed`;落 sha256 pin 文件 `offline-package-v2-contract-pin.json` 记录 vendored 文件自身 hash + 来源 app 仓 commit + contractVersion。
- **FR12**:CI round-trip 自检——对 emptyTail 向量 input tuple,builder 输出 hexdump byte-for-byte == 向量 `expectedPayloadHex`;并含 **mutation 反例集**(upper-hex digest / 字段乱序 / 加尾 `0x0A` / 加 BOM / 非空 tail),断言产出 != 期望或 builder reject。
- **FR13**:本地 Ed25519 sign-verify 自验——用代码内固定测试密钥对签 empty-tail payload,用 FR7 形态公钥本地验签断言通过;篡改 payload 任一字节 → 验签失败(覆盖 payload 字节之外的签名层/编码层)。
- **FR14**:签发入口为 cmd 一次性子命令(贴 `cmd/api` 现有 `-smoke`/`-seed`/`-unlock` 子命令先例),给定输入 + 私钥 env 产出 `active.json`,不监听端口、不写 DB、不发起网络请求。
- **FR15**:文档 supersede——在 T4 PRD/docs 声明 v2 签名 supersede roadmap `prd.md:141` 的 v1 描述,**不改冻结 roadmap 正文**(走声明而非受控演进改写)。

**Non-functional**
- **NFR1**:payload builder 确定性(无随机/时间/map 遍历顺序依赖),同输入在两个独立进程各运行一次输出字节完全相等;自检不需私钥。
- **NFR2**:私钥包 `config.Secret` 类型(`String()`/`MarshalJSON()`/`LogValue()` 全 `[REDACTED]`,仅 `Reveal()` 吐明文);私钥不出现在任何 stdout/日志/`active.json` 明文;生产私钥经 Secret Manager → env 注入(继承 roadmap NFR2/R6,**非 non-goal**)。
- **NFR3**:遵守 server 冻结契约——包依赖方向(builder 若落 `internal/platform/*` 绝不 import `internal/http`/`internal/modules`)、错误信封 append-only、config 仅 `os.Getenv`+`.env`、`scripts/verify.sh` 是 CI 唯一入口;pin gate 挂 `go test`、**不可 skip**(区别于 sqlc/migration 的可 skip gate)。
- **NFR4**:sha256 pin gate 检测**本地篡改**(向量文件 hash != pin → CI 退出非 0);**明确不覆盖上游 diverge**(app 改了字节规格而 server 未 bump 时 CI 仍绿),后者靠跨仓 commit 纪律(`NEEDS-SERVER-BUMP`,对称 T5 的 `NEEDS-CLIENT-BUMP`,需 app 仓配合)。

## 7. User Flow or State Flow

**CI 自检流(确定性、不需私钥)**:`bash scripts/verify.sh` → `go test` 跑 builder → 对每条向量比对 hexdump → mutation 集逐个断言不等/拒绝 → pin gate 算 vendored 文件 sha256 比对 → 任一不符退出非 0。

**离线签发流(需私钥)**:operator 注入私钥 env + keyId → `go run ./cmd/api -sign-active …`(子命令名实现期定) → config.Load 解析私钥(Secret)+ 校验长度 → 校验 keyId 状态 `active`(否则 fail-closed)→ 校验私钥派生公钥 == keyId 映射公钥(否则 fail-closed)→ builder 构造 empty-tail payload → ed25519.Sign → base64 编码 → 组装 `active.json` → 写 stdout/指定路径 → 退出码 0。

**keyId 状态机**:`minted`(已铸密钥,app 未确认铺达)→ [人工 gated 翻转,明博确认 app 发版铺达后]→ `active`(可签)。签发路径只接受 `active`。

| 状态/输入 | 行为 |
|---|---|
| digest 大写 hex | 规范化为 lower-hex 或 reject(FR4) |
| 字段含 `\r`/BOM/控制字符 | reject,不产出(FR5) |
| 非空 tail 输入 | 当前形态 reject 或显式不静默吞行(FR2/FR12) |
| keyId 状态 = minted | fail-closed 拒签(FR8) |
| 私钥/keyId 公钥不匹配 | fail-closed 拒签(FR9) |
| 向量文件被篡改/缺失 | CI 退出非 0,非 skip(NFR3/NFR4) |

## 8. Data, API, Permissions

**canonical payload 字节规格**(签名计算的输入字节串,非 active.json 的 JSON wire):
```
sigV2Tag + "\n" + version + "\n" + digest + "\n" + minAppVersion + "\n" + fileManifestHash + "\n" + rollbackFloor
```
empty-tail bit-exact 自检基线(inputs:`version=1.4.0` / `digest=sha256:fd3c…3c49` / `minAppVersion=3.2.0` / 两尾空,contractVersion 1.0.0):
```
6f66666c696e652d7061636b6167652d7369672d76320a312e342e300a7368613235363a666433633635383365386362343333373962653138643164626333373461313731303934633237356630393465393164393833376632363733656635336334390a332e322e300a0a
```
(末尾 `0a0a` = 两个尾部空行;5 个 `0a`;已逐字节核对解码为上述 6 字段。)

**密钥材料**(实现期从 app 仓 cross-repo-handoff.md 核对精确编码):
- 私钥:env 注入(建议 base64 of 32 字节 seed → `ed25519.NewKeyFromSeed`),包 `config.Secret`,config.Load 校验长度/编码。
- 公钥:raw 32 字节标准 base64(FR7);非敏感,可入日志。
- keyId:公开标识符,大小写敏感,带状态字段(`minted`/`active`);非敏感,不包 Secret。

**active.json wire**:signatureV2 字段的精确形状(字符串 `base64:<...>` 还是 `{keyId, sig, publicKey}` 对象)、payload 6 字段 ↔ active.json 字段映射,**实现前必须从 app 仓 PR #1 的 v2-signature-contract.md 核对**(T1)。

**权限/产物落点**:签发是 operator 离线命令(非公网端点),无新权限角色;active.json 写 stdout 或 `-o` 指定路径,不引入对象存储等新外部服务。无 DB schema、无迁移(签名是无状态计算)。

## 9. Acceptance Criteria

- **AC1**(FR1,FR2,NFR1):Given emptyTail 向量 input tuple,when 调 builder,then 输出 hexdump byte-for-byte == 向量 `expectedPayloadHex`,且末尾两字节为 `0x0a 0x0a`、`0x0A` 出现恰 5 次。Verifiable by: automated_test。
- **AC2**(FR1,NFR1):Given 同一 input tuple,when 在两个独立进程各运行 builder 一次,then 两次输出字节完全相等。Verifiable by: automated_test。
- **AC3**(FR4):Given digest 含大写 hex(如 `sha256:AABB…`),when 调 builder,then 规范化为 lower-hex 或返回错误,产出的 payload 不含大写 hex。Verifiable by: automated_test。
- **AC4**(FR5):Given 某字段值含 `\r`、BOM 或控制字符,when 调 builder,then 返回错误且不产出 payload。Verifiable by: automated_test。
- **AC5**(FR3,FR10):Given 全仓源码,when grep `offline-package-sig-v2` 字面量,then 仅在被 pin 的契约常量一处出现;把该字面量改为 `offline-package-sig-V2` → pin gate 退出非 0。Verifiable by: code_review + automated_test。
- **AC6**(FR6,FR7,FR13):Given 固定测试密钥对,when 用 builder + `ed25519.Sign` 产出 signatureV2,then 该字段为 `base64:<标准 base64 带 padding>`,公钥序列化为 raw 32 字节标准 base64(解码后恰 32 字节,非 44)。Verifiable by: automated_test。
- **AC7**(FR12,FR13):Given 测试密钥对 + empty-tail payload,when `ed25519.Verify(公钥, payload, 解码签名)`,then 返回 true;篡改 payload 任一字节后返回 false。Verifiable by: automated_test。
- **AC8**(FR12):Given mutation 集(upper-hex digest / 字段乱序 / 加尾 `0x0A` / 加 BOM / 非空 tail),when 运行 CI 自检,then 每个变异的 builder 输出 hexdump != `expectedPayloadHex` 或 builder 拒绝。Verifiable by: automated_test。
- **AC9**(FR11,NFR3,NFR4):Given vendored 黄金向量文件,when 篡改其一字节或删除该文件后跑 `bash scripts/verify.sh`,then 退出码非 0(pin gate 不可 skip)。Verifiable by: automated_test。
- **AC10**(FR8,FR9):Given keyId 状态为 `minted` 或私钥派生公钥 != keyId 映射公钥,when 调签发路径,then fail-closed 返回错误且不产出 signatureV2。Verifiable by: automated_test。
- **AC11**(NFR2):Given 私钥经 config 加载,when 序列化/打日志,then 输出 `[REDACTED]`;CI 守卫断言私钥明文不出现在 stdout/日志路径。Verifiable by: automated_test。
- **AC12**(FR14):Given 签发子命令,when 给定输入 env 执行,then 产出 `active.json` 且退出码 0,不监听端口、不发起网络请求、不写 DB。Verifiable by: automated_test + code_review。
- **AC13**(FR15):Given T4 PRD/docs,when review,then 含 "v2 supersede roadmap v1" 的声明,且 `git diff` 不含对 roadmap `prd.md` 正文的删改。Verifiable by: code_review。
- **AC14**(NFR3):Given builder 落点,when `go list -deps` 该包,then 输出不含 `internal/http`;若落 `internal/platform/*` 则也不含 `internal/modules`。Verifiable by: automated_test。
- **AC15**(FR11,NFR4):Given vendored 向量文件,when review,then 文件记录来源 app 仓 PR #1 commit hash + contractVersion + sha256,且 PRD 标注 pin 不覆盖上游 diverge 的能力边界。Verifiable by: code_review。

## 10. Edge Cases and Failure States

- **E1**:digest 大写/混合大小写 hex → normalize 或 reject(AC3),绝不原样签入。
- **E2**:字段值含 `\r`/裸 `\n`/BOM/控制字符 → reject(AC4)。
- **E3**:非空 tail(populated-tail)输入 → 当前形态显式 reject;严防"空字段跳过"实现把末尾 `0a0a` 写成 `0a`(byte count 差 1,整签废)。
- **E4**:keyId 处于 `minted` 状态被用于签发 → fail-closed(AC10)。
- **E5**:私钥派生公钥与 keyId 映射公钥不匹配(轮换错配)→ fail-closed 拒签(AC10)。
- **E6**:app 仓 PR #1 未 merge / 向量文件缺失 → vendor task(T1)阻塞;CI 缺向量退出非 0(非 skip)。
- **E7**:上游 app 改了字节规格但 server pin 未 bump → CI 绿但跨仓失配(pin 能力边界 R4,靠跨仓纪律)。
- **E8**:签发抢跑(app v2 验签器未全量铺达)→ app `unknownKeyId`/`unknown sigVersion` fail-closed 可用性事故;发布 gate(Q1/D9)拦截。

## 11. Risks and Mitigations

- **R1**(impact high / likelihood medium / owner engineering):向量初始转录错(source 非 app 权威产物)→ self-consistent 假绿(builder 比对的是同源错误向量)。缓解:从 app PR #1 权威产物逐字节 copy + 记录来源 commit + provenance 可追溯(FR11/AC15)。
- **R2**(high / medium / owner Mingbo):server 抢跑写 signatureV2 进生产 → app fail-closed 可用性事故。缓解:发布 gate(app PR #1 merge + 双端验签器全量铺达前不写生产 active.json)+ keyId 状态机(D9/FR8)。
- **R3**(high / medium / owner engineering):字节级低级错(大写 hex / base64 变体 / 空字段吞行 / BOM / `\r`)→ 极难排查的验签失败。缓解:lower-hex normalize + 字段卫生 reject + mutation 反例集 + 本地 sign-verify round-trip(FR4/FR5/FR12/FR13)。
- **R4**(medium / medium / owner engineering):pin 只防本地篡改不防上游 diverge → 误读为"防住了"。缓解:PRD 诚实标注能力边界 + `NEEDS-SERVER-BUMP` 跨仓纪律 + 定期人工对账(NFR4/AC15)。
- **R5**(high / low / owner engineering):私钥泄露 / 签错 keyId。缓解:Secret 包装 + 不进日志 + 私钥/keyId 一致性启动校验(NFR2/FR9)。
- **R6**(medium / medium / owner product):empty-tail 前提失效(真实 active.json 迟早带 fileManifestHash)→ FR11 与 non-goal 冲突。缓解:tail 段参数化预留 + PRD 显式声明前提 + 前提失效则扩 builder/开新站(D7/D10/Q2)。

## 12. Default Decisions

- **D1**:按"首次实现 v2 生成器"立 PRD(server 仓零 v1 代码,第一手 grep 证实)。Why:`.go` 文件无任何 offline-package/v1 签名实现,v1 仅存于 roadmap 文字。Override if:实际在某分支已有 v1 签名代码需迁移。
- **D2**:落点 = builder 纯函数落 `internal/platform/offlinesig`(或 `internal/offlinesig`),签名器 + 签发 = cmd 一次性子命令(贴 seed/unlock 先例),不走 HTTP/不写 DB。Why:签名生成不在请求路径、不产端点,塞 modules 是形态错配;builder 纯函数应可被无密钥环境复用。Override if:明博要求 on-demand HTTP 签发端点。
- **D3**:私钥包 `config.Secret`,env 注入(base64 32 字节 seed),config.Load 校验;keyId/公钥不敏感不包 Secret。Why:对齐 NEON_DSN Secret 先例 + roadmap NFR2。Override if:私钥改走运行时外置(硬件/离线机)不进 config。
- **D4**:vendor 黄金向量 = copy + `go:embed`(贴 observability 模式),pin vendored 文件自身 sha256;CI gate fail-closed 不可 skip;只防本地篡改,上游 diverge 靠 `NEEDS-SERVER-BUMP` 跨仓纪律。Why:server CI 读不到 app 仓文档,只能 pin 仓内副本;submodule 在 Go module + Cloud Run build 链是已知麻烦。Override if:改用 app 仓发布的 signed artifact 拉取机制。
- **D5**:digest 强制 lower-hex normalize + reject 非 lower-hex 输入。Why:消除原契约 "verbatim" 歧义,堵住"digest 校验过、签名校验过不了"的极难排查坑。Override if:app 契约明确要求 verbatim 保留大小写(需核对)。
- **D6**:字段值卫生——reject 含 `\r`/裸 `\n`/BOM/控制字符的字段值。Why:empty-tail 测不到字段内脏字节,Windows `\r\n` 等会静默污染 payload。Override if:契约要求对某字段保留特定字节。
- **D7**:builder tail 段参数化(预留 populated-tail),empty-tail 是当前唯一 case,不把"两尾空"焊死在主路径。Why:低成本(一个函数参数)预留 v2.1 演进,不破 empty-tail。Override if:确认永不需要 populated-tail。
- **D8**:本站验收 = server 内部 byte-exact + 本地 Ed25519 sign-verify round-trip;"app 双端实测验签"是跨仓联调验收,**non-blocking for server CI**。Why:app 验签器在 app 仓,server CI 不可触达;不能让 CI 卡在跨仓结果上。Override if:引入 app 验签器的可执行 conformance 工具进 server CI。
- **D9**:范围红线——本站 = 生成器 + 自检 + 离线签发能力;**生产发布 gated**,前置 app PR #1 merge + 双端 v2 验签器全量铺达;server 端落 keyId 状态机 + 发布前置 fail-closed gate,翻转需人工确认。Why:roadmap 标 T4"待客户端联调窗口",抢跑 = 可用性事故;PRD 阶段给最保守 default,不卡锻造。Override if:见 Q1。
- **D10**:empty-tail 恒空作为本站前提显式声明;前提失效则扩 builder/开新站。Why:issue 明示 empty-tail 主形态,但真实 active.json tail 是否恒空需 app 核对。Override if:见 Q2。

## 13. Open Questions

> 均 Mingbo-owned,带 recommended default,记录供事后定,**不阻塞本 PRD 锻造**(不计入 R3 当场打断)。

- **Q1**:跨仓发布时序——`signatureV2` 何时写进生产 `active.json` 并发布?
  - Why Mingbo-owned:依赖 "app PR #1 已 merge + 双端 v2 验签器全量铺达" 这个**外部发版事实**,server 仓无法自动判定;何时认定铺达完成、是否等灰度、出事谁回滚属跨仓发布承诺。
  - Recommended default:本站范围切到"生成器 + 自检 + 离线签发能力",生产发布作为后续 gated 动作待联调窗口;server 端落 keyId 状态机(minted→active)+ 发布前置 fail-closed gate,翻转动作明博手动确认后执行(D9)。
- **Q2**:本站生产签发的真实 `active.json`,`fileManifestHash` 与 `rollbackFloor` 是否恒为空字符串?
  - Why Mingbo-owned:取决于 T4 离线包业务设计(首版是否携带文件清单/回滚地板),是产品范围决策,且直接决定 FR11"集成 active.json"与 non-goal "populated-tail" 是否冲突。
  - Recommended default:按 issue 明示的 empty-tail 主形态,假定本站签发的 active.json tail 恒空,builder 把 tail 段参数化预留 populated-tail(D7);前提失效则扩 builder 或开新站。实现前从 app `cross-repo-handoff.md` 核对(T1)。

## 14. Implementation Tasks

- **T1**(owner main_coordinator,deps: []):vendor app 仓 PR #1 三文件(`v2-signature-contract.md` / `canonical-payload-vectors.json` / `cross-repo-handoff.md`)进 server 仓并核对字节细节(active.json wire、payload↔active.json 字段映射、公钥 base64 变体、私钥编码、向量值);确认 app PR #1 已 merge;确认 Q1/Q2。Done when: AC15。
- **T2**(owner implementer,deps: [T1]):实现 canonical payload builder 纯函数(字节规格 + empty-tail + lower-hex normalize + 字段卫生 + 确定性 + 单一出口),`go:embed` 黄金向量 + pin 常量。Done when: AC1, AC2, AC3, AC4, AC5。
- **T3**(owner implementer,deps: [T2]):实现 Ed25519 签名 + 标准 base64 编码 + 公钥 raw 序列化 + signatureV2 字段组装。Done when: AC6, AC7。
- **T4**(owner implementer,deps: [T2]):实现 keyId 状态机(minted/active)+ 私钥/keyId 一致性校验 + 私钥 `config.Secret` 包装 + config 注入校验。Done when: AC10, AC11。
- **T5**(owner implementer,deps: [T3, T4]):实现签发 cmd 一次性子命令(产 active.json,不端口/不 DB/不联网)。Done when: AC12。
- **T6**(owner test-runner,deps: [T2, T3]):CI round-trip 正例 + mutation 反例集 + pin gate(挂 verify.sh/go test,fail-closed 不可 skip)+ 依赖方向守卫测试。Done when: AC8, AC9, AC14。
- **T7**(owner main_coordinator,deps: [T1]):文档——T4 docs 声明 v2 supersede roadmap v1(不改冻结正文)+ 标注 pin 能力边界 + `NEEDS-SERVER-BUMP` 跨仓纪律。Done when: AC13, AC15。
- **T8**(owner reviewer,deps: [T5, T6, T7]):异源 Codex review + reviewer 复审字节正确性 + evaluator 按 AC 逐条验收。Done when: AC1, AC6, AC7, AC8, AC9, AC10, AC12。

---

**rejected_directions**:不适用(L1 单方向,无候选发散)。本 PRD 未启 L2 两阶段。
