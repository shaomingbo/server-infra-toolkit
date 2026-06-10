#!/usr/bin/env bash
# deploy-precheck.sh runs READ-ONLY pre-deploy checks that catch the four
# high-frequency config mistakes the runbook (docs/DEPLOY.md §1) calls out
# before a manual deploy. It is part of the MANUAL deploy chain alongside
# build.sh / smoke.sh — it is NOT wired into verify.sh and NOT in CI (FR6 / AC8).
#
#   ./scripts/deploy-precheck.sh [git-sha]
#
# The SHA defaults to `git rev-parse --short HEAD`; pass a first arg to check a
# specific image tag (must match the tag build.sh pushed).
#
# Four checks, all read-only (no gcloud writes ever — D6):
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
echo "==> [1/4] IAM: 运行时 SA 对 secret '$SECRET_NAME' 是否持 secretAccessor"
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
echo "==> [2/4] 云端 DSN: secret '$SECRET_NAME' latest 版本含 sslmode=require"
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
echo "==> [3/4] 镜像: tag '$sha' 是否已推到 IMAGE_REPO"
if ! gcloud artifacts docker images describe "$image" --project="$project" >/dev/null 2>&1; then
	fail_config "镜像 tag '$sha' 未推到仓库(先跑 ./scripts/build.sh $sha;或镜像仓库/凭据有误)"
fi
echo "    OK: 镜像 $sha 已存在"

# --- check 4: local .env NEON_DSN contains sslmode=require (body never echoed) ---
# Grep the .env line directly — never read the DSN into a shell var. The grep
# output is discarded (-q), so the DSN body never reaches the terminal or logs.
echo "==> [4/4] DSN: 本地 .env 的 $SECRET_NAME 含 sslmode=require"
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

echo "==> 四项前置校验全部通过"
