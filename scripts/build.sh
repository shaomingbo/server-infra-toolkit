#!/usr/bin/env bash
# build.sh builds the linux/amd64 image and pushes it to Artifact Registry.
#
#   ./scripts/build.sh [git-sha]
#
# The SHA defaults to `git rev-parse --short HEAD`; pass a first arg to override.
# It is stamped into main.version via the GIT_SHA build-arg (PRD E4) and used as
# the image tag.
#
# --platform linux/amd64 is REQUIRED: dev machines are Mac arm64 but Cloud Run
# only runs amd64; without it the deployed container fails to start.
#
# It does NOT deploy. The deploy step (with secret/SA flags) lives in the
# runbook (docs/DEPLOY.md) and is run manually.
set -euo pipefail

# Extract IMAGE_REPO from .env if present — the only .env var this script uses.
# We grep the single line out instead of `source .env`: the NEON_DSN line can
# contain unquoted shell metacharacters (e.g. `&channel_binding=require`) that make
# `source` parse-error and silently drop every var. Mirrors deploy-precheck.sh's
# env_value: take the last matching uncommented line, strip one surrounding pair of
# quotes. .env is gitignored; see .env.example for the IMAGE_REPO format.
if [ -f .env ]; then
  # `|| true`: this script runs under `set -e`, and grep exits 1 when IMAGE_REPO is
  # absent — without the guard that would abort here instead of letting the
  # `${IMAGE_REPO:?...}` check below report it.
  line=$(grep -E '^[[:space:]]*IMAGE_REPO=' .env | tail -n1) || true
  if [ -n "$line" ]; then
    line="${line#*=}"
    case "$line" in
      \"*\") line="${line#\"}"; line="${line%\"}" ;;
      \'*\') line="${line#\'}"; line="${line%\'}" ;;
    esac
    IMAGE_REPO="$line"
  fi
fi

sha="${1:-$(git rev-parse --short HEAD)}"
image="${IMAGE_REPO:?IMAGE_REPO 未设置,完整镜像仓库路径见 .env.example}:${sha}"

echo "==> docker build --platform linux/amd64 (GIT_SHA=${sha})"
docker build --platform linux/amd64 --build-arg GIT_SHA="${sha}" -t "${image}" .

echo "==> docker push ${image}"
docker push "${image}"

echo "pushed: ${image}"
