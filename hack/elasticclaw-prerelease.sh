#!/usr/bin/env bash
set -euo pipefail

REPO="elasticclaw/elasticclaw"
ASSET_NAME="elasticclaw-darwin-arm64"
INSTALL_NAME="elasticclaw-beta"
INSTALL_PATH="/usr/local/bin/${INSTALL_NAME}"

headers=(
  -H "Accept: application/vnd.github+json"
  -H "X-GitHub-Api-Version: 2022-11-28"
)

if [[ -n "${GITHUB_TOKEN:-}" ]]; then
  headers+=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
fi

echo "Finding latest prerelease for ${REPO}..."

download_url="$(
  curl -fsSL "${headers[@]}" "https://api.github.com/repos/${REPO}/releases" \
    | python3 -c '
import json, sys

releases = json.load(sys.stdin)

for r in releases:
    if not r.get("prerelease"):
        continue

    for asset in r.get("assets", []):
        if asset.get("name") == "elasticclaw-darwin-arm64":
            print(asset["browser_download_url"])
            sys.exit(0)

sys.exit("No prerelease asset named elasticclaw-darwin-arm64 found")
'
)"

tmp="$(mktemp)"

echo "Downloading ${download_url}..."
curl -fL "${headers[@]}" -o "$tmp" "$download_url"

chmod +x "$tmp"

echo "Installing to ${INSTALL_PATH}..."
sudo mv "$tmp" "$INSTALL_PATH"

echo "Installed:"
"$INSTALL_PATH" version || true
