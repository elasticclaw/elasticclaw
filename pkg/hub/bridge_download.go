package hub

import (
	"fmt"

	"github.com/elasticclaw/elasticclaw/pkg/cliversion"
)

func bridgeDownloadShell(ref, digest, destination string) string {
	return fmt.Sprintf(`BRIDGE_SRC=%s
BRIDGE_SHA256=%s
BRIDGE_PATH=%s
download_connector_once() {
  rm -f "$BRIDGE_PATH" || return 1
  if echo "$BRIDGE_SRC" | grep -qE '^https?://'; then
    curl -fsSL --proto '=https,http' --proto-redir '=https' "$BRIDGE_SRC" -o "$BRIDGE_PATH" || return 1
  else
    # OCI ref — oras verifies pinned manifest and blob digests.
    if ! command -v oras &>/dev/null; then
      echo "Installing oras..."
      ORAS_DIR=$(mktemp -d) || return 1
      curl -fsSL --proto '=https' --proto-redir '=https' https://github.com/oras-project/oras/releases/download/v%s/oras_%s_linux_amd64.tar.gz -o "$ORAS_DIR/oras.tar.gz" || { rm -rf "$ORAS_DIR"; return 1; }
      echo "%s  $ORAS_DIR/oras.tar.gz" | sha256sum -c - || { echo "ERROR: oras SHA256 mismatch"; rm -rf "$ORAS_DIR"; return 1; }
      tar xzf "$ORAS_DIR/oras.tar.gz" -C "$ORAS_DIR" || { rm -rf "$ORAS_DIR"; return 1; }
      sudo mv "$ORAS_DIR/oras" /usr/local/bin/oras || { rm -rf "$ORAS_DIR"; return 1; }
      rm -rf "$ORAS_DIR"
    fi
    sudo apt-get install -y curl ca-certificates 2>/dev/null || true
    BRIDGE_DIR=$(mktemp -d) || return 1
    oras pull "$BRIDGE_SRC" -o "$BRIDGE_DIR" || { rm -rf "$BRIDGE_DIR"; return 1; }
    BINARY=$(find "$BRIDGE_DIR" -name 'claw-bridge*' -type f | head -1)
    if [ -z "$BINARY" ]; then
      echo "ERROR: ElasticClaw connector binary not found after oras pull"
      rm -rf "$BRIDGE_DIR"
      return 1
    fi
    cp "$BINARY" "$BRIDGE_PATH" || { rm -rf "$BRIDGE_DIR"; return 1; }
    rm -rf "$BRIDGE_DIR"
  fi
  if [ -n "$BRIDGE_SHA256" ]; then
    echo "$BRIDGE_SHA256  $BRIDGE_PATH" | sha256sum -c - || { echo "ERROR: claw-bridge SHA256 mismatch"; return 1; }
  else
    echo "[supply-chain] WARNING: claw-bridge binary SHA256 unavailable (pinned OCI digests are verified by oras)"
  fi
}
`, shellQuote(ref), shellQuote(digest), shellQuote(destination), cliversion.OrasVersion, cliversion.OrasVersion, cliversion.OrasLinuxAMD64SHA256)
}

func daytonaBridgeDownloadScript(ref, digest string) string {
	return "set -euo pipefail\n" + bridgeDownloadShell(ref, digest, "/tmp/claw-bridge.download") + `
download_connector_once || exit 1
chmod +x "$BRIDGE_PATH"
mv -f "$BRIDGE_PATH" /tmp/claw-bridge
echo downloaded`
}
