#!/usr/bin/env bash
# Smoke check for the Next.js static export.
# Verifies that all expected paths are generated in web/out/.
#
# Usage: ./scripts/check-static-export.sh [out_dir]
#   out_dir: path to the Next.js static export (default: web/out)

set -euo pipefail

OUT_DIR="${1:-out}"
ERRORS=0

# The section list is READ from sections.ts rather than restated here: it is
# the same list page.tsx's generateStaticParams derives the routes from, so a
# hand-maintained copy drifts silently — a section added there but missed here
# is exactly the missing-route failure this script exists to catch.
SECTIONS_FILE="$(cd "$(dirname "$0")/.." && pwd)/app/settings/[[...parts]]/sections.ts"
if [ ! -f "$SECTIONS_FILE" ]; then
  echo "ERROR: cannot read the section list at $SECTIONS_FILE"
  exit 1
fi
SECTIONS=$(sed -n '/VALID_SECTIONS = \[/,/^\]/p' "$SECTIONS_FILE" | sed -n 's/^[[:space:]]*"\([a-z0-9-]*\)".*$/\1/p')
if [ -z "$SECTIONS" ]; then
  echo "ERROR: no sections parsed from $SECTIONS_FILE"
  exit 1
fi

# Expected paths that must exist for the settings route, plus the
# standalone analytics route the agents shell toggles into.
EXPECTED_PATHS=(
  "analytics/index.html"
  "settings/index.html"
  "settings/_workspace/index.html"
)
for section in $SECTIONS; do
  EXPECTED_PATHS+=("settings/$section/index.html")
  EXPECTED_PATHS+=("settings/_workspace/$section/index.html")
done

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
