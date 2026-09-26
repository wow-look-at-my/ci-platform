#!/opt/homebrew/bin/bash
# Runs a live control plane on localhost against a freshly seeded demo dataset,
# for driving the real UI while developing it. Every start reseeds from scratch.
#
# Sign in with the operator token printed below. The GitHub App values are
# placeholders: GitHub sign-in, webhooks, and check runs have no App behind
# them and fail when used.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo"

port="${PREVIEW_PORT:-8090}"
data="$repo/.preview"
token="preview-operator-token"

rm -rf "$data"
mkdir -p "$data"

go run ./cmd/buildweb
go run ./cmd/demofixtures -seed "$data"
openssl genrsa -out "$data/app-private-key.pem" 2048

export CIPLATFORM_LISTEN="127.0.0.1:$port"
export CIPLATFORM_PUBLIC_URL="http://localhost:$port"
export CIPLATFORM_DATABASE_URL="$data/ciplatform.db"
export CIPLATFORM_BLOB_ROOT="$data/blobs"
export CIPLATFORM_OIDC_KEY_PATH="$data/oidc"
export CIPLATFORM_WEBHOOK_SECRET="preview-webhook-secret"
export CIPLATFORM_JOB_TOKEN_SECRET="preview-job-token-secret"
export CIPLATFORM_OPERATOR_TOKEN="$token"
export CIPLATFORM_SESSION_SECRET="preview-session-secret"
export CIPLATFORM_APP_ID="1"
export CIPLATFORM_APP_PRIVATE_KEY_PATH="$data/app-private-key.pem"
export CIPLATFORM_OAUTH_CLIENT_ID="preview-no-github-app"
export CIPLATFORM_OAUTH_CLIENT_SECRET="preview-no-github-app"
export CIPLATFORM_ALLOWED_OWNERS="PazerOP"
export CIPLATFORM_ADMIN_LOGINS="PazerOP"

echo "preview: http://localhost:$port  operator token: $token"
exec go run ./cmd/ciplatform
