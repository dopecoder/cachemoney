#!/usr/bin/env bash
# deploy_cachemoney.sh <server-public-ip> <ssh-key.pem>
#
# Builds cachemoney from the repo HEAD for linux/arm64 (carrying the gnet event-loop backend)
# and installs it to /usr/local/bin/cachemoney on the server-under-test. The third-party DBs
# are built on the server by user-data; only cachemoney comes from your working tree.
set -euo pipefail

SERVER="${1:?usage: deploy_cachemoney.sh <server-public-ip> <ssh-key.pem>}"
KEY="${2:?usage: deploy_cachemoney.sh <server-public-ip> <ssh-key.pem>}"
SSH_USER="${SSH_USER:-ubuntu}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
OUT="$WORK/cachemoney"

echo ">> building cachemoney (linux/arm64) from $REPO_ROOT"
(cd "$REPO_ROOT" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$OUT" ./cmd/cachemoney)

echo ">> uploading to ${SSH_USER}@${SERVER}"
scp -i "$KEY" -o StrictHostKeyChecking=accept-new "$OUT" "${SSH_USER}@${SERVER}:/tmp/cachemoney"
ssh -i "$KEY" -o StrictHostKeyChecking=accept-new "${SSH_USER}@${SERVER}" \
  'sudo install -m0755 /tmp/cachemoney /usr/local/bin/cachemoney && ls -l /usr/local/bin/cachemoney'

echo ">> cachemoney installed on ${SERVER}"
