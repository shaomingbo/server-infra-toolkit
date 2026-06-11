#!/usr/bin/env bash
# deploy-precheck.sh runs READ-ONLY pre-deploy checks that catch the five
# high-frequency config mistakes the runbook (docs/DEPLOY.md §1) calls out
# before a manual deploy. It is part of the MANUAL deploy chain alongside
# build.sh / smoke.sh — it is NOT wired into verify.sh and NOT in CI (FR6 / AC8).
#
#   ./scripts/deploy-precheck.sh [git-sha]
#
# The SHA defaults to `git rev-parse --short HEAD`; pass a first arg to check a
# specific image tag (must match the tag build.sh pushed).
#
# Five checks, all read-only (no gcloud writes ever — D6):
#   1. IAM: the runtime service account holds roles/secretmanager.secretAccessor
#      on the NEON_DSN secret (get-iam-policy, read-only).
#   2. Cloud-side DSN sslmode: the NEON_DSN value that production actually uses is
#      the Secret Manager `latest` version, not the local .env. This check both
#      reads it AND verifies it contains sslmode=require — piped straight into grep
#      so the plaintext is NEVER echoed to the terminal/logs and NEVER stored in a
#      shell variable. Failure text distinguishes "unreadable" from "readable but
#      missing sslmode=require".
#   3. Image tag pushed: the [git-sha] tag exists in IMAGE_REPO.
#   4. Local DSN sslmode: local .env NEON_DSN contains sslmode=require (grep only,
#      the DSN body is never echoed).
#   5. Migration version reconciliation: the production DB's current goose version
#      equals the latest migration in db/migrations/. Catches "deployed code but
#      never ran goose up" (the gap T6 found). Reads the cloud-side DSN (Secret
#      Manager latest) into a goose env var — same zero-echo discipline as check 2:
#      the plaintext flows only through GOOSE_DBSTRING inside a subshell, is never
#      echoed and never lands in argv (so it can't surface in `ps`).
#
# Checks 2 and 4 are deliberately BOTH present: they validate the two faces of the
# DSN. Check 2 guards the value production runs with (Secret Manager `latest`);
# check 4 guards the value local dev/smoke runs with (.env). They can drift apart,
# so neither subsumes the other.
#
# Exit codes:
#   0  all four checks passed
#   1  a CONFIG check failed (the named item is missing/wrong) — fix the config
#   3  CREDENTIALS missing (gcloud not installed / not authenticated) — distinct
#      from config failure so E3 "no gcloud creds" is not misreported as a
#      missing config item.
#
# Coordinates are NEVER hardcoded: project comes from `gcloud config`, the
# runtime SA is derived from the project number (or overridden by
# RUNTIME_SERVICE_ACCOUNT in .env), IMAGE_REPO comes from .env. Real GCP
# coordinates live only in the local .env (NFR2).
#
# NOTE: this script does NOT `source .env` (unlike build.sh). The NEON_DSN line
# can contain unquoted shell metacharacters (e.g. `&channel_binding=require`)
# that make `source` parse-error and silently drop every var. We grep values out
# of the file instead — this also keeps the DSN plaintext out of any shell var.
set -uo pipefail

# Exit codes as named constants for readability.
EXIT_CONFIG=1
EXIT_CREDS=3

# Secret name is a frozen runbook constant (docs/DEPLOY.md §0), not a coordinate.
SECRET_NAME="NEON_DSN"
ENV_FILE=".env"

fail_config() {
	echo "配置缺失: $*" >&2
	exit "$EXIT_CONFIG"
}

fail_creds() {
	echo "凭据缺失: $*" >&2
	exit "$EXIT_CREDS"
}

# env_value KEY: read the value of `KEY=...` from .env without sourcing it.
# Takes the last matching uncommented line, strips an optional surrounding pair
# of quotes. Returns empty if the file or key is absent.
env_value() {
	local key="$1" line
	[ -f "$ENV_FILE" ] || return 0
	line=$(grep -E "^[[:space:]]*${key}=" "$ENV_FILE" | tail -n1)
	[ -n "$line" ] || return 0
	line="${line#*=}"
	# strip one matching pair of surrounding double or single quotes
	case "$line" in
		\"*\") line="${line#\"}"; line="${line%\"}" ;;
		\'*\') line="${line#\'}"; line="${line%\'}" ;;
	esac
	printf '%s' "$line"
}

# --- credential gate (E3): distinguish "no gcloud creds" from "config missing" ---
if ! command -v gcloud >/dev/null 2>&1; then
	fail_creds "未找到 gcloud CLI,请先安装 Google Cloud SDK 后重试"
fi

active_account=$(gcloud auth list --filter=status:ACTIVE --format='value(account)' 2>/dev/null | head -n1)
if [ -z "$active_account" ]; then
	fail_creds "gcloud 无活跃登录账号,请先 gcloud auth login"
fi

project=$(gcloud config get-value project 2>/dev/null)
if [ -z "$project" ] || [ "$project" = "(unset)" ]; then
	fail_creds "gcloud 未配置 project,请先 gcloud config set project <PROJECT_ID>"
fi

# Runtime SA: prefer an explicit override from .env, else derive the default
# compute SA from the project number (matches docs/DEPLOY.md §0). Deriving from
# the live project keeps the script free of hardcoded coordinates.
runtime_sa="$(env_value RUNTIME_SERVICE_ACCOUNT)"
if [ -z "$runtime_sa" ]; then
	project_number=$(gcloud projects describe "$project" --format='value(projectNumber)' 2>/dev/null)
	if [ -z "$project_number" ]; then
		fail_creds "无法读取 project number(检查 gcloud 凭据与 project '$project' 访问权限)"
	fi
	runtime_sa="${project_number}-compute@developer.gserviceaccount.com"
fi

echo "==> 部署前置只读校验 (project=$project, account=$active_account)"

# --- check 1: IAM secretAccessor binding on the NEON_DSN secret ---
echo "==> [1/5] IAM: 运行时 SA 对 secret '$SECRET_NAME' 是否持 secretAccessor"
iam_members=$(gcloud secrets get-iam-policy "$SECRET_NAME" \
	--project="$project" \
	--flatten='bindings[].members' \
	--filter="bindings.role=roles/secretmanager.secretAccessor" \
	--format='value(bindings.members)' 2>/dev/null)
iam_status=$?
if [ "$iam_status" -ne 0 ]; then
	fail_config "无法读取 secret '$SECRET_NAME' 的 IAM policy(secret 不存在或当前账号无 viewer 权限)"
fi
if ! printf '%s\n' "$iam_members" | grep -qF "serviceAccount:${runtime_sa}"; then
	fail_config "运行时 SA '$runtime_sa' 未绑定 roles/secretmanager.secretAccessor 到 secret '$SECRET_NAME'(运行时读 secret 会 403,见 docs/DEPLOY.md §1)"
fi
echo "    OK: $runtime_sa 已持 secretAccessor"

# --- check 2: cloud-side DSN (Secret Manager latest) contains sslmode=require ---
# The DSN that production actually uses lives in Secret Manager, NOT in local .env.
# Pipe `versions access` straight into grep: the plaintext flows through the pipe
# only, is never echoed (grep -q discards it) and never lands in a shell variable.
# PIPESTATUS lets us tell "secret unreadable" (left side != 0) apart from "readable
# but missing sslmode=require" (left side 0, grep != 0).
echo "==> [2/5] 云端 DSN: secret '$SECRET_NAME' latest 版本含 sslmode=require"
# grep output goes to /dev/null (not grep -q) so grep consumes ALL of gcloud's
# output: this avoids grep closing the pipe early and handing gcloud a SIGPIPE that
# PIPESTATUS[0] would misread as "unreadable". The plaintext still never surfaces.
gcloud secrets versions access latest --secret="$SECRET_NAME" --project="$project" 2>/dev/null \
	| grep 'sslmode=require' >/dev/null
# Capture the WHOLE PIPESTATUS array in one statement: any later simple command
# (including a scalar assignment) resets PIPESTATUS, which under `set -u` would make
# a second `${PIPESTATUS[1]}` read an unbound element and abort the script.
pipe_status=("${PIPESTATUS[@]}")
access_status=${pipe_status[0]}
grep_status=${pipe_status[1]}
if [ "$access_status" -ne 0 ]; then
	fail_config "secret '$SECRET_NAME' 的 latest 版本不可读(secret/版本不存在或无 accessor 权限)"
fi
if [ "$grep_status" -ne 0 ]; then
	fail_config "secret '$SECRET_NAME' 的 latest 版本可读,但缺少 sslmode=require(云端 DSN 缺 TLS,Neon 连不上,见 docs/DEPLOY.md §1)"
fi
echo "    OK: 云端 DSN 含 sslmode=require(明文未回显)"

# --- check 3: image tag pushed to Artifact Registry ---
sha="${1:-$(git rev-parse --short HEAD 2>/dev/null)}"
if [ -z "$sha" ]; then
	fail_config "无法确定 git SHA(不在 git 仓库内?请显式传入: deploy-precheck.sh <git-sha>)"
fi
image_repo="$(env_value IMAGE_REPO)"
if [ -z "$image_repo" ]; then
	fail_config "IMAGE_REPO 未在 .env 设置(见 .env.example 的 IMAGE_REPO 格式)"
fi
image="${image_repo}:${sha}"
echo "==> [3/5] 镜像: tag '$sha' 是否已推到 IMAGE_REPO"
if ! gcloud artifacts docker images describe "$image" --project="$project" >/dev/null 2>&1; then
	fail_config "镜像 tag '$sha' 未推到仓库(先跑 ./scripts/build.sh $sha;或镜像仓库/凭据有误)"
fi
echo "    OK: 镜像 $sha 已存在"

# --- check 4: local .env NEON_DSN contains sslmode=require (body never echoed) ---
# Grep the .env line directly — never read the DSN into a shell var. The grep
# output is discarded (-q), so the DSN body never reaches the terminal or logs.
echo "==> [4/5] DSN: 本地 .env 的 $SECRET_NAME 含 sslmode=require"
if [ ! -f "$ENV_FILE" ]; then
	fail_config "本地未找到 $ENV_FILE(见 .env.example)"
fi
dsn_line_count=$(grep -cE "^[[:space:]]*${SECRET_NAME}=" "$ENV_FILE")
if [ "$dsn_line_count" -eq 0 ]; then
	fail_config "本地 $ENV_FILE 未提供 $SECRET_NAME(见 .env.example)"
fi
if ! grep -E "^[[:space:]]*${SECRET_NAME}=" "$ENV_FILE" | grep -q 'sslmode=require'; then
	fail_config "本地 $ENV_FILE 的 $SECRET_NAME 缺少 sslmode=require(Neon 强制 TLS,缺了连不上,见 docs/DEPLOY.md §1)"
fi
echo "    OK: DSN 含 sslmode=require(DSN 本体未回显)"

# --- check 5: migration version reconciliation (cloud DB vs local migrations) ---
# Production must have actually run `goose up`: a deploy with code referencing a
# table whose migration never ran on the live DB is the gap T6 found. We compare
# the DB's current goose version against the latest migration file in the repo.
#
# DSN handling mirrors check 2's zero-echo discipline: read the Secret Manager
# `latest` DSN into GOOSE_DBSTRING *inside a subshell* so the plaintext is passed
# to goose via an env var (NOT argv — keeps it out of `ps`), is never echoed, and
# never persists in this script's environment. goose is locked in the tools module
# (`go tool goose`), so the whole thing runs in a `cd tools` subshell.
#
# Self-test bypass: PRECHECK_DSN_OVERRIDE lets the red-path test point at a throwaway
# local DB without touching gcloud. It is for self-testing ONLY and must never be set
# in real deploys (real runs read the cloud DSN from Secret Manager).
#
# Known/accepted side effect: `goose version` against a DB that has NEVER been
# migrated auto-creates an empty goose version table (goose_db_version). No business
# impact — it only adds goose's own bookkeeping table, reports version 0, and the
# reconciliation below then fails loudly telling you to run `goose up`.
echo "==> [5/5] 迁移版本: 生产库 goose 版本 == 本地 db/migrations 最新版本"

# Local expected version = numeric prefix of the highest-numbered migration file.
# Filenames are 5-digit zero-padded (00003_events.sql), so lexical order == numeric
# order. Glob (not `ls | grep`) so odd filenames can't break parsing; `10#` forces
# base-10 so a leading-zero prefix is never misread as octal.
local_version=0
for f in db/migrations/[0-9][0-9][0-9][0-9][0-9]_*.sql; do
	[ -e "$f" ] || continue
	base=${f##*/}
	v=$((10#${base%%_*}))
	[ "$v" -gt "$local_version" ] && local_version="$v"
done
if [ "$local_version" -eq 0 ]; then
	fail_config "db/migrations/ 下未找到形如 00001_*.sql 的迁移文件(无法确定本地期望版本)"
fi

# Read the DB version. The DSN reaches goose only via GOOSE_DBSTRING in this subshell.
# goose prints `goose: version N` to STDERR (stdout stays empty), so we capture 2>&1.
# Real runs source the DSN from Secret Manager; PRECHECK_DSN_OVERRIDE (self-test only)
# short-circuits that. The DSN itself is never echoed — only the parsed integer is.
if [ -n "${PRECHECK_DSN_OVERRIDE:-}" ]; then
	goose_output=$(
		cd tools && GOOSE_DRIVER=postgres GOOSE_DBSTRING="$PRECHECK_DSN_OVERRIDE" \
			go tool goose -dir ../db/migrations version 2>&1
	)
	goose_status=$?
else
	goose_output=$(
		dsn=$(gcloud secrets versions access latest --secret="$SECRET_NAME" --project="$project" 2>/dev/null) || exit 90
		cd tools && GOOSE_DRIVER=postgres GOOSE_DBSTRING="$dsn" \
			go tool goose -dir ../db/migrations version 2>&1
	)
	goose_status=$?
fi
if [ "$goose_status" -eq 90 ]; then
	fail_config "secret '$SECRET_NAME' 的 latest 版本不可读(无法对账迁移版本,见 check 2)"
fi
# Parse `goose: version N` — take the integer after the `version` keyword. If goose
# errored (bad DSN, unreachable DB) there's no such line and db_version stays empty.
db_version=$(printf '%s\n' "$goose_output" | grep -oE 'version[[:space:]]+[0-9]+' | grep -oE '[0-9]+$' | tail -n1)
if [ -z "$db_version" ]; then
	# Do NOT print $goose_output verbatim — on connection errors goose may echo the
	# DSN. Surface only a generic diagnostic.
	fail_config "无法从生产库读取 goose 迁移版本(DB 不可达或 goose 执行失败;诊断已抑制以防 DSN 泄露)"
fi

if [ "$db_version" -eq "$local_version" ]; then
	echo "    OK: 生产库迁移版本 $db_version == 本地最新 $local_version"
elif [ "$db_version" -lt "$local_version" ]; then
	fail_config "有 $((local_version - db_version)) 个迁移未对生产库执行(库 $db_version < 代码 $local_version),先跑 runbook 第 6 节 goose up"
else
	fail_config "库 schema 版本比代码新(库 $db_version > 代码 $local_version,代码可能回滚过),人工核查"
fi

echo "==> 五项前置校验全部通过"
