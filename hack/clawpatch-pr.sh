#!/usr/bin/env bash
set -euo pipefail

cmd="${1:-}"

base_branch="${BASE_BRANCH:-main}"
remote="${REMOTE:-origin}"
review_limit="${CLAWPATCH_REVIEW_LIMIT:-10}"
report_output="${CLAWPATCH_REPORT:-.clawpatch/report.md}"
validate_cmd="${CLAWPATCH_VALIDATE:-make test}"

usage() {
  cat <<'EOF'
Usage:
  hack/clawpatch-pr.sh init
  hack/clawpatch-pr.sh review
  hack/clawpatch-pr.sh report
  hack/clawpatch-pr.sh show
  hack/clawpatch-pr.sh triage
  hack/clawpatch-pr.sh pr

Environment:
  FINDING                 Fix this finding id instead of selecting the top open finding.
  STATUS                  Triage status for clawpatch-triage.
  NOTE                    Optional triage note.
  BASE_BRANCH            Base branch for PRs. Default: main.
  REMOTE                 Git remote to push to. Default: origin.
  CLAWPATCH_REVIEW_LIMIT Number of features to review. Default: 10.
  CLAWPATCH_REPORT       Markdown report path. Default: .clawpatch/report.md.
  CLAWPATCH_VALIDATE     Validation command before commit. Default: make test.
EOF
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "error: $1 is required" >&2
    exit 1
  }
}

require_clean_worktree() {
  if [[ -n "$(git status --porcelain)" ]]; then
    echo "error: git worktree must be clean before creating a Clawpatch PR" >&2
    git status --short >&2
    exit 1
  fi
}

require_base_branch() {
  local current
  current="$(git branch --show-current)"
  if [[ "$current" != "$base_branch" ]]; then
    echo "error: run from ${base_branch}; current branch is ${current}" >&2
    exit 1
  fi
}

ensure_clawpatch_initialized() {
  if ! clawpatch status >/dev/null 2>&1; then
    clawpatch init
  fi
}

json_field() {
  local json="$1"
  local filter="$2"
  jq -r "$filter // empty" <<<"$json"
}

finding_json_from_next() {
  clawpatch next --status open --json 2>/dev/null || true
}

finding_json_from_report() {
  clawpatch report --status open --json \
    | jq -c '
      def sev_rank:
        {"critical":0,"high":1,"medium":2,"low":3}[.severity] // 99;
      def conf_rank:
        {"high":0,"medium":1,"low":2}[.confidence] // 9;

      [
        .. | objects
        | {
            id: (.id // .findingId // .finding.id // empty),
            severity: ((.severity // "") | ascii_downcase),
            confidence: ((.confidence // "") | ascii_downcase),
            category: (.category // ""),
            title: (.title // .summary // .message // ""),
            status: ((.status // "open") | ascii_downcase)
          }
        | select(.id != "" and .status == "open" and .severity != "")
      ]
      | unique_by(.id)
      | sort_by(sev_rank, conf_rank, .id)
      | .[0] // empty
    '
}

selected_finding_json() {
  if [[ -n "${FINDING:-}" ]]; then
    clawpatch show --finding "$FINDING" --json
    return
  fi

  local next_json
  next_json="$(finding_json_from_next)"
  if [[ -n "$next_json" ]] && jq -e 'objects' >/dev/null 2>&1 <<<"$next_json"; then
    local next_id
    next_id="$(json_field "$next_json" '.id // .findingId // .finding.id')"
    if [[ -n "$next_id" ]]; then
      clawpatch show --finding "$next_id" --json
      return
    fi
  fi

  finding_json_from_report
}

finding_id_from_json() {
  json_field "$1" '.id // .findingId // .finding.id'
}

finding_attr() {
  local json="$1"
  local field="$2"
  json_field "$json" ".${field} // .finding.${field}"
}

slugify() {
  tr '[:upper:]' '[:lower:]' \
    | sed -E 's/[^a-z0-9]+/-/g; s/^-+//; s/-+$//' \
    | cut -c1-80
}

run_init() {
  require_cmd clawpatch
  clawpatch doctor
  ensure_clawpatch_initialized
  clawpatch map
}

run_review() {
  require_cmd clawpatch
  ensure_clawpatch_initialized
  clawpatch map
  clawpatch review --limit "$review_limit"
}

run_report() {
  require_cmd clawpatch
  ensure_clawpatch_initialized
  mkdir -p "$(dirname "$report_output")"
  clawpatch report --output "$report_output"
  echo "Wrote ${report_output}"
}

run_show() {
  require_cmd clawpatch
  ensure_clawpatch_initialized

  local finding_json id
  finding_json="$(selected_finding_json)"
  id="$(finding_id_from_json "$finding_json")"
  if [[ -z "$id" ]]; then
    echo "No open Clawpatch findings found." >&2
    exit 1
  fi

  clawpatch show --finding "$id"
}

run_triage() {
  require_cmd clawpatch
  ensure_clawpatch_initialized

  if [[ -z "${FINDING:-}" ]]; then
    echo "error: FINDING is required for clawpatch-triage" >&2
    exit 1
  fi
  if [[ -z "${STATUS:-}" ]]; then
    echo "error: STATUS is required for clawpatch-triage" >&2
    exit 1
  fi

  local args=(triage --finding "$FINDING" --status "$STATUS")
  if [[ -n "${NOTE:-}" ]]; then
    args+=(--note "$NOTE")
  fi

  clawpatch "${args[@]}"
}

run_pr() {
  require_cmd clawpatch
  require_cmd jq
  require_cmd gh
  require_clean_worktree
  require_base_branch
  ensure_clawpatch_initialized

  gh auth status >/dev/null
  git fetch "$remote" "$base_branch"

  local finding_json id severity confidence category title branch slug
  finding_json="$(selected_finding_json)"
  id="$(finding_id_from_json "$finding_json")"
  if [[ -z "$id" ]]; then
    echo "No open Clawpatch findings found." >&2
    exit 1
  fi

  severity="$(finding_attr "$finding_json" severity | tr '[:upper:]' '[:lower:]')"
  confidence="$(finding_attr "$finding_json" confidence | tr '[:upper:]' '[:lower:]')"
  category="$(finding_attr "$finding_json" category)"
  title="$(finding_attr "$finding_json" title)"

  severity="${severity:-unknown}"
  confidence="${confidence:-unknown}"
  category="${category:-unknown}"
  title="${title:-Clawpatch finding ${id}}"
  slug="$(printf '%s' "$id" | slugify)"
  branch="clawpatch/${severity}/${slug}"

  if git show-ref --verify --quiet "refs/heads/${branch}"; then
    echo "error: branch ${branch} already exists" >&2
    exit 1
  fi

  git switch -c "$branch"

  echo "Fixing Clawpatch finding ${id} (${severity}, ${confidence}, ${category})"
  clawpatch show --finding "$id"
  clawpatch fix --finding "$id"
  clawpatch revalidate --finding "$id"
  bash -lc "$validate_cmd"

  if [[ -z "$(git status --porcelain)" ]]; then
    echo "error: clawpatch fix produced no git changes" >&2
    exit 1
  fi

  git add -A
  git commit -m "Fix Clawpatch finding ${id}"
  git push -u "$remote" "$branch"

  local pr_body
  pr_body="$(mktemp)"
  cat >"$pr_body" <<EOF
## Clawpatch finding

- Finding: \`${id}\`
- Severity: \`${severity}\`
- Confidence: \`${confidence}\`
- Category: \`${category}\`
- Title: ${title}

## Validation

- \`clawpatch revalidate --finding ${id}\`
- \`${validate_cmd}\`

## Notes

This PR was created by \`make clawpatch-pr\` for one agreed open finding.
EOF

  gh pr create \
    --base "$base_branch" \
    --head "$branch" \
    --title "Fix Clawpatch ${severity} finding ${id}" \
    --body-file "$pr_body"
}

case "$cmd" in
  init) run_init ;;
  review) run_review ;;
  report) run_report ;;
  show) run_show ;;
  triage) run_triage ;;
  pr) run_pr ;;
  -h|--help|help|"") usage ;;
  *)
    usage >&2
    exit 1
    ;;
esac
