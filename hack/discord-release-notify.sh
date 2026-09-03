#!/usr/bin/env bash
# Announce a release on Discord via an incoming channel webhook.
#
# Two shapes, selected by DISCORD_MODE:
#
#   changelog — stable releases. Uses the curated notes the release-notes
#               workflow already produced with Groq (What's New / Improvements /
#               Fixes), so the announcement reads like the docs site entry.
#
#   prs       — prereleases. No curated notes exist for an RC, so we fall back to
#               the pull-request list GitHub generates in the release body.
#
# Callers are expected to run this only after the GitHub Release exists, so the
# link in the embed points at published assets rather than a bare tag.
#
# DISCORD_WEBHOOK comes from the DISCORD_RELEASE_WEBHOOK repository secret. Both
# release workflows skip the announcement when that secret is unset, so forks and
# a not-yet-configured repo release normally.
set -euo pipefail

command -v jq >/dev/null || { echo "missing jq"; exit 1; }
command -v curl >/dev/null || { echo "missing curl"; exit 1; }

require() {
  local name="$1"
  if [[ -z "${!name:-}" ]]; then
    echo "discord-release-notify: missing required env ${name}" >&2
    exit 1
  fi
}

require DISCORD_WEBHOOK
require TAG
require DISCORD_MODE

REPO="${GITHUB_REPOSITORY:-elasticclaw/elasticclaw}"
RELEASE_URL="https://github.com/${REPO}/releases/tag/${TAG}"

# Discord rejects the whole payload when any limit is exceeded: 4096 chars per
# description, 1024 per field value, 25 fields, 6000 across the embed. Truncate
# instead of letting a big release silently fail to post.
clamp() {
  local max="$1" text
  text="$(cat)"
  if (( ${#text} <= max )); then
    printf '%s' "$text"
  else
    printf '%s\n…' "${text:0:max-2}"
  fi
}

# JSON array of strings -> markdown bullet list. Anything else yields "".
bullets() {
  printf '%s' "${1:-}" | jq -r 'if type == "array" then map("- " + .) | join("\n") else "" end' 2>/dev/null || true
}

# Emits a field object, or nothing when the section is empty (Discord rejects
# fields with an empty value).
field() {
  local name="$1" value="$2"
  [[ -n "$value" ]] || return 0
  jq -n --arg name "$name" --arg value "$(printf '%s' "$value" | clamp 1024)" \
    '{name: $name, value: $value, inline: false}'
}

case "$DISCORD_MODE" in
  changelog)
    TITLE="${TAG}"
    [[ -n "${NOTES_TITLE:-}" ]] && TITLE="${TAG} — ${NOTES_TITLE}"
    DESCRIPTION="$(printf '%s' "${NOTES_SUMMARY:-}" | clamp 4096)"
    COLOR=2278750  # green
    FIELDS="$(
      {
        field "What's New" "$(bullets "${NOTES_WHATS_NEW:-}")"
        field "Improvements" "$(bullets "${NOTES_IMPROVEMENTS:-}")"
        field "Fixes" "$(bullets "${NOTES_FIXES:-}")"
      } | jq -s '.'
    )"
    ;;
  prs)
    require RELEASE_BODY
    TITLE="${TAG} (prerelease)"
    # GitHub writes "* <pr title> by @<user> in <url>" under "What's Changed".
    # Rewrite each into "- <title> ([#123](<url>))" so the embed stays compact
    # and still links every PR.
    DESCRIPTION="$(
      printf '%s\n' "$RELEASE_BODY" \
        | sed -n 's|^\* \(.*\) by @[^ ]* in https://github.com/[^ ]*/pull/\([0-9]*\)$|- \1 ([#\2](https://github.com/'"${REPO}"'/pull/\2))|p' \
        | clamp 4096
    )"
    [[ -n "$DESCRIPTION" ]] || DESCRIPTION="No pull requests since the previous release."
    COLOR=16098851  # amber
    FIELDS='[]'
    ;;
  *)
    echo "discord-release-notify: unknown DISCORD_MODE '${DISCORD_MODE}' (want changelog|prs)" >&2
    exit 1
    ;;
esac

jq -n \
  --arg title "$TITLE" \
  --arg url "$RELEASE_URL" \
  --arg description "$DESCRIPTION" \
  --argjson color "$COLOR" \
  --argjson fields "$FIELDS" \
  '{
     username: "ElasticClaw",
     embeds: [{
       title: $title,
       url: $url,
       description: $description,
       color: $color,
       fields: $fields,
       footer: {text: "elasticclaw release"}
     }]
   }' > /tmp/discord-payload.json

curl -fsS -X POST \
  -H 'Content-Type: application/json' \
  --data @/tmp/discord-payload.json \
  "$DISCORD_WEBHOOK" >/dev/null

echo "Posted ${TAG} to Discord (mode=${DISCORD_MODE})"
