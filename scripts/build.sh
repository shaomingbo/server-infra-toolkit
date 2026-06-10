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

# Load IMAGE_REPO (and any other deploy params) from .env if present.
# .env is gitignored; see .env.example for the IMAGE_REPO format.
if [ -f .env ]; then
  set -a
  # shellcheck disable=SC1091
  source .env
  set +a
fi

sha="${1:-$(git rev-parse --short HEAD)}"
image="${IMAGE_REPO:?IMAGE_REPO 未设置,完整镜像仓库路径见 .env.example}:${sha}"

echo "==> docker build --platform linux/amd64 (GIT_SHA=${sha})"
docker build --platform linux/amd64 --build-arg GIT_SHA="${sha}" -t "${image}" .

echo "==> docker push ${image}"
docker push "${image}"

echo "pushed: ${image}"
