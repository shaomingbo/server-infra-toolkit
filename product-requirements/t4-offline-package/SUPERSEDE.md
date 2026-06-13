# T4 离线包签名:v2 扩展签名 supersede roadmap 的 v1 描述

本文件是 T4 FR15 / AC13 的 **supersede 声明产物**(声明而非受控演进改写——不动冻结的 roadmap 正文)。

## 被 supersede 的旧描述

`product-requirements/server-infra-roadmap/prd.md:141` 把 offline-package 的签名记为 **v1** 形态:

> Ed25519 签名覆盖 `UTF8(version + "\n" + digest)`

这是 roadmap 锻造期记的线索,**不是已落地实现**(server 仓 `.go` 文件里 offline-package / active.json / ed25519 / v1 签名零代码落地,grep 确认)。

## 现行真相源(v2,本站落地)

server 端实际落地并联调用的是 **v2 扩展签名**(contractVersion 1.0.0),canonical payload 为六字段、固定顺序、单 `0x0A` 分隔:

```
UTF8( "offline-package-sig-v2" + LF + version + LF + digest + LF + minAppVersion + LF + fileManifestHash + LF + rollbackFloor )
```

首行常量 tag `offline-package-sig-v2` 做域分隔,使旧 v1 签名在 v2 验签器上**结构性失效**(legacyV1Signature 反例向量钉死这条红线)。主形态是 empty-tail(`fileManifestHash` 与 `rollbackFloor` 均为空串,payload 以两个尾部 `0x0A` 结束)。

字节规格、向量、wire 编码的真相源是 vendored 契约:

- `internal/platform/offlinesig/contract/v2-signature-contract.md`(pin 基线)
- `internal/platform/offlinesig/contract/canonical-payload-vectors.json`(2 条黄金向量)
- `internal/platform/offlinesig/contract/verify-extended-cases.json`(interop 已知答案向量)

实现落在 `internal/platform/offlinesig/`(builder + 签名 + 编码 + active.json 组装),签发入口是 `cmd/api -sign-active` 一次性子命令。

**roadmap prd.md:141 不删不改**:它是冻结正文,本声明只在 supersede 关系上覆盖它;读到 roadmap v1 描述时以本文件 + vendored 契约为准。

## pin 的能力边界(诚实标注,FR15 / NFR4 / AC15)

`internal/platform/offlinesig/contract/offline-package-v2-contract-pin.json` 记录 vendored 文件的来源(app 仓 commit `b1b13d850a95435feae26a99250e2c7e79036fd8`、contractVersion `1.0.0`)与 sha256;`offlinesig` 包 `init()` 在 build/test 期 fail-closed 校验这些 sha256(篡改/删除 vendored 文件 → 构建红,挂 `go test` 即被 `scripts/verify.sh` 覆盖,不可 skip)。

**pin 只防本地篡改,不防上游 diverge**:app 仓改了字节规格而 server 未同步 bump pin 时,server CI 仍会绿——因为 server CI 读不到 app 仓的最新文档,只能校验仓内副本对自身的一致性。这道闸不构成"跨仓字节一致已被自动保证"的证据。

## NEEDS-SERVER-BUMP 跨仓纪律

上游 diverge 这一缺口靠**跨仓 commit 纪律**补:app 仓改动 v2 字节规格(payload 字段/顺序/分隔/编码/tag)时,该 commit 必须打 `NEEDS-SERVER-BUMP` 标记(对称 T5 的 `NEEDS-CLIENT-BUMP`),提示 server 侧:

1. re-vendor app 仓三份契约文件到 `internal/platform/offlinesig/contract/`;
2. 用 `shasum -a 256` 重算并更新 pin 文件里的 sha256 与 `source_commit`;
3. 跑 `bash scripts/verify.sh` 让黄金向量 + interop 向量重新对齐绿。

外加定期人工对账(NFR4 / AC15)兜底——纪律靠人,不靠机器自动检出。

## 签发运营时序纪律(D9,可用性关键)

`-sign-active` 子命令读四个 env(模板见 `.env.example` 的 OFFLINE_SIGN_* 段):

- `OFFLINE_SIGN_PRIVATE_KEY`(唯一 secret,包在 config.Secret 里不回显)
- `OFFLINE_SIGN_KEY_ID`(要签发的 keyId,大小写敏感)
- `OFFLINE_SIGN_ACTIVE_KEY_IDS`(本地 Active 白名单,逗号分隔)
- `OFFLINE_SIGN_EXPECTED_PUBLIC_KEY`(该 keyId 已发布的公钥,RAW 32 字节标准 base64,**独立输入、非从私钥派生**)

**两道 fail-closed 闸**:

1. **FR8 发布时序闸**:keyId 只有出现在 `OFFLINE_SIGN_ACTIVE_KEY_IDS` 里才被签发(Active),否则视为 Minted 拒签。把 keyId 加进这个列表**必须先确认 app 端该 keyId 的公钥已在双端全量铺达**——否则 app 收到未知 keyId 会 fail-closed(unknownKeyId),抢跑 = 可用性事故而非可恢复告警。提升 Active 是刻意的人工动作,签发器从不自动提升。
2. **FR9 私钥↔keyId 一致性闸**:签发器校验注入的私钥派生出的公钥**恰好等于** `OFFLINE_SIGN_EXPECTED_PUBLIC_KEY`(operator 从 app 发布记录抄来的、该 keyId 已发布的公钥)。配错私钥/keyId 配对 → 派生公钥不匹配 → 拒签(ErrKeyMismatch),挡住"签出 app 用已发布公钥验不过的签名"这类静默 desync。该公钥是独立 env,不是从私钥自派生(自派生会让这道校验恒真、空转)。
