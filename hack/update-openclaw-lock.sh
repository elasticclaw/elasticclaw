#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VERSION="$(sed -n 's/^[[:space:]]*OpenClawVersion[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "$ROOT/pkg/cliversion/cliversion.go")"
test -n "$VERSION"
RUNTIME_TMP="$(mktemp -d)"
trap 'rm -rf "$RUNTIME_TMP"' EXIT
cd "$RUNTIME_TMP"
printf '{ "name": "elasticclaw-openclaw-runtime", "private": true, "version": "0.0.0", "dependencies": { "openclaw": "%s" } }\n' "$VERSION" > package.json
npm install --package-lock-only --ignore-scripts --no-audit --no-fund
cp package.json package-lock.json "$ROOT/pkg/cliversion/openclaw/"
