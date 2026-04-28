#!/bin/bash
# Manual bootopt — interactive hypothesis testing without LLM
#
# Usage:
#   ./scripts/bootopt-manual.sh [-r /path/to/repo]
#
# This runs the manual.go tool which prompts you to paste hypothesis JSON.
# Great for testing specific ideas before automating.

set -euo pipefail

REPO="${REPO:-$(cd "$(dirname "$0")/.." && pwd)}"

while getopts "r:h" opt; do
  case $opt in
    r) REPO="$OPTARG" ;;
    h)
      echo "Usage: $0 [-r repo]"
      echo "  -r: Path to elasticclaw repo (default: auto-detect)"
      exit 0
      ;;
    *) exit 1 ;;
  esac
done

cd "$REPO"

if ! git diff --quiet; then
  echo "ERROR: Repo has uncommitted changes. Commit or stash first."
  exit 1
fi

echo "=== Manual Bootopt ==="
echo "Repo: $REPO"
echo ""
echo "Paste hypothesis JSON when prompted."
echo "Example:"
echo '{"description":"Test","rationale":"test","target_files":["cmd/claw-bridge/main.go"],"diff":"","risk_level":"low","expected_win":"0ms"}'
echo ""

go run ./cmd/bootopt/manual.go \
  -repo "$REPO" \
  -baseline-runs 3 \
  -timing-runs 3
