# offline-package: v2 signature cross-repo byte contract

> contractVersion: **1.0.0** (matches the top-level `contractVersion` of
> `fixtures/verify-extended-cases.json` and `fixtures/canonical-payload-vectors.json`).
>
> This document defines the **v2 (extended) signature byte layout** of the
> offline-package manifest path to the precision a separate server repo can
> **reproduce it bit-for-bit** without reading this repo's source. It is the
> authoritative byte spec for the publisher's signing step (PRD FR1/FR2/FR3/FR7,
> AC1/AC7/AC8/AC9/AC10/AC11). It defines bytes and handshake only; it does not
> change any code, fixture, or layout in this repo.

## Section index

1. Byte construction rules (the full set)
2. Manifest profile (primary shape) + worked example
3. Roles and the key-distribution handshake
4. Threat model and boundary
5. In-repo byte-recipe transcription points
6. contractVersion and the server pin
7. Why this lives in `modules/offline-package/docs/`

---

## 1. Byte construction rules (the full set)

The v2 canonical payload is the exact byte sequence the Ed25519 signature is
computed over. The server's signer and the app's verifier MUST produce the same
bytes or the signature will not verify.

**Field set and order (FIXED, 6 fields):**

| # | Field | Source / meaning |
|---|-------|------------------|
| 1 | `sigV2Tag` | The constant string `offline-package-sig-v2` (domain separator, always line 1) |
| 2 | `version` | Package semver, e.g. `1.4.0` |
| 3 | `digest` | The full `sha256:<lower-hex>` string of the package payload (NOT stripped of prefix) |
| 4 | `minAppVersion` | App-version floor semver, e.g. `3.2.0` |
| 5 | `fileManifestHash` | Directory-integrity hash (see Section 2 for the manifest profile) |
| 6 | `rollbackFloor` | The ledger `minSupportedVersion` anti-downgrade floor (see Section 2) |

**Join and encoding rules:**

- Fields are joined by a **single** `\n` byte (`0x0A`, LF) between adjacent
  fields. There are exactly 5 separator `\n` bytes for the 6 fields.
- **No trailing newline** is appended after the final field, **no BOM**, **no
  CRLF**. The bytes are the UTF-8 encoding of the joined string.
- `sigV2Tag` (`offline-package-sig-v2`) is **always the first line**. It
  domain-separates v2 from the legacy v1 payload (`version + "\n" + digest`), so
  a v1 signature is structurally invalid against the v2 verifier and vice versa.
- The signature algorithm is **Ed25519** (RFC 8032), computed over the raw
  canonical payload bytes (no pre-hashing of the payload beyond what Ed25519
  itself does).
- The `digest` field's hex part is **exactly 64 lower-case hex characters** and
  carries the literal `sha256:` prefix; the **entire** `sha256:<hex>` string
  (prefix included) is part of the signed bytes.
- **Case handling — server MUST emit lower-hex.** The app-side verifier's
  `digest` *equality* check is case-insensitive (it lower-cases before
  comparing), but the server **must still generate `signatureV2` over a
  lower-hex digest**. The digest is signed *verbatim* into the canonical
  payload, so an upper-case (or mixed-case) hex digest produces different
  payload bytes and makes `verifyExtendedSignature` fail — the digest equality
  check would pass while signature verification fails, a contradiction that is
  very hard to diagnose. Lower-hex on the server side is therefore mandatory,
  not merely conventional.

**Wire encoding of the binary values (`signature`, `publicKeys` values):**

- `signature` and each value in `publicKeys` (the 32-byte raw Ed25519 public
  key) are **RFC 4648 standard base64 WITH padding** (the `+`/`/` alphabet and
  trailing `=`). They are **NOT** URL-safe base64 (`-`/`_`) and the padding is
  **not** stripped.
- In the fixtures and the live `active.json` the signature value carries the
  literal `base64:` scheme prefix (e.g. `base64:f9YR...Aw==`); the base64
  payload after the prefix follows the rule above.
- `publicKeys` maps `keyId -> raw 32-byte Ed25519 public key (standard base64)`.
  The 32 raw bytes are the bare public key; the JVM verifier re-wraps them in an
  X.509 SPKI prefix and the iOS verifier feeds them straight to CryptoKit
  `Curve25519.Signing.PublicKey` (see Section 5 for why the SPKI prefix only
  appears on one path).

**`keyId` rules:**

- `keyId` is **case-sensitive** — `Test-Key-1` and `test-key-1` are different
  keys and the verifier does a case-sensitive map lookup.
- `keyId` is a publisher-chosen identifier string. The repo's fixtures use the
  charset `[a-z0-9-]` (lower-case ASCII letters, digits, hyphen); the verifier
  treats it as an opaque UTF-8 map key and does not normalise case or trim
  whitespace.

**Fail-closed newline guard (injectivity, dual-end):**

The `\n`-join is only injective (collision-free) if no field can itself contain
a `\n`. An embedded newline in one field could shift the byte boundary and make
two different field tuples encode to identical bytes (e.g.
`minAppVersion="X\nY", fileManifestHash="M"` would collide with
`minAppVersion="X", fileManifestHash="Y\nM"`), which would let a tampered tuple
ride a signature minted for a different tuple. Both verifiers therefore
**reject fail-closed** any v2 field that contains a newline:

- **Android (Kotlin)** checks `field.contains('\n')` over the joined fields. This
  is a UTF-16 `String` `contains` check.
- **iOS (Swift)** probes the raw UTF-8 **bytes** for `0x0A` via
  `field.utf8.contains(0x0A)`, NOT `String.contains("\n")`. The byte-level probe
  is deliberate: a `String.contains` check can miss a `0x0A` that is mid-grapheme
  in some unusual encodings, whereas the actual delimiter byte is exactly what
  matters for injectivity.

Both ends therefore reject any field whose bytes contain `0x0A`. **CRLF**
(`\r\n`, i.e. `0x0D 0x0A`) is rejected by the same guard because it contains the
`0x0A` byte. No legal value (semver, `sha256:<hex>` digest/manifest, empty
floor) ever contains `0x0A`, so the guard only fires on tampered/malformed
input.

---

## 2. Manifest profile (primary shape) — the empty-tail signature

The **live app-manifest fetch** is the primary v2 shape. When the app pulls
`active.json` and verifies the `signatureV2`, it hands the verifier the 6 fields
with `fileManifestHash` and `rollbackFloor` as the **EMPTY string**. Those two
trailing fields are **SIGNED VERBATIM** (they are part of the bytes the
signature covers) but they are not consumed by the app on the manifest path —
they are witnessed, not read (see Section 4).

The empty-tail canonical payload is therefore:

```
UTF8( sigV2Tag + "\n" + version + "\n" + digest + "\n" + minAppVersion + "\n" + "" + "\n" + "" )
```

This expands to **exactly 5 `\n` bytes**, the last two of which are consecutive —
i.e. the payload **ends with two trailing empty lines** (one for the empty
`fileManifestHash`, one for the empty `rollbackFloor`). There is still **no**
trailing newline beyond those two empty-field separators (the final empty
`rollbackFloor` contributes zero bytes after its preceding `\n`).

### Worked example (values from T3/T1 real output — copied verbatim, not invented)

Inputs:

| Field | Value |
|-------|-------|
| `version` | `1.4.0` |
| `digest` | `sha256:fd3c6583e8cb43379be18d1dbc374a171094c275f094e91d9837f2673ef53c49` |
| `minAppVersion` | `3.2.0` |
| `fileManifestHash` | `` (empty string — empty-tail manifest profile) |
| `rollbackFloor` | `` (empty string — empty-tail manifest profile) |
| `keyId` | `test-key-1` |
| `publicKey` (raw 32-byte, standard base64) | `Ft5xnZ+9IflzXGL66xCrS7tvGWLjq5VSgxt1yj4J0S8=` |

Canonical payload (the exact bytes signed), as a byte-by-byte hexdump:

```
6f66666c696e652d7061636b6167652d7369672d76320a312e342e300a7368613235363a666433633635383365386362343333373962653138643164626333373461313731303934633237356630393465393164393833376632363733656635336334390a332e322e300a0a
```

Decoding that hexdump back to ASCII confirms the layout (each `0a` is a `\n`):

```
offline-package-sig-v2\n1.4.0\nsha256:fd3c6583e8cb43379be18d1dbc374a171094c275f094e91d9837f2673ef53c49\n3.2.0\n\n
```

Expected signature over those exact bytes (Ed25519, the matching private key):

```
signatureV2 = base64:f9YRnYW+wAxIQY9//1rsH3RnA6qHZqEgCpUzR0pfDq8QYcqNopHjz0kpiu37tjBKonbiXxMsqmyqRG2xU4z2Aw==
```

A server reproduction is **bit-exact** if, given the same `version` / `digest` /
`minAppVersion` and the same private key whose public half is
`Ft5xnZ+9IflzXGL66xCrS7tvGWLjq5VSgxt1yj4J0S8=`, the produced canonical-payload
hexdump matches the hexdump above byte-for-byte. (Ed25519 is deterministic per
RFC 8032, so the signature itself is reproducible given the same key; the
hexdump is the primary equality check because it does not require the private
key to verify.)

---

## 3. Roles and the key-distribution handshake

**Role split (who owns what):**

| Concern | Owner |
|---------|-------|
| Key generation | **server** (keys minted server-side) |
| Private key storage | **server** Secret Manager — the private key **never enters the app** |
| Signing (computing `signatureV2`) | **server** publish pipeline |
| Verification | **app** client (the dual-end native verifiers) |

**Distribution ordering invariant (do NOT reorder):**

1. **server mints** a keypair and records the `keyId`.
2. **app ships the public key**: the new `keyId -> publicKey` entry is baked into
   the app bundle and **rolled out to the target install coverage** via an app
   release.
3. **only after that coverage is reached** may the **server start signing with
   that `keyId`**.

Signing with a `keyId` whose public key has not yet reached the target app
coverage is a **contract violation**, not an app bug. The symptom is the app
hitting an **`unknownKeyId` fail-closed** rejection (the verifier has no public
key for that `keyId`, so it refuses the manifest). That is an **availability
incident** caused by violating the ordering, and it is attributed to the
publisher, not to the app verifier.

**Key lifecycle:**

- **Rotation**: rotating a key means shipping the new key in the next app
  version and **removing the old key in that same release**. There is no
  parallel "both keys live forever" state beyond the rollout window needed for
  step 2.
- **Revocation / expiry**: there is **no** revocation mechanism and **no** key
  expiry. The absence of revocation/expiry is a **known, accepted risk** — a
  compromised key is handled by rotation (ship new, drop old) on the next
  release, not by an out-of-band revocation channel.

---

## 4. Threat model and boundary

**What the manifest `signatureV2` binds (authenticity guarantee):**

The v2 signature on `active.json` binds the **authenticity of `version`,
`digest`, and `minAppVersion`** (plus the verbatim empty `fileManifestHash` and
`rollbackFloor` tail). A tampered `version`, `digest`, or `minAppVersion` makes
the signature fail to verify, so the app refuses the manifest.

**What it does NOT bind (known boundary):**

- The signature does **NOT** bind the **persisted ledger `minSupportedVersion`
  plaintext floor** that `OfflineActiveHealer` reads on the local
  ledger/downgrade path. That floor lives in the local ledger blob, not in the
  signed manifest. The downgrade-protection path that reads it is **still not
  covered by a signature** in this contract — this is a **known, accepted
  boundary**. (On the manifest path the `rollbackFloor` field is signed but
  empty; the real ledger floor is a separate persisted value the manifest
  signature does not reach.)
- On the manifest profile, `active.json`'s `fileManifestHash` and
  `rollbackFloor` are **dead fields the app does not read** — they are fixed to
  the empty string and injected purely to keep the signed payload at the fixed
  6-field shape. They participate in the signature (witnessed verbatim) but
  carry no value the app consumes on the manifest path.

---

## 5. In-repo byte-recipe transcription points

The v2 payload byte recipe is hand-transcribed in the following places. Any
change to the recipe (field set, order, separator, tag, encoding) must update
**all** of them in lockstep, or the dual ends and the generators drift.

| Path | Role | Enforcement |
|------|------|-------------|
| `modules/offline-package/android/src/main/kotlin/com/appinfra/offlinepackage/PackageVerifier.kt` | Android native verifier (`extendedCanonicalPayload`) | **CI-hard** — `scripts/verify-all.sh` |
| `modules/offline-package/ios/Sources/OfflinePackage/PackageVerifier.swift` | iOS native verifier (`extendedCanonicalPayload`) | **CI-hard** — `scripts/verify-all.sh` |
| `scripts/gen-verify-extended-fixture.sh` | Mints the v2 golden fixture (`verify-extended-cases.json`) | **advisory** — re-mint manually after a recipe change |
| `scripts/gen-offline-demo-package.sh` (embedded `SelfVerify.java`) | Self-check of the demo package's v1+v2 signatures at generation time | **advisory** — re-mint manually after a recipe change |

**Hard vs advisory:**

- The **two native verifiers** are the **CI-hard** sources of truth: they are
  exercised against the committed fixtures by `scripts/verify-all.sh`, so a
  recipe drift between Android and iOS (or against the fixture) fails CI.
- The **three scripts** are **advisory**: they generate artefacts but are not run
  in the CI conformance gate. After any v2 recipe change, the generated
  artefacts must be **manually re-minted** by running the relevant script.

**SelfVerify.java crypto-path caveat:** the embedded `SelfVerify.java` in
`scripts/gen-offline-demo-package.sh` reconstructs the verify path using the
**JDK Ed25519 via an X.509 SPKI-wrapped key** (`KeyFactory("Ed25519")` +
`X509EncodedKeySpec` over a 12-byte SPKI prefix + the raw 32-byte key). This is a
**different crypto implementation** from the production Android path, which uses
**BouncyCastle's lightweight Ed25519 over the raw 32-byte key** (no SPKI
re-wrapping). Both are RFC-8032 Ed25519 and interoperate on the same payload
bytes, but they are two distinct code paths — a PASS in `SelfVerify.java` is an
interop smoke check, not a test of the production BouncyCastle path.

---

## 6. contractVersion and the server pin

- This document is **contractVersion 1.0.0**, matching the top-level
  `contractVersion: "1.0.0"` of `modules/offline-package/fixtures/verify-extended-cases.json`
  and `modules/offline-package/fixtures/canonical-payload-vectors.json`.
- The server's T4 step **pins the SHA-256 of this document** as its
  reproduction baseline: the server repo records this file's `sha256` so any
  later edit to the byte recipe is detectable as a baseline change.
- **Any change to the payload recipe** (field set, order, the single-`\n`
  separator, `sigV2Tag`, the no-trailing-newline / no-BOM / UTF-8 rules, the
  base64 alphabet, or the empty-tail shape) **MUST bump `contractVersion`** here
  and re-pin the document hash server-side. Editorial changes that do not touch
  the bytes do not bump.

---

## 7. Why this lives in `modules/offline-package/docs/` (not `contracts/`)

This file is placed under `modules/offline-package/docs/` rather than the
repo-level `contracts/` directory to mirror the existing precedent of
`modules/offline-package/docs/manifest-forward-compat.md`. `contracts/` is
reserved for **cross-cutting infra contracts that carry a semver ceremony**
spanning multiple modules; this document is a **single-module byte-layout spec**
local to offline-package, so it belongs with the module's own docs.
