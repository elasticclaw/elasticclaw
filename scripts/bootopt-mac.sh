#!/bin/bash
# Boot time autoresearch runner for macOS
#
# Usage:
#   export ANTHROPIC_API_KEY=sk-ant-...
#   ./scripts/bootopt-mac.sh [-i 20] [-r /path/to/repo]
#
# Requirements:
#   - Go installed
#   - Git repo checked out
#   - ANTHROPIC_API_KEY env var set

set -euo pipefail

ITERATIONS=10
REPO="${REPO:-$(cd "$(dirname "$0")/.." && pwd)}"
SESSION="$(date +%Y%m%d-%H%M%S)"

while getopts "i:r:s:h" opt; do
  case $opt in
    i) ITERATIONS="$OPTARG" ;;
    r) REPO="$OPTARG" ;;
    s) SESSION="$OPTARG" ;;
    h)
      echo "Usage: $0 [-i iterations] [-r repo] [-s session]"
      echo "  -i: Number of optimization iterations (default: 10)"
      echo "  -r: Path to elasticclaw repo (default: auto-detect)"
      echo "  -s: Session ID (default: timestamp)"
      exit 0
      ;;
    *) exit 1 ;;
  esac
done

if [ -z "${ANTHROPIC_API_KEY:-}" ]; then
  echo "ERROR: ANTHROPIC_API_KEY not set"
  exit 1
fi

cd "$REPO"

# Ensure clean git state
if ! git diff --quiet; then
  echo "ERROR: Repo has uncommitted changes. Commit or stash first."
  exit 1
fi

echo "=== Boot Time Autoresearch ==="
echo "Repo: $REPO"
echo "Iterations: $ITERATIONS"
echo "Session: $SESSION"
echo ""

# Run the autoresearch tool
go run ./cmd/bootopt \
  -iterations "$ITERATIONS" \
  -anthropic-key "$ANTHROPIC_API_KEY" \
  -repo "$REPO" \
  -session "$SESSION" \
  -baseline-runs 5 \
  -timing-runs 5 \
  -test-command "go test ./pkg/hub/"

echo ""
echo "=== Done ==="
echo "State saved to: /tmp/bootopt/bootopt-$SESSION.json"
