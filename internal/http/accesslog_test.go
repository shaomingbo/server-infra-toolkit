package http

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
)

// captureStdout redirects OS file descriptor 1 to a pipe while fn runs and
// returns everything written to it. The platform/log package binds os.Stdout
// (fd 1) at init, so redirecting at the fd level is required to capture it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	origFd, err := syscall.Dup(1)
	if err != nil {
		t.Fatalf("dup stdout: %v", err)
	}
	if err := syscall.Dup2(int(w.Fd()), 1); err != nil {
		t.Fatalf("dup2 stdout: %v", err)
	}

	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			sb.WriteString(sc.Text())
			sb.WriteByte('\n')
		}
		done <- sb.String()
	}()

	fn()

	// Restore fd 1, close the write end so the reader goroutine finishes.
	_ = syscall.Dup2(origFd, 1)
	_ = syscall.Close(origFd)
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

func parseAccessLine(t *testing.T, out string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m["msg"] == "request" {
			return m
		}
	}
	t.Fatalf("no access-log line found in output: %q", out)
	return nil
}

// AC2c / AC4: one JSON access line is emitted with the six required fields
// non-empty, and request_id equals the inbound X-Request-Id.
func TestAccessLog_SixFieldsAndInboundID(t *testing.T) {
	srv := testServer()

	const inbound = "inbound-trace-id"
	out := captureStdout(t, func() {
		req := httptest.NewRequest(http.MethodGet, "/livez", nil)
		req.Header.Set(requestIDHeader, inbound)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
	})

	m := parseAccessLine(t, out)

	if m["request_id"] != inbound {
		t.Fatalf("request_id = %v, want %q", m["request_id"], inbound)
	}
	if m["method"] != http.MethodGet {
		t.Fatalf("method = %v, want GET", m["method"])
	}
	if m["path"] != "/livez" {
		t.Fatalf("path = %v, want /livez", m["path"])
	}
	// JSON numbers decode to float64.
	if status, ok := m["status"].(float64); !ok || status != float64(http.StatusOK) {
		t.Fatalf("status = %v, want 200", m["status"])
	}
	if _, ok := m["latency_ms"]; !ok {
		t.Fatalf("latency_ms field missing")
	}
	if v, ok := m["version"].(string); !ok || v == "" {
		t.Fatalf("version = %v, want non-empty", m["version"])
	}
}
