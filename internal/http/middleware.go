package http

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	platformlog "github.com/shaomingbo/server-infra-toolkit/internal/platform/log"
)

// requestIDHeader is the canonical header for propagating a request id.
const requestIDHeader = "X-Request-Id"

// ctxKey is an unexported context key type to avoid collisions.
type ctxKey int

const requestIDKey ctxKey = 0

// RequestIDFromContext returns the request id stored in ctx, or "" if absent.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// newRequestID generates a random request id using crypto/rand, avoiding any
// third-party uuid dependency.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read is documented to never fail on supported platforms.
		// If the system entropy source is unavailable that is a catastrophic
		// failure, and silently degrading to a predictable value would break
		// request-id uniqueness; panic instead of producing a guessable id.
		panic("crypto/rand unavailable: cannot generate request id: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// statusRecorder wraps http.ResponseWriter to capture the status code written by
// the handler for the access log, and tracks whether the response has been
// committed so the recover middleware does not write a second response over an
// already-started one.
//
// It honors net/http semantics: only the first WriteHeader takes effect, and a
// Write before any WriteHeader implies a 200.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(b)
}

// recoverMiddleware is the outermost middleware. It creates the shared
// statusRecorder that wraps the response for the whole chain, catches any panic
// from downstream, and keeps the process alive.
//
// On panic it inspects whether the response has already been committed: if the
// inner handler already wrote a header/body, writing a second 500 would trigger
// a superfluous WriteHeader and corrupt the response, so we only log the panic.
// Only when nothing has been committed yet do we write the 500 error envelope.
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			if p := recover(); p != nil {
				// The inner request-id middleware echoes the id on the response
				// header before the handler runs; read it from there since the
				// context id added downstream does not propagate back out here.
				requestID := w.Header().Get(requestIDHeader)
				if rec.wroteHeader {
					// Response already committed: do not write a second one,
					// just record the panic.
					platformlog.Panic(requestID, r.Method, r.URL.Path, p)
					return
				}
				platformlog.Panic(requestID, r.Method, r.URL.Path, p)
				WriteError(rec, http.StatusInternalServerError, CodeInternal, "internal server error", requestID)
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// requestIDMiddleware reads an inbound X-Request-Id (generating one when
// missing), stores it in the request context, and echoes it on the response.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get(requestIDHeader)
		if requestID == "" {
			requestID = newRequestID()
		}
		w.Header().Set(requestIDHeader, requestID)
		ctx := context.WithValue(r.Context(), requestIDKey, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// accessLogMiddleware emits one structured access-log line per request after the
// handler completes. The response status is read from the shared statusRecorder
// created by the outer recover middleware.
func accessLogMiddleware(version string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// The recover middleware (outermost) wraps the response in a shared
		// statusRecorder before this middleware runs, so w is that recorder.
		rec, _ := w.(*statusRecorder)

		// Log via defer so the access line is emitted even when the inner
		// handler panics. On a panic we attribute status 500 (which the outer
		// recover middleware will produce) and re-panic so recover still writes
		// the error envelope and keeps the process alive.
		defer func() {
			var status int
			if rec != nil && rec.wroteHeader {
				status = rec.status
			}
			if p := recover(); p != nil {
				if status == 0 {
					status = http.StatusInternalServerError
				}
				platformlog.Request(
					RequestIDFromContext(r.Context()),
					r.Method, r.URL.Path, status, time.Since(start), version,
				)
				panic(p)
			}
			if status == 0 {
				status = http.StatusOK
			}
			platformlog.Request(
				RequestIDFromContext(r.Context()),
				r.Method, r.URL.Path, status, time.Since(start), version,
			)
		}()

		next.ServeHTTP(w, r)
	})
}

// chain assembles the fixed middleware order: recover (outermost) → request-id
// → access-log → handler.
//
// ORDER INVARIANT (do not change): request-id MUST nest inside both recover and
// access-log. recoverMiddleware reads the request id from the X-Request-Id
// response header (set by requestIDMiddleware), because the context id added
// downstream does not propagate back out to the recover layer. If request-id is
// moved outside recover, the panic error envelope's requestId silently becomes
// empty. access-log likewise depends on request-id running first to read the id
// from context.
func chain(version string, h http.Handler) http.Handler {
	return recoverMiddleware(
		requestIDMiddleware(
			accessLogMiddleware(version, h),
		),
	)
}
