package auth

// refresh.go implements POST /v1/auth/refresh: refresh-token ROTATION with
// reuse/replay detection (FR4). Every successful refresh consumes the presented
// token and mints a brand-new access + refresh pair on the SAME rotation chain
// (token_family), so a leaked refresh token has a single use before it is burned.
//
// SECURITY INVARIANTS this file upholds:
//   - Single 401 for EVERY credential failure (unknown selector, wrong verifier,
//     expired, already-revoked, concurrent loser, replay, disabled user). The
//     status and body are byte-identical across these so the client cannot tell
//     which one happened; no token, selector, or user detail ever reaches a log or
//     an error value (the same anti-enumeration discipline as login.go).
//   - The rotation is ATOMIC (AC15): mark-old-used + insert-new-refresh +
//     insert-new-access run inside ONE transaction. A mid-flight error rolls the
//     whole thing back, so the old token is never half-consumed and no orphan new
//     token is left behind.
//   - Concurrency is serialized at TWO levels. (1) A family-level transaction-scoped
//     advisory lock (pg_advisory_xact_lock, keyed on token_family) serializes ALL
//     rotateRefresh calls on the SAME family — acquired BEFORE any FOR UPDATE row
//     lock, so there is a single lock with no lock-ordering cycle and thus no
//     deadlock (two concurrent replays on one family no longer interleave their
//     family-wide RevokeTokenFamily into a DB deadlock → no 500). (2) A FOR UPDATE
//     row lock on the selector then serializes the same single token (AC6): the
//     first commits a rotation, the second sees used_at already set and loses
//     cleanly with a 401, WITHOUT revoking the family (the legitimate user already
//     holds a new token; connecting them would lock the user out).
//   - A genuine REPLAY (a used token presented long after rotation) revokes the
//     whole family (AC5/E3): the chain is assumed compromised, so every token on it
//     is killed and the user must re-authenticate. Because the family advisory lock
//     serializes replay against concurrent rotation, the family-wide revoke can no
//     longer race a concurrent insert of a fresh row that would escape it: either
//     the replay revokes first (the later rotation then reads a revoked row under
//     FOR UPDATE and refuses to insert) or the rotation commits its new row first
//     (the later replay's RevokeTokenFamily then sees and kills it too).

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/shaomingbo/server-infra-toolkit/internal/platform/db/gen"
)

// maxRefreshBodyBytes caps the refresh request body, mirroring maxLoginBodyBytes
// (login.go): the body is a single short token string, so 4 KiB is generous while
// turning an oversized-body probe into a 400 instead of unbounded memory.
const maxRefreshBodyBytes = 4 << 10 // 4 KiB

// refreshReuseLeeway distinguishes a benign concurrency race from a genuine
// replay when a token's used_at is already set. Two legitimate requests carrying
// the same refresh token can arrive nearly simultaneously (e.g. a client retrying
// or two app tabs): the first rotates and sets used_at, the second arrives micro-
// seconds later and finds it set. That second request is the SAME honest user, so
// revoking the family would lock them out (AC6). A token replayed by a thief, in
// contrast, surfaces long after the rotation that consumed it. We treat a used
// token seen within this window as the concurrency loser (401, family kept) and one
// seen later as a replay (revoke the family, 401).
//
// Tuning trade-off (why 30s, not the original 10s):
//   - Too SHORT: a legitimate client that hits a network timeout and retries with
//     the SAME old token — commonly 10-30s later — falls outside the window, gets
//     misjudged as a replay, has its family revoked, and is forced to re-login. The
//     original 10s was too tight for typical client retry timeouts.
//   - Too LONG: detecting a TRUE replay (and revoking its family) is delayed. But
//     the security cost of widening is small: a token reused WITHIN the window
//     already returns 401, so the attacker never obtains a new token regardless;
//     family revocation is only an extra belt-and-suspenders signal for a replay
//     seen LATER, so a longer window merely defers that secondary defense — it does
//     not hand the attacker anything.
//   - 30s balances tolerance for typical client retries against a still-low replay-
//     detection latency. The value is tunable and can be recalibrated against real
//     client refresh-timeout behavior.
const refreshReuseLeeway = 30 * time.Second

// refreshRequest is the wire shape of POST /v1/auth/refresh (D13). The single
// field is required and camelCase; DisallowUnknownFields rejects anything else.
type refreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

// refresh handles POST /v1/auth/refresh. It strictly parses the body, splits the
// presented token into selector + verifier, then rotates (or rejects) inside one
// row-locked transaction. The numbered steps below mirror the PRD refresh flow.
func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	requestID := w.Header().Get(requestIDHeader)

	// (1) Strict parse (NFR8/AC13): cap the body and reject unknown fields. Any
	// structural problem is a 400 — never a panic, never a credential signal.
	req, ok := decodeRefreshRequest(w, r)
	if !ok {
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid request body", requestID)
		return
	}

	// (2) Split "selector.verifier". A token that does not split into exactly two
	// parts is not a valid refresh token; it is an unauthorized credential, not a
	// malformed body — so it gets the SAME generic 401 as a wrong verifier (no
	// distinguishing signal).
	selector, verifier, found := strings.Cut(req.RefreshToken, refreshSeparator)
	if !found || selector == "" || verifier == "" {
		writeError(w, http.StatusUnauthorized, codeUnauthorized, unauthorizedMessage, requestID)
		return
	}

	// (3) Rotate (or reject) atomically under a row lock. rotateRefresh returns the
	// new session on success, or (zero, false, nil) for ANY credential failure
	// (which the caller must map to the single generic 401), or (zero, false, err)
	// for a genuine server error.
	session, ok, err := h.rotateRefresh(r.Context(), selector, verifier)
	if err != nil {
		// A server-side failure (DB error, CSPRNG down). Do NOT leak the cause or
		// any credential; emit a generic 500.
		writeError(w, http.StatusInternalServerError, codeInternal, "internal server error", requestID)
		return
	}
	if !ok {
		// Every credential failure — unknown selector, wrong verifier, expired,
		// revoked, concurrency loser, replay, disabled user — lands here with the
		// SAME 401 and the SAME message, byte-identical, no branch hint.
		writeError(w, http.StatusUnauthorized, codeUnauthorized, unauthorizedMessage, requestID)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(session)
}

// decodeRefreshRequest strictly parses the refresh body exactly as login does:
// http.MaxBytesReader caps the size, DisallowUnknownFields + a trailing-data check
// reject anything but one well-formed object, and refreshToken is required. It
// returns (req, false) on ANY structural error without panicking.
func decodeRefreshRequest(w http.ResponseWriter, r *http.Request) (refreshRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRefreshBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req refreshRequest
	if err := dec.Decode(&req); err != nil {
		return refreshRequest{}, false
	}
	if dec.More() {
		return refreshRequest{}, false
	}
	if req.RefreshToken == "" {
		return refreshRequest{}, false
	}
	return req, true
}

// rotateRefresh performs the full refresh decision inside a single transaction
// that first takes a family-level advisory lock (serializing all rotations on the
// same token_family) and then row-locks the selector's row (FOR UPDATE, serializing
// the same single token). It returns:
//   - (session, true, nil)  on a successful rotation;
//   - (zero, false, nil)    for EVERY credential failure (unknown selector, wrong
//     verifier, expired, revoked, concurrency loser, replay, disabled user) — the
//     caller maps all of these to one generic 401;
//   - (zero, false, err)    only for a genuine server error (DB failure, CSPRNG).
//
// When a genuine replay is detected the family is revoked WITHIN this same
// transaction and the function still reports a credential failure (false, nil), so
// the revocation commits and the client gets a 401. A concurrency loser returns
// the same false WITHOUT revoking the family.
func (h *Handler) rotateRefresh(ctx context.Context, selector, verifier string) (_ loginSession, _ bool, err error) {
	// ISOLATION-LEVEL GUARD: this flow's concurrency correctness depends on the
	// default READ COMMITTED isolation + FOR UPDATE — when a concurrent loser blocks
	// on the row lock, READ COMMITTED re-reads the LATEST committed version after the
	// lock is granted, so the loser sees used_at already set and loses cleanly with a
	// 401 (AC6). If the default isolation is ever raised to REPEATABLE READ /
	// SERIALIZABLE, FOR UPDATE waiting on a lock fails with a 40001 serialization
	// error because the held snapshot is stale, turning the concurrency loser into a
	// 500 instead of a clean 401 (AC6 regression). Keep this on READ COMMITTED.
	tx, err := h.db.Begin(ctx)
	if err != nil {
		return loginSession{}, false, err
	}
	// Roll back on any error path; the rollback after a successful Commit is a
	// documented pgx no-op. A credential failure that revokes the family commits
	// (it returns nil err with ok=false), so the revoke persists; only true server
	// errors roll back.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	qtx := dbgen.New(tx).WithTx(tx)

	// (1) Non-locking pre-read to learn the token_family. We take the family-level
	// advisory lock BEFORE any FOR UPDATE row lock, but the advisory key is the
	// token_family — which we do not yet know — so read the row WITHOUT a lock first.
	// This read is used ONLY to obtain the immutable token_family value; it makes NO
	// security decision (an unknown selector is the sole exception: there is no family
	// to lock and nothing to rotate, so it is the same generic 401 as elsewhere). All
	// authoritative state for the decision comes from the FOR UPDATE read in step (3).
	preread, err := qtx.GetRefreshTokenBySelector(ctx, selector)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return loginSession{}, false, nil
		}
		return loginSession{}, false, err
	}

	// (2) Family advisory lock — taken BEFORE the FOR UPDATE row lock so the lock
	// order is "advisory then row" for EVERY rotateRefresh on this family. A single
	// family-wide lock acquired first means there is no lock-ordering cycle (no
	// path holds a row lock while waiting on the advisory lock or vice versa), so
	// two concurrent replays on the same family — whose RevokeTokenFamily statements
	// would otherwise deadlock on the same batch of rows — are now serialized to a
	// clean 401 each instead of one becoming a 500. It also makes the replay's
	// family-wide revoke atomic against a concurrent rotation's new-row insert, so
	// no freshly inserted token escapes a revoke (see the file-level invariant).
	if err = qtx.LockTokenFamily(ctx, preread.TokenFamily); err != nil {
		return loginSession{}, false, err
	}

	// (3) Row-locked read of the AUTHORITATIVE current state. Only now, holding the
	// family lock, do we FOR UPDATE the selector's row and read its committed state
	// (including db_now). pgx.ErrNoRows here is defensive — the row existed at the
	// pre-read and the family lock serializes mutations, so it should not vanish —
	// but a missing row is still just an unknown token → 401, nothing to revoke.
	// Every security decision below uses THIS row, never the step-(1) pre-read.
	row, err := qtx.GetRefreshTokenBySelectorForUpdate(ctx, selector)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return loginSession{}, false, nil
		}
		return loginSession{}, false, err
	}

	// (4) Constant-time verifier comparison. Decode the presented verifier to raw
	// bytes and SHA-256 it the SAME way token.go stored it, then compare against
	// the stored hash in constant time. A non-match (or a verifier that does not
	// decode) is an unauthorized credential → 401, no revocation.
	verRaw, decErr := tokenEnc.DecodeString(verifier)
	if decErr != nil {
		return loginSession{}, false, nil
	}
	sum := sha256.Sum256(verRaw)
	if subtle.ConstantTimeCompare(sum[:], row.VerifierHash) != 1 {
		return loginSession{}, false, nil
	}

	now := time.Now()

	// (5) Already revoked → 401. The chain was already killed (by a prior replay
	// detection or an explicit logout); nothing more to do.
	if row.RevokedAt.Valid {
		return loginSession{}, false, nil
	}

	// (6) Already used (rotated) → reuse. This MUST come before the expired check:
	// a used token that is ALSO expired is still a replay signal — judging it
	// "expired" first would 401 without revoking the family, letting a replayed-but-
	// expired token slip past replay detection. Distinguish a benign concurrency
	// race from a genuine replay by how long ago it was used.
	if row.UsedAt.Valid {
		// CLOCK: used_at is written by the DB (SQL now() in MarkRefreshTokenUsed), so
		// the grace-window age MUST be measured in the same DB clock domain — db_now
		// (now()::timestamptz read on the SAME locked-row snapshot) minus used_at.
		// Using the app's now() here would mix two clock sources: when the DB clock
		// trails the app's by more than the leeway, a legitimate concurrency loser
		// would be misjudged as a replay and have its family wrongly revoked (AC6
		// regression — locking the honest user out).
		if row.DbNow.Time.Sub(row.UsedAt.Time) <= refreshReuseLeeway {
			// Concurrency loser: the legitimate user already rotated this token in a
			// near-simultaneous request and holds the new one. Do NOT revoke the
			// family (that would lock the honest user out, AC6) — just reject this
			// duplicate with a 401.
			return loginSession{}, false, nil
		}
		// Genuine replay: a consumed token presented well after rotation means the
		// chain is compromised. Revoke the whole family (AC5/E3) within this
		// transaction, then 401. Commit the revocation.
		if rerr := qtx.RevokeTokenFamily(ctx, row.TokenFamily); rerr != nil {
			return loginSession{}, false, rerr
		}
		if cerr := tx.Commit(ctx); cerr != nil {
			return loginSession{}, false, cerr
		}
		committed = true
		return loginSession{}, false, nil
	}

	// (7) Expired → 401. Past the rotation-chain ceiling; not a replay, just stale.
	// CLOCK: expires_at is written by the APP (now+TTL on insert), so it is compared
	// against the app's now() — app↔app is self-consistent, and the TTL dwarfs any
	// DB↔app skew, so this check is unaffected by clock drift. Do NOT switch this to
	// db_now: that would newly mix an app-written value against the DB clock.
	if row.ExpiresAt.Valid && !row.ExpiresAt.Time.After(now) {
		return loginSession{}, false, nil
	}

	// (8) User-status gate (FR13/AC20): a disabled (non-active) user cannot refresh,
	// even with a valid token. Treat any non-"active" status as a credential
	// failure → 401. A vanished user row (ErrNoRows) is likewise a 401.
	user, err := qtx.GetUserByID(ctx, row.UserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return loginSession{}, false, nil
		}
		return loginSession{}, false, err
	}
	if user.Status != userStatusActive {
		return loginSession{}, false, nil
	}

	// (9) All checks passed → rotate. Mark the old token used, mint a NEW refresh
	// token on the SAME family (the rotation chain is preserved — do NOT start a new
	// family), and a new access token. All three writes share this transaction.
	if err = qtx.MarkRefreshTokenUsed(ctx, row.ID); err != nil {
		return loginSession{}, false, err
	}

	access, err := newAccessToken()
	if err != nil {
		return loginSession{}, false, err
	}
	refresh, err := newRefreshToken()
	if err != nil {
		return loginSession{}, false, err
	}

	accessExpiry := now.Add(accessTokenTTL)
	refreshExpiry := now.Add(refreshTokenTTL)

	if _, err = qtx.InsertRefreshToken(ctx, dbgen.InsertRefreshTokenParams{
		UserID:       row.UserID,
		Selector:     refresh.selector,
		VerifierHash: refresh.verifierHash,
		TokenFamily:  row.TokenFamily, // preserve the chain (FR4)
		ExpiresAt:    pgtype.Timestamptz{Time: refreshExpiry, Valid: true},
	}); err != nil {
		return loginSession{}, false, err
	}
	if _, err = qtx.InsertAccessToken(ctx, dbgen.InsertAccessTokenParams{
		UserID:    row.UserID,
		TokenHash: access.hash,
		ExpiresAt: pgtype.Timestamptz{Time: accessExpiry, Valid: true},
	}); err != nil {
		return loginSession{}, false, err
	}

	if err = tx.Commit(ctx); err != nil {
		return loginSession{}, false, err
	}
	committed = true

	return loginSession{
		UserID:       uuidToString(row.UserID),
		AccessToken:  access.plaintext,
		RefreshToken: refresh.plaintext,
		// Unix MILLISECONDS (D13/NFR7), the new access token's expiry.
		ExpiresAt: accessExpiry.UnixMilli(),
	}, true, nil
}
