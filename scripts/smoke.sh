#!/usr/bin/env bash
# smoke.sh runs post-deploy smoke assertions against a live base URL.
#
#   ./scripts/smoke.sh https://my-service-xyz.run.app [expected-version]
#
# It checks only what an external HTTP client can observe:
#   - GET /livez returns 200, body has "status":"ok" and a non-empty version
#     that is not "dev"/"unknown" (and equals the optional expected-version arg).
#   - an unknown route returns 4xx and the JSON error envelope (code + requestId)
# A non-zero exit means the smoke failed.
#
# It does NOT trigger a deploy. Two AC checks are intentionally NOT automated
# here and belong in the deploy runbook (docs/DEPLOY.md), done
# manually/separately:
#   - AC12-③ Cloud Run log entry inspection
#   - AC12-④ Neon "SELECT 1" reachability (run `server -smoke` / `go run ./cmd/api -smoke`)
# The deployer also confirms /livez version is the real revision, not
# dev/unknown.
set -euo pipefail

base="${1:-}"
if [ -z "$base" ]; then
	echo "usage: $0 <base-url> [expected-version]" >&2
	exit 2
fi
base="${base%/}"
expected_version="${2:-}"

echo "==> GET $base/livez"
# -w appends the HTTP status code on its own trailing line.
health_resp=$(curl -fsS -w '\n%{http_code}' "$base/livez")
health_code=$(echo "$health_resp" | tail -n1)
health_json=$(echo "$health_resp" | sed '$d')
if [ "$health_code" != "200" ]; then
	echo "livez: expected 200, got $health_code" >&2
	exit 1
fi
if ! echo "$health_json" | grep -q '"status":[[:space:]]*"ok"'; then
	echo "livez: body missing \"status\":\"ok\": $health_json" >&2
	exit 1
fi

# Extract the version value from "version":"<value>".
version=$(echo "$health_json" | sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
if [ -z "$version" ]; then
	echo "livez: body missing non-empty version field: $health_json" >&2
	exit 1
fi
if [ "$version" = "dev" ] || [ "$version" = "unknown" ]; then
	echo "livez: version is a placeholder ($version), expected a real revision" >&2
	exit 1
fi
if [ -n "$expected_version" ] && [ "$version" != "$expected_version" ]; then
	echo "livez: version is $version, expected $expected_version" >&2
	exit 1
fi

echo "==> GET $base/__no_such_route__ (expect 4xx error envelope)"
# Unknown route returns a non-2xx, so do not use curl -f here.
nf_resp=$(curl -sS -w '\n%{http_code}' "$base/__no_such_route__")
nf_code=$(echo "$nf_resp" | tail -n1)
nf_body=$(echo "$nf_resp" | sed '$d')
case "$nf_code" in
	4??) ;;
	*)
		echo "not-found: expected 4xx, got $nf_code: $nf_body" >&2
		exit 1
		;;
esac
if ! echo "$nf_body" | grep -q '"code"'; then
	echo "not-found: response missing error.code: $nf_body" >&2
	exit 1
fi
if ! echo "$nf_body" | grep -q '"requestId"'; then
	echo "not-found: response missing requestId: $nf_body" >&2
	exit 1
fi

echo "==> smoke OK"
