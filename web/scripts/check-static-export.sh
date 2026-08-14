#!/usr/bin/env bash
# Smoke check for the Next.js static export.
# Verifies that all expected paths are generated in web/out/.
#
# Usage: ./scripts/check-static-export.sh [out_dir]
#   out_dir: path to the Next.js static export (default: web/out)

set -euo pipefail

OUT_DIR="${1:-out}"
ERRORS=0

# Expected paths that must exist for the settings route, plus the
# standalone analytics route the agents shell toggles into.
EXPECTED_PATHS=(
  "analytics/index.html"
  "settings/index.html"
  "settings/runtimes/index.html"
  "settings/models/index.html"
  "settings/github/index.html"
  "settings/authentication/index.html"
  "settings/issue-trackers/index.html"
  "settings/workspaces/index.html"
  "settings/workflows/index.html"
  "settings/secrets/index.html"
  "settings/mcp-servers/index.html"
  "settings/ai-config/index.html"
  "settings/analytics/index.html"
  "settings/doctor/index.html"
  "settings/troubleshoot/index.html"
  "settings/_workspace/index.html"
  "settings/_workspace/runtimes/index.html"
  "settings/_workspace/models/index.html"
  "settings/_workspace/github/index.html"
  "settings/_workspace/authentication/index.html"
  "settings/_workspace/issue-trackers/index.html"
  "settings/_workspace/workspaces/index.html"
  "settings/_workspace/workflows/index.html"
  "settings/_workspace/secrets/index.html"
  "settings/_workspace/mcp-servers/index.html"
  "settings/_workspace/ai-config/index.html"
  "settings/_workspace/analytics/index.html"
  "settings/_workspace/doctor/index.html"
  "settings/_workspace/troubleshoot/index.html"
)

echo "Checking static export in: $OUT_DIR"
echo ""

for path in "${EXPECTED_PATHS[@]}"; do
  full_path="$OUT_DIR/$path"
  if [ -f "$full_path" ]; then
    echo "  ✓ $path"
  else
    echo "  ✗ MISSING: $path"
    ERRORS=$((ERRORS + 1))
  fi
done

echo ""
if [ "$ERRORS" -eq 0 ]; then
  echo "All expected settings paths are present."
  exit 0
else
  echo "ERROR: $ERRORS expected path(s) missing from static export."
  exit 1
fi
