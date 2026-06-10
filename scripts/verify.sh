#!/usr/bin/env bash
# verify.sh is the single local/CI verification entry point. CI must only call
# this script and never spell out the individual steps, so the gate stays
# defined in one place (AC8/AC9/AC16).
#
# This does NOT trigger any deploy and does NOT run the Neon/Cloud Run smoke
# (FR5/FR7: keep the PR gate fast and deterministic).
set -euo pipefail

cd "$(dirname "$0")/.."

# 1. gofmt check. gofmt -l only prints unformatted files and never exits
# non-zero, so we inspect its output ourselves (E6).
echo "==> gofmt"
fmt_out=$(gofmt -l .)
if [ -n "$fmt_out" ]; then
	echo "gofmt found unformatted files:"
	echo "$fmt_out"
	exit 1
fi

# 2. go vet
echo "==> go vet"
go vet ./...

# 3. tests
echo "==> go test"
go test -count=1 ./...

# 4. Dependency-direction check (AC16/NFR3): internal/platform/* must never
# depend on internal/http or internal/modules. We compute each platform
# subpackage's full dependency closure and fail if a forbidden package appears.
echo "==> dependency direction"
module="github.com/shaomingbo/server-infra-toolkit"
forbidden="$module/internal/http $module/internal/modules"

platform_pkgs=$(go list "$module/internal/platform/..." 2>/dev/null)
for pkg in $platform_pkgs; do
	deps=$(go list -deps "$pkg")
	for bad in $forbidden; do
		if echo "$deps" | grep -qx "$bad" || echo "$deps" | grep -q "^$bad/"; then
			echo "dependency-direction violation: $pkg depends on $bad"
			exit 1
		fi
	done
done

# NFR3 (other half): internal/http must never depend on internal/modules. This
# is forward-looking: internal/modules does not exist yet, so the closure cannot
# contain it today and this passes vacuously, but it guards the boundary once
# modules land.
http_forbidden="$module/internal/modules"
http_pkgs=$(go list "$module/internal/http/..." 2>/dev/null)
for pkg in $http_pkgs; do
	deps=$(go list -deps "$pkg")
	if echo "$deps" | grep -qx "$http_forbidden" || echo "$deps" | grep -q "^$http_forbidden/"; then
		echo "dependency-direction violation: $pkg depends on $http_forbidden"
		exit 1
	fi
done

# 5. sqlc drift gate (FR11/AC12): regenerate sqlc output and fail if it differs
# from what is committed, so query/schema edits without a matching `sqlc
# generate` never sneak in. The tool is pinned in the separate tools module
# (tools/go.mod), invoked via `go tool sqlc`; sqlc.yaml lives at repo root and
# its schema/queries/out paths are relative to that root regardless of CWD.
#
# Config-existence gating (AC12, anti-false-green): only run when sqlc.yaml
# exists, otherwise print an explicit skip reason and keep going (exit 0). A
# silent skip would let a deleted config masquerade as a passing gate.
echo "==> sqlc drift"
if [ ! -f sqlc.yaml ]; then
	echo "skip sqlc gate: sqlc.yaml 不存在"
else
	gen_dir="internal/platform/db/gen"
	(cd tools && go tool sqlc -f ../sqlc.yaml generate)
	# 用 git status --porcelain 而非 git diff:git diff 只比工作区 vs index,对【未跟踪的
	# 新生成文件】是盲的(新增 query 生成新 gen 文件却漏 git add 时会假绿)。porcelain
	# 同时覆盖 已跟踪文件的改/删 与 未跟踪新文件,非空即漂移。
	drift="$(git status --porcelain -- "$gen_dir")"
	if [ -n "$drift" ]; then
		echo "sqlc drift: $gen_dir 与提交内容不一致(含未提交的新生成文件);请在 tools 目录跑 'go tool sqlc -f ../sqlc.yaml generate' 并提交结果"
		echo "$drift"
		exit 1
	fi
fi

# 6. migration gate (FR11/AC13/AC14): apply every migration up then down
# against a *dockerized / localhost* Postgres and assert the self-test table
# appears after up and is gone after down. This NEVER touches Neon: the DSN
# comes only from TEST_DATABASE_URL (CI points it at the Postgres service
# container; locally at a throwaway container). No DSN is hardcoded here.
#
# Gating (AC12, anti-false-green): run only when the migrations dir is
# non-empty AND the test Postgres is reachable; otherwise print an explicit
# skip reason and continue (exit 0).
echo "==> migration gate"
migrations_dir="db/migrations"
if [ -z "$(ls -A "$migrations_dir"/*.sql 2>/dev/null)" ]; then
	echo "skip migration gate: $migrations_dir 无迁移文件"
elif [ -z "${TEST_DATABASE_URL:-}" ]; then
	echo "skip migration gate: 未设置 TEST_DATABASE_URL(本地需先起 dockerized Postgres 并导出该变量)"
elif ! command -v psql >/dev/null 2>&1; then
	echo "skip migration gate: 未找到 psql(断言依赖 psql)"
elif ! psql "$TEST_DATABASE_URL" -tAc 'SELECT 1' >/dev/null 2>&1; then
	echo "skip migration gate: TEST_DATABASE_URL 指向的测试 Postgres 不可达"
else
	selftest_tbl="public._infra_selftest"

	(cd tools && go tool goose -dir ../db/migrations postgres "$TEST_DATABASE_URL" up)
	if [ "$(psql "$TEST_DATABASE_URL" -tAc "SELECT to_regclass('$selftest_tbl') IS NOT NULL")" != "t" ]; then
		echo "migration gate: up 后 $selftest_tbl 不存在(up 断言失败)"
		exit 1
	fi

	# down-to 0 回滚【全部】迁移(不是 `down` 的单步):未来多于一个迁移时,`down` 只
	# 回退最新一条,自验表(首个迁移建)仍在,会让下面的 IS NULL 断言误红。down-to 0
	# 保证无论迁移数量,自验表都被回滚掉,round-trip 语义对未来稳健。
	(cd tools && go tool goose -dir ../db/migrations postgres "$TEST_DATABASE_URL" down-to 0)
	if [ "$(psql "$TEST_DATABASE_URL" -tAc "SELECT to_regclass('$selftest_tbl') IS NULL")" != "t" ]; then
		echo "migration gate: down-to 0 后 $selftest_tbl 仍存在(down 断言失败)"
		exit 1
	fi
fi

echo "==> verify OK"
