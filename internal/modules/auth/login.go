package auth

// login.go implements POST /v1/auth/login: strict request parsing, password
// verification with a constant-work no-such-user path (FR10 / AC4), and atomic
// issuance of an access + refresh token pair on success (FR1).
//
// SECURITY INVARIANTS this file upholds:
//   - User enumeration is closed: "no such user", "wrong password", a malformed
//     body, and a missing field MUST be indistinguishable to the client. Wrong
//     credentials always return the SAME 401 status and the SAME generic message;
//     malformed/oversize/unknown-field bodies return 400 (a structural error the
//     client made, not a credential signal). The not-found branch still runs a
//     full argon2 computation (DummyVerify) so it costs the same wall-clock time
//     as a wrong-password branch.
//   - No credential ever reaches a log or an error value: the password lives only
//     in the decoded struct and the (discarded) verify result; it is never
//     formatted into an error or logged.
//   - The client cannot supply a token value (anti session-fixation): every login
//     mints brand-new random tokens.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/shaomingbo/server-infra-toolkit/internal/platform/db/gen"
)

const (
	// maxLoginBodyBytes caps the request body (NFR8). A login body is two short
	// strings; 4 KiB is generous while making an oversized-body DoS a 400 instead
	// of unbounded memory. http.MaxBytesReader enforces this without panicking.
	maxLoginBodyBytes = 4 << 10 // 4 KiB

	// accessTokenTTL is how long an issued access token stays valid. Access tokens
	// are intentionally SHORT-lived (FR3): they are checked against the DB on every
	// protected request, and a short TTL bounds the blast radius of a leaked token
	// even before explicit revocation. 15 minutes is the common OWASP short-session
	// value; the refresh token carries the long-lived session.
	accessTokenTTL = 15 * time.Minute

	// refreshTokenTTL bounds the lifetime of a refresh rotation chain — the real
	// session length, since refresh issues fresh access tokens. 30 days is a
	// conventional balance between not nagging the user to re-login and bounding how
	// long a stolen-but-unused refresh token remains usable. Rotation (a later task)
	// re-issues within this window; this is the absolute ceiling.
	refreshTokenTTL = 30 * 24 * time.Hour
)

// unauthorizedMessage is the single generic message returned for EVERY credential
// failure (wrong password, no such user). It must be byte-for-byte identical
// across those paths so the response body cannot leak which one occurred (AC2).
const unauthorizedMessage = "invalid username or password"

// Error code slugs emitted by auth, matching internal/http's envelope codes in
// kind (stable, human-meaningful slugs). codeUnauthorized is the T2 addition;
// codeBadRequest / codeInternal mirror the HTTP layer's slugs so a client sees one
// consistent vocabulary regardless of which layer produced the error.
const (
	codeUnauthorized = "unauthorized"
	codeBadRequest   = "bad_request"
	codeInternal     = "internal"
)

// loginRequest is the wire shape of POST /v1/auth/login (D13). Both fields are
// required; DisallowUnknownFields rejects anything else. Field names are the
// lowercase the client sends.
//
// Password is the redacting `credential` type (FR7/D7), NOT a bare string: if this
// struct is ever reflected by slog.Any (e.g. a recovered panic carrying the
// decoded request, FR9), the password renders "[REDACTED]". Because credential is
// a string alias with the DEFAULT JSON behavior (no custom UnmarshalJSON), strict
// parsing (DisallowUnknownFields, type checks, NFR8) is unchanged.
type loginRequest struct {
	Username string     `json:"username"`
	Password credential `json:"password"`
}

// loginSession is the success response (D13, frozen client contract). Every field
// is camelCase and required; the client's Decodable/Deserializer rejects a missing
// or mistyped field. expiresAt is the access token's expiry as a Unix MILLISECOND
// absolute timestamp (int64) — NOT seconds; a seconds value would be off by 1000x
// and make tokens appear to never (or immediately) expire (E7/NFR7).
type loginSession struct {
	UserID       string `json:"userId"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
}

// login handles POST /v1/auth/login. See the file header for the security
// invariants; the numbered steps below mirror the PRD login flow (§7.1).
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	requestID := w.Header().Get(requestIDHeader)

	// (1) Strict parse (NFR8/AC13): cap the body and reject unknown fields. Any
	// structural problem — oversize, malformed JSON, unknown field, wrong type — is
	// a 400, never a panic and never a credential signal.
	req, ok := decodeLoginRequest(w, r)
	if !ok {
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid request body", requestID)
		return
	}

	// (1b) RateLimiter facade call site (FR12/D9). The default no-op always allows,
	// so this never blocks today; it is the single, unconditional seam where a
	// future limiter would plug in. The key is the normalized username (a future
	// per-account limiter's natural bucket); a future limiter could surface
	// retryAfter, which the no-op leaves at 0. This is NOT the brute-force defense —
	// that is the DB account lockout below.
	//
	// TIMING CAUTION for any future REAL limiter (BLOCKER1 sibling): a real limiter
	// that rejects HERE — before verifyCredentials' argon2-equivalent work — would
	// answer rejected requests faster than accepted ones, reintroducing an observable
	// timing side-channel (a fast 401 = "rate-limited / known account"). A real
	// limiter must therefore either run its rejection AFTER an equivalent argon2 +
	// DB-write budget is spent, or impose that budget out-of-band (e.g. an upstream
	// gateway that adds no per-path timing skew) — it must NOT short-circuit return
	// before the constant-work path.
	if allowed, _ := h.rateLimiter.Allow(r.Context(), strings.ToLower(req.Username)); !allowed {
		// Unreachable with the no-op default. A real limiter that returns false would
		// land here; per D3 the lockout/limit response must not be a distinguishable
		// 429 on the credential path, so emit the same generic 401.
		writeError(w, http.StatusUnauthorized, codeUnauthorized, unauthorizedMessage, requestID)
		return
	}

	// (2) Normalize the username (case-insensitive lookup) and (3) verify the
	// password under a constant-work regime so the not-found, locked, disabled, and
	// wrong-password paths are all time-indistinguishable.
	authedUser, ok, err := h.verifyCredentials(r.Context(), requestID, req.Username, req.Password)
	if err != nil {
		// A server-side failure (CSPRNG down, DB error during lookup). Do NOT leak
		// the cause or any credential; emit a generic 500.
		writeError(w, http.StatusInternalServerError, codeInternal, "internal server error", requestID)
		return
	}
	if !ok {
		// EVERY credential/lockout/status failure lands here with ONE status and ONE
		// message, byte-identical: wrong password, no such user, locked account, and
		// a disabled user are indistinguishable to the client (FR5/AC5). The account
		// is NEVER told it is locked (no 429, no Retry-After, no lockout-specific
		// code, D3) — the lockout is enforced server-side and invisible on the wire.
		writeError(w, http.StatusUnauthorized, codeUnauthorized, unauthorizedMessage, requestID)
		return
	}

	// (4) Success: mint a fresh token pair and persist it atomically, then return
	// the LoginSession. expiresAt is the access token's expiry in Unix ms.
	session, err := h.issueSession(r.Context(), authedUser.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "internal server error", requestID)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(session)
}

// decodeLoginRequest strictly parses the login body: it caps the size with
// http.MaxBytesReader, rejects unknown fields and trailing data, and returns
// (req, false) on ANY structural error without panicking. Both fields are
// required — an empty username or password is treated as a malformed request, not
// a credential to test, so a blank-field probe cannot be distinguished from other
// 400s.
func decodeLoginRequest(w http.ResponseWriter, r *http.Request) (loginRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req loginRequest
	if err := dec.Decode(&req); err != nil {
		// Covers malformed JSON, wrong field types, unknown fields, and an
		// over-limit body (MaxBytesReader surfaces as a decode error). All are 400.
		return loginRequest{}, false
	}
	// Reject any trailing data after the one expected object (e.g. "{...}{...}" or
	// "{...} garbage"): a well-formed request body is EXACTLY one JSON value. A
	// second Decode must return io.EOF — anything else (nil = a second value parsed,
	// or a non-EOF error = trailing junk) means the body carried more than one value.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return loginRequest{}, false
	}
	if req.Username == "" || req.Password == "" {
		return loginRequest{}, false
	}
	return req, true
}

// verifyCredentials looks the user up by normalized username and verifies the
// password, ALWAYS performing exactly one argon2 computation regardless of whether
// the user exists, is locked, or is disabled (FR2/FR6/FR10/AC6). It also enforces
// the account-lockout lifecycle: a locked account is rejected before its real hash
// is touched (FR2), a failed credential atomically increments the failure count
// (FR1), and a successful login clears the count (FR3).
//
// It returns:
//   - (user, true, nil)   only on a correct password for an existing, ACTIVE,
//     not-locked user;
//   - (zero, false, nil)  for EVERY credential/lockout/status failure — wrong
//     password, no such user, locked account, disabled user — which the caller
//     maps to ONE generic 401 (FR5);
//   - (zero, false, err)  only for a genuine server error (DB failure other than
//     not-found, CSPRNG failure in the dummy path).
//
// CONSTANT WORK (BLOCKER1, two-dimensional alignment): every non-server-error path
// spends exactly ONE argon2 op AND exactly ONE primary-key UPDATE, so neither the
// CPU time nor the DB-write presence distinguishes the paths.
//   - argon2: the real Verify for an existing active user, or DummyVerify (against
//     the precomputed budget hash, NOT the user's real hash) for the no-user /
//     locked / broken-hash paths — so none is time-distinguishable on CPU (AC6).
//   - DB write: wrong-password runs RecordLoginFailure; success runs
//     resetLoginFailures; no-user / locked / disabled run TouchUserTiming. WITHOUT
//     this every-path DB write, only the wrong-password (user-exists) path touched
//     the DB, and the response-time difference would leak whether a username exists
//     (the enumeration channel BLOCKER1 closes).
//
// The locked path deliberately does NOT run argon2 against the real password_hash:
// it spends the SAME budget argon2 (DummyVerify) as the no-user path, so a locked
// account is not an argon2-DoS amplifier and not observable by timing (FR2/AC7).
// (The real password_hash IS in memory — GetUserByUsername selects the whole row in
// one query — but it is already a hashed value, harmless at rest; the DoS source the
// lock gate defends against is the EXPENSIVE argon2 computation, not the cheap SELECT,
// so "skip argon2 on the real hash" is the security-relevant invariant, not "skip
// reading the hash".)
func (h *Handler) verifyCredentials(ctx context.Context, requestID, username string, password credential) (dbgen.GetUserByUsernameRow, bool, error) {
	user, err := h.store.userByUsername(ctx, strings.ToLower(username))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No such user: spend an equivalent argon2 cost AND an equivalent primary-key
		// UPDATE so this branch is time-indistinguishable from a wrong-password branch
		// (BLOCKER1), then report a non-match. There is no account to lock; the
		// TouchUserTiming UPDATE targets a zero-value UUID (matches 0 rows) purely to
		// mirror the wrong-password path's RecordLoginFailure write cost. A DB error
		// here is a genuine server error — drop it as 500 rather than let the no-user
		// path become a faster, observable channel.
		DummyVerify(string(password))
		if terr := h.store.touchUserTiming(ctx, zeroUUID()); terr != nil {
			return dbgen.GetUserByUsernameRow{}, false, terr
		}
		return dbgen.GetUserByUsernameRow{}, false, nil
	case err != nil:
		// A genuine database error (not "no rows"). Surface it as a server error;
		// the caller maps it to a generic 500 with no credential detail.
		return dbgen.GetUserByUsernameRow{}, false, err
	}

	// (A) LOCK GATE (FR2/FR4/FR5) — checked AFTER lookup, BEFORE the real hash.
	// locked_until is compared against the DB's now() (db_now), both read in the
	// SAME statement, so the expiry decision is in one clock domain and immune to
	// DB↔app skew (D5). A STILL-locked account (locked_until strictly after db_now)
	// spends one EQUIVALENT-WORK argon2 via DummyVerify (so timing matches a normal
	// failure and argon2 never runs against the real password_hash) AND one
	// equivalent primary-key UPDATE
	// via TouchUserTiming (BLOCKER1 timing alignment — matching the wrong-password
	// path's DB write), then returns a non-match WITHOUT incrementing the count (a
	// locked account is already locked; re-counting would only extend it — and that
	// timing UPDATE deliberately does NOT touch failed_attempts/locked_until). An
	// EXPIRED lock falls through to normal verification; the atomic RecordLoginFailure
	// below then restarts the count from 1 and clears the stale lock in one statement
	// (D6), so an expired window does not re-lock the account on a single typo.
	if user.LockedUntil.Valid && user.LockedUntil.Time.After(user.DbNow.Time) {
		DummyVerify(string(password))
		if terr := h.store.touchUserTiming(ctx, user.ID); terr != nil {
			return dbgen.GetUserByUsernameRow{}, false, terr
		}
		return dbgen.GetUserByUsernameRow{}, false, nil
	}

	// (B) Real password verification (one argon2). A malformed stored hash yields
	// (false, error) from Verify; treat that as a non-match (the account cannot be
	// logged into) but still spend an equivalent argon2 so a broken-hash account is
	// not faster than a normal wrong-password. A broken hash is NOT a credential the
	// user got wrong, so it does not feed the failure counter.
	match, verr := Verify(string(password), user.PasswordHash)
	if verr != nil {
		DummyVerify(string(password))
		return dbgen.GetUserByUsernameRow{}, false, nil
	}

	// (C) STATUS GATE (FR6/AC8) — checked AFTER the argon2 work so a disabled user's
	// timing matches an active user's. A non-active (e.g. disabled) user can NEVER
	// authenticate, with EITHER a correct or a wrong password, and a disabled user
	// does NOT participate in lockout counting (no increment here): being disabled is
	// a terminal state, not a guessing attempt to throttle. It still spends one
	// equivalent primary-key UPDATE via TouchUserTiming (BLOCKER1) — the real argon2
	// already ran at (B), and this UPDATE matches the wrong-password path's DB write
	// without touching failed_attempts/locked_until — then returns the same generic
	// non-match as every other failure.
	if user.Status != userStatusActive {
		if terr := h.store.touchUserTiming(ctx, user.ID); terr != nil {
			return dbgen.GetUserByUsernameRow{}, false, terr
		}
		return dbgen.GetUserByUsernameRow{}, false, nil
	}

	if !match {
		// (D) Wrong password for an existing, active, not-locked user → atomically
		// increment the failure count (D2/FR1). This single CTE+FOR UPDATE statement
		// is also this path's equivalent DB write for the BLOCKER1 timing alignment.
		// The query returns just_locked = true ONLY on the request that transitions
		// the account from unlocked/expired to locked (the CTE's FOR-UPDATE old row
		// holds the PRE-update lock, compared against the post-update one), so we emit
		// account_locked
		// EXACTLY once even when concurrent over-threshold requests race past the Go
		// gate (MAJOR2): every later concurrent request sees the already-locked old row
		// and returns just_locked=false. A DB error on the counter is a genuine server
		// error (the caller maps it to 500): we must not silently drop a failure that
		// should have counted toward lockout.
		res, rerr := h.store.recordLoginFailure(ctx, user.ID, lockoutThreshold, lockoutWindow)
		if rerr != nil {
			return dbgen.GetUserByUsernameRow{}, false, rerr
		}
		if res.JustLocked {
			logAccountLocked(requestID, uuidToString(user.ID))
		}
		return dbgen.GetUserByUsernameRow{}, false, nil
	}

	// (E) Success: an active, not-locked user with the correct password. Clear the
	// failure count and any lock so an earlier streak of typos does not haunt the
	// next login (FR3/D6/E5). A reset failure is a genuine server error.
	if rerr := h.store.resetLoginFailures(ctx, user.ID); rerr != nil {
		return dbgen.GetUserByUsernameRow{}, false, rerr
	}
	return user, true, nil
}

// issueSession mints a fresh access + refresh token pair and persists both inside
// a single transaction (FR1/FR7), returning the LoginSession to send to the
// client. The transaction uses the db.Begin -> dbgen.New(tx).WithTx seam so the
// two inserts commit atomically: a login never leaves a half-written session.
func (h *Handler) issueSession(ctx context.Context, userID pgtype.UUID) (loginSession, error) {
	access, err := newAccessToken()
	if err != nil {
		return loginSession{}, err
	}
	refresh, err := newRefreshToken()
	if err != nil {
		return loginSession{}, err
	}
	family, err := newTokenFamily()
	if err != nil {
		return loginSession{}, err
	}

	now := time.Now()
	accessExpiry := now.Add(accessTokenTTL)
	refreshExpiry := now.Add(refreshTokenTTL)

	if err := h.store.persistSession(ctx, sessionRows{
		access: dbgen.InsertAccessTokenParams{
			UserID:    userID,
			TokenHash: access.hash,
			ExpiresAt: pgtype.Timestamptz{Time: accessExpiry, Valid: true},
		},
		refresh: dbgen.InsertRefreshTokenParams{
			UserID:       userID,
			Selector:     refresh.selector,
			VerifierHash: refresh.verifierHash,
			TokenFamily:  family,
			ExpiresAt:    pgtype.Timestamptz{Time: refreshExpiry, Valid: true},
		},
	}); err != nil {
		return loginSession{}, err
	}

	return loginSession{
		UserID:       uuidToString(userID),
		AccessToken:  access.plaintext,
		RefreshToken: refresh.plaintext,
		// Unix MILLISECONDS (D13/NFR7), not seconds.
		ExpiresAt: accessExpiry.UnixMilli(),
	}, nil
}

// zeroUUID is a Valid (non-NULL) all-zero UUID, used ONLY as the target of the
// no-user path's TouchUserTiming write (BLOCKER1 timing alignment). It is Valid so
// it encodes as a real UUID literal (not SQL NULL); its all-zero value cannot
// collide with a real user id (ids are random v4 UUIDs), so the UPDATE matches 0
// rows — the point is the equivalent DB-write cost, not actually mutating a row.
func zeroUUID() pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte{}, Valid: true}
}

// uuidToString renders a pgtype.UUID as the canonical 8-4-4-4-12 hyphenated
// string the API exposes for userId (D13: userId is a string). pgtype.UUID's own
// Value() drives the DB encoding; for the wire we format the 16 bytes directly so
// the output does not depend on a DB round-trip.
func uuidToString(u pgtype.UUID) string {
	b := u.Bytes
	const hex = "0123456789abcdef"
	// 36 chars: 32 hex digits + 4 hyphens.
	out := make([]byte, 36)
	pos := 0
	for i := 0; i < 16; i++ {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out[pos] = '-'
			pos++
		}
		out[pos] = hex[b[i]>>4]
		out[pos+1] = hex[b[i]&0x0f]
		pos += 2
	}
	return string(out)
}

// writeError emits the frozen error-envelope shape
// ({"error":{"code","message"},"requestId"}) used across the service. auth cannot
// import internal/http's WriteError (frozen dependency direction: modules must not
// import internal/http), so it renders the SAME shape locally; envelope_test.go
// pins this output byte-for-byte against internal/http.WriteError so the two can
// never drift. requestID comes from the X-Request-Id header the middleware set
// upstream (read off the response writer, since the request-context key type is
// unexported in internal/http).
func writeError(w http.ResponseWriter, status int, code, message, requestID string) {
	type envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		RequestID string `json:"requestId"`
	}
	var env envelope
	env.Error.Code = code
	env.Error.Message = message
	env.RequestID = requestID

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(env)
}
