#!/usr/bin/env bash
set -euo pipefail

PR_NUMBER="${1:-}"
BASE_BRANCH="${2:-main}"
REPORT_FILE="${3:-clawpatch-report.md}"

if [[ -z "$PR_NUMBER" ]]; then
  echo "usage: $0 <pr-number> [base-branch] [report-file]"
  exit 1
fi

command -v git >/dev/null || { echo "missing git"; exit 1; }
command -v gh >/dev/null || { echo "missing gh"; exit 1; }
command -v clawpatch >/dev/null || { echo "missing clawpatch"; exit 1; }

if ! gh auth status >/dev/null 2>&1; then
  echo "gh is not authenticated. Run: gh auth login"
  exit 1
fi

git fetch origin "$BASE_BRANCH"

clawpatch ci \
  --since "origin/$BASE_BRANCH" \
  --output "$REPORT_FILE"

gh pr comment "$PR_NUMBER" \
  --body-file "$REPORT_FILE"

echo "Posted Clawpatch report to PR #$PR_NUMBER"