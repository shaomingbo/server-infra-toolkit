# auth contract schemas

`login.schema.json` and `refresh.schema.json` are the machine-readable wire
truth for the auth login/refresh success responses (T3). They are the source of
truth; the human prose in `docs/CONTRACTS.md` §6 is a summary that defers to
these files when the two disagree.

## Cross-repo consumption

The client repository `app-infra-toolkit` vendors these two schemas by
git-commit-pin and validates its decoder against them in its own CI. Because the
client pins a specific commit, **any commit that changes a file in this
directory MUST carry the `NEEDS-CLIENT-BUMP` marker in its message** so the
client knows to bump its pin.

## `format: "uuid"` validation strength

The `userId` `format: "uuid"` check only verifies the 8-4-4-4-12 hex grouping
shape. It does NOT check the RFC 4122 version or variant bits, so do not read it
as proof that the id is a conformant RFC 4122 UUID — it is a shape guard, not a
spec-conformance guarantee.

Also note: in JSON Schema draft 2020-12, `format` is annotation-only by
default — a validator rejects a malformed value only when format assertion is
explicitly enabled (this repo's conformance test calls `AssertFormat()`). A
consumer that wants `format: "uuid"` to actually reject must confirm its
validator's format-assertion mode is on; otherwise treat the keyword as a
machine-readable semantic declaration, not an enforced constraint.
