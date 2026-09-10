#!/usr/bin/env bash
# Deploy the current ElasticClaw hub binary (with embedded web UI) to a remote
# Linux amd64 server.
#
# Usage:
#   hack/deploy-hub.sh <remote-host> [ssh-user] [ssh-port]
#
# Examples:
#   hack/deploy-hub.sh 65.108.123.179
#   hack/deploy-hub.sh 65.108.123.179 root 45863
#
# The script:
#   1. Builds the Next.js web UI static export and copies it into
#      internal/webui/out/ so it gets embedded in the Go binary.
#   2. Cross-compiles bin/elasticclaw-linux-amd64 with -tags embedweb.
#   3. If BRIDGE_IMAGE_PUSH=1, builds and pushes the claw-bridge OCI image so
#      the new hub can download it.
#   4. Backs up the remote /usr/local/bin/elasticclaw to a timestamped file.
#   5. Copies the new binary to /tmp/elasticclaw-new on the remote host.
#   6. Verifies the transferred size looks reasonable.
#   7. Moves the new binary into place, restarts the elasticclaw service,
#      and verifies the service is active and reports the new commit.
#   8. Adds or updates bridge_image in hub.yaml when a bridge image is
#      configured.
#
# Set SKIP_WEB_UI=1 to skip the web UI build (deploys a Go-only binary).
#
# Set BRIDGE_IMAGE to configure the claw-bridge download URL in hub.yaml. This
# is required for branch deployments because the deployed hub version (pr-552)
# does not match a GitHub release, so the default release-based connector URL
# will 404. Example:
#   BRIDGE_IMAGE=https://github.com/elasticclaw/elasticclaw/releases/download/2026.7.21/claw-bridge-linux-amd64 \
#     ./hack/deploy-hub.sh <host> root
#
# Set BRIDGE_IMAGE_PUSH=1 to build the claw-bridge Linux amd64 binary and push
# it to an OCI reference with oras. If BRIDGE_IMAGE is not set, it defaults to
# ttl.sh/${USER}/claw-bridge:1w. If BRIDGE_IMAGE is provided, it must be a valid
# OCI destination such as ttl.sh/<user>/claw-bridge:<tag>. Because ttl.sh uses
# the tag as the TTL specifier (e.g., '1w'), the image name is suffixed with the
# current short commit so concurrent deployments do not overwrite each other.
# For example, ttl.sh/xav/claw-bridge:1w becomes ttl.sh/xav/claw-bridge-99b7854c:1w.
# When the push succeeds the script updates bridge_image in hub.yaml to use the
# unique image. Example:
#   BRIDGE_IMAGE_PUSH=1 ./hack/deploy-hub.sh <host> root
#   BRIDGE_IMAGE=ttl.sh/marc/claw-bridge:1w BRIDGE_IMAGE_PUSH=1 \
#     ./hack/deploy-hub.sh <host> root
#
# This only replaces the binary and restarts the systemd service. It does NOT
# touch the hub database or the systemd unit file. It may add or update
# bridge_image in hub.yaml if BRIDGE_IMAGE is provided.

set -euo pipefail

REMOTE_HOST="${1:-}"
SSH_USER="${2:-root}"
SSH_PORT="${3:-22}"

if [ -z "$REMOTE_HOST" ]; then
  echo "Usage: $0 <remote-host> [ssh-user] [ssh-port]"
  exit 1
fi

SSH_OPTS="-o ConnectTimeout=10 -o BatchMode=no"
SSH="ssh $SSH_OPTS -p $SSH_PORT $SSH_USER@$REMOTE_HOST"
SCP="scp -P $SSH_PORT -o ConnectTimeout=30"

BUILD_TAGS=""
if [ -z "${SKIP_WEB_UI:-}" ]; then
  echo "==> Building web UI static export..."
  (cd web && npm install && npm run build)
  rm -rf internal/webui/out
  cp -r web/out internal/webui/out
  BUILD_TAGS="embedweb"
else
  echo "==> SKIP_WEB_UI set; deploying Go-only binary (no embedded web UI)."
fi

echo "==> Building ElasticClaw hub binary for Linux amd64..."
VERSION="pr-552"
COMMIT="$(git rev-parse --short HEAD)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  ${BUILD_TAGS:+-tags "$BUILD_TAGS"} \
  -ldflags "-X github.com/elasticclaw/elasticclaw/cmd.Version=$VERSION \
            -X github.com/elasticclaw/elasticclaw/cmd.Commit=$COMMIT \
            -X github.com/elasticclaw/elasticclaw/cmd.BuildDate=$BUILD_DATE" \
  -o bin/elasticclaw-linux-amd64 .

LOCAL_SIZE="$(wc -c < bin/elasticclaw-linux-amd64 | tr -d ' ')"
echo "==> Local binary size: $LOCAL_SIZE bytes"

echo "==> Checking bridge image build/push..."
if [ "${BRIDGE_IMAGE_PUSH:-}" = "1" ]; then
  if [ -z "${BRIDGE_IMAGE:-}" ]; then
    BRIDGE_IMAGE="ttl.sh/${USER:-$(whoami)}/claw-bridge:1w"
    echo "==> BRIDGE_IMAGE not set; defaulting to $BRIDGE_IMAGE"
  fi

  case "$BRIDGE_IMAGE" in
    http://*|https://*)
      echo "ERROR: BRIDGE_IMAGE_PUSH=1 requires an OCI reference, not a URL. Got: $BRIDGE_IMAGE"
      exit 1
      ;;
  esac

  if ! command -v oras >/dev/null 2>&1; then
    echo "ERROR: oras is required for BRIDGE_IMAGE_PUSH=1 but it is not in PATH."
    echo "Install oras from https://oras.land/docs/installation"
    exit 1
  fi

  # Split the reference at the last ':' to preserve the ttl.sh TTL tag.
  # The tag (e.g., '1w') is the TTL specifier and must stay intact; uniqueness
  # is added to the image name instead.
  BRIDGE_IMAGE_BASE="${BRIDGE_IMAGE%:*}"
  BRIDGE_IMAGE_TAG="${BRIDGE_IMAGE##*:}"

  if [ "$BRIDGE_IMAGE_BASE" = "$BRIDGE_IMAGE" ] || [ "$BRIDGE_IMAGE_TAG" = "$BRIDGE_IMAGE" ]; then
    echo "ERROR: BRIDGE_IMAGE must include a tag (e.g., ttl.sh/xav/claw-bridge:1w). Got: $BRIDGE_IMAGE"
    exit 1
  fi

  if [[ "$BRIDGE_IMAGE_BASE" == *-${COMMIT} ]]; then
    BRIDGE_IMAGE_UNIQUE="$BRIDGE_IMAGE"
  else
    BRIDGE_IMAGE_UNIQUE="${BRIDGE_IMAGE_BASE}-${COMMIT}:${BRIDGE_IMAGE_TAG}"
  fi

  echo "==> Building claw-bridge binary for Linux amd64..."
  make build-bridge-linux

  echo "==> Pushing claw-bridge to $BRIDGE_IMAGE_UNIQUE with oras..."
  oras push "$BRIDGE_IMAGE_UNIQUE" \
    bin/claw-bridge-linux-amd64:application/octet-stream

  echo "==> Pushed bridge image: $BRIDGE_IMAGE_UNIQUE"
fi

echo "==> Remote version before deployment:"
$SSH "elasticclaw version || true"

echo "==> Backing up remote elasticclaw binary..."
BACKUP_PATH="/usr/local/bin/elasticclaw.bak.$(date -u +%Y%m%d%H%M%S)"
$SSH "sudo cp /usr/local/bin/elasticclaw $BACKUP_PATH && echo 'Backed up to $BACKUP_PATH'"

echo "==> Copying new binary to remote host..."
$SCP $SSH_OPTS bin/elasticclaw-linux-amd64 "$SSH_USER@$REMOTE_HOST:/tmp/elasticclaw-new"

echo "==> Verifying remote binary size..."
REMOTE_SIZE="$($SSH "wc -c < /tmp/elasticclaw-new | tr -d ' '")"
echo "Remote binary size: $REMOTE_SIZE bytes"

if [ "$LOCAL_SIZE" != "$REMOTE_SIZE" ]; then
  echo "ERROR: size mismatch (local=$LOCAL_SIZE remote=$REMOTE_SIZE). Aborting before replacing."
  echo "Rollback: remote /tmp/elasticclaw-new was not installed. Backup remains at $BACKUP_PATH."
  exit 1
fi

echo "==> Installing binary and restarting service..."
$SSH "
  set -e
  sudo mv /tmp/elasticclaw-new /usr/local/bin/elasticclaw
  sudo chmod +x /usr/local/bin/elasticclaw
  echo 'Installed version:'
  elasticclaw version
  sudo systemctl restart elasticclaw
  sleep 3
  sudo systemctl is-active elasticclaw
  echo 'Service is active.'
"

echo "==> Checking connector bridge_image configuration..."

REMOTE_HUB_USER="$($SSH "
  for f in /etc/systemd/system/elasticclaw.service /etc/systemd/system/multi-user.target.wants/elasticclaw.service /lib/systemd/system/elasticclaw.service; do
    if [ -f \"\${f}\" ]; then
      u=\$(grep -E '^User=' \"\${f}\" 2>/dev/null | head -1 | cut -d= -f2)
      if [ -n \"\${u}\" ]; then
        echo \"\${u}\"
        exit 0
      fi
      break
    fi
  done
  echo root
")"
echo "==> Hub service user: $REMOTE_HUB_USER"

HUB_CONFIG_PATH="$($SSH "
  if [ -n \"\${ELASTICCLAW_HUB_CONFIG:-}\" ]; then
    echo \"\$ELASTICCLAW_HUB_CONFIG\"
  elif [ -f /etc/elasticclaw/hub.yaml ]; then
    echo /etc/elasticclaw/hub.yaml
  else
    echo ~$REMOTE_HUB_USER/.elasticclaw/hub.yaml
  fi
")"
echo "==> Using hub config: $HUB_CONFIG_PATH"

CURRENT_BRIDGE_IMAGE="$($SSH "
  if [ -f '$HUB_CONFIG_PATH' ]; then
    grep -E '^[[:space:]]*bridge_image:' '$HUB_CONFIG_PATH' | head -1 | sed 's/^[[:space:]]*bridge_image:[[:space:]]*//' | tr -d '\"' || true
  fi
")"

HUB_CONFIG_DIR="$(dirname "$HUB_CONFIG_PATH")"
SSH_USER_HOME="$($SSH "getent passwd \"$SSH_USER\" 2>/dev/null | cut -d: -f6")"
if [ -n "$SSH_USER_HOME" ] && [[ "$HUB_CONFIG_PATH" == "$SSH_USER_HOME"* ]]; then
  CONFIG_SUDO=""
else
  CONFIG_SUDO="sudo"
fi

# setBridgeImage deletes any existing bridge_image line in hub.yaml and appends
# the requested value. This keeps the file idempotent across repeated runs.
setBridgeImage() {
  local value="$1"
  echo "Setting bridge_image in $HUB_CONFIG_PATH to $value..."
  $SSH "${CONFIG_SUDO:+sudo }sh -c 'mkdir -p \"$HUB_CONFIG_DIR\" && sed -i \"/^[[:space:]]*bridge_image:/d\" \"$HUB_CONFIG_PATH\" 2>/dev/null || true; echo \"bridge_image: $value\" >> \"$HUB_CONFIG_PATH\"'"
}

if [ "${BRIDGE_IMAGE_PUSH:-}" = "1" ] && [ -n "${BRIDGE_IMAGE:-}" ]; then
  if [ "$CURRENT_BRIDGE_IMAGE" = "$BRIDGE_IMAGE_UNIQUE" ]; then
    echo "bridge_image already set to $BRIDGE_IMAGE_UNIQUE in $HUB_CONFIG_PATH"
    echo "The hub will use the pushed image for connector downloads."
  else
    setBridgeImage "$BRIDGE_IMAGE_UNIQUE"
    echo "Restarting elasticclaw to pick up the new bridge_image..."
    $SSH "sudo systemctl restart elasticclaw && sleep 2 && sudo systemctl is-active elasticclaw"
  fi
elif [ -n "$CURRENT_BRIDGE_IMAGE" ]; then
  echo "Found bridge_image in $HUB_CONFIG_PATH: $CURRENT_BRIDGE_IMAGE"
  echo "The hub will use this URL for connector downloads."
elif [ -n "${BRIDGE_IMAGE:-}" ]; then
  setBridgeImage "$BRIDGE_IMAGE"
  echo "Restarting elasticclaw to pick up the new bridge_image..."
  $SSH "sudo systemctl restart elasticclaw && sleep 2 && sudo systemctl is-active elasticclaw"
else
  echo "WARNING: bridge_image is not configured in $HUB_CONFIG_PATH and BRIDGE_IMAGE is not set."
  echo "Connector (claw-bridge) downloads will fail with a 404 because the deployed hub version"
  echo "($VERSION) does not match a GitHub release."
  echo ""
  echo "To fix, rerun the script with BRIDGE_IMAGE set, e.g.:"
  echo "  BRIDGE_IMAGE=https://github.com/elasticclaw/elasticclaw/releases/download/2026.7.21/claw-bridge-linux-amd64 $0 $REMOTE_HOST $SSH_USER $SSH_PORT"
  echo ""
  echo "Or manually edit $HUB_CONFIG_PATH and add:"
  echo "  bridge_image: https://github.com/elasticclaw/elasticclaw/releases/download/2026.7.21/claw-bridge-linux-amd64"
fi

echo "==> Deployment complete."
echo "Backup available at: $BACKUP_PATH"
