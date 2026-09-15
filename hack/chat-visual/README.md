# Chat visual-verification harness

A throwaway, reusable local rig for screenshotting the agent chat UI against a
seeded hub, so a UI change can be compared before/after instead of described.

Three pieces:

| File | What it does |
| --- | --- |
| `pkg/hub/zz_chat_visual_test.go` | Boots a real hub on `HUB_PORT`, seeds 3 claws (one with a long, kind-complete conversation), opens fake bridges so they show as `connected`, then blocks forever. Gated on `MANUAL_VERIFY`. |
| `run-hub.sh` | Starts/stops that harness via `nohup` + pidfile and waits for the port. |
| `screenshot.mjs` | Zero-dependency CDP driver: headless Chrome, sets the hub token, selects the rich claw, expands the turn cards, captures desktop light/dark, mobile, and a full-conversation PNG. |

## Run it

```bash
# 1. hub (port must match NEXT_PUBLIC_HUB_URL in web/.env.local — default 8085)
hack/chat-visual/run-hub.sh
# log: /tmp/ec-chat-visual-hub.log   pid: hack/chat-visual/hub.pid

# 2. web dev server (localhost, never 127.0.0.1)
nohup npm --prefix web run dev > /tmp/ec-chat-visual-next.log 2>&1 &

# 3. screenshots
node hack/chat-visual/screenshot.mjs --label before
#   → hack/chat-visual/out/before-desktop-dark.png
#     hack/chat-visual/out/before-desktop-light.png
#     hack/chat-visual/out/before-mobile-dark.png
#     hack/chat-visual/out/before-desktop-conversation-full.png

# 4. make the UI change, then:
node hack/chat-visual/screenshot.mjs --label after

# 5. stop
hack/chat-visual/run-hub.sh stop
lsof -ti tcp:3000 | xargs kill
```

`web/.env.local` is gitignored and must contain:

```
NEXT_PUBLIC_HUB_URL=http://localhost:8085
```

That single value both points the browser at the hub and sets the Next dev
`/api/*` rewrite destination.

## screenshot.mjs flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--label` | `shot` | Filename prefix, for before/after pairs. |
| `--app` | `http://localhost:3000` | Next dev server. |
| `--hub` | `http://localhost:8085` | Hub, used to resolve the claw id. |
| `--token` | `test-token` | Written to `sessionStorage.ec_hub_token`. |
| `--claw` | `token-refresh-race` | Substring of the claw name to open. |
| `--keep-chrome` | off | Leave headless Chrome running on :9222 for manual poking. |

## Seeded data

`fix/token-refresh-race` (connected, the rich one) is a three-turn bug-fix PR
conversation over `installationToken`:

- **user messages** — a short one and a multi-paragraph one carrying an
  `[Attachments]` footer (renders as an attachment chip)
- **assistant markdown** — `##`/`###`, paragraphs, bullet + numbered lists,
  inline code, fenced `go` / `ts` / `bash` / `diff` blocks, a table, a
  blockquote, an external link
- **tool steps** — `Read` ×4 (collapses to "Read 4 files"), `Grep`, a **failed**
  `Bash` with `exit 1` and a race-detector error body, `Edit` with a diff
  result, `Write`, `WebFetch` with a URL, a `Task` subagent spawn with
  `subagent_name/type/model/prompt`, and a `Bash` whose output is long enough to
  hit the scroll cap
- **info activities** — `model_started`, `diagnostic`, `session_error` (amber)
- **chrome rows** — `[hub] ▶ Stage: …`, a watchdog nag, a `system` divider, a
  `state` transition, and `activity_summary` "Show N earlier tool calls" blocks
- **in-progress turn** — a trailing `Task` and `Bash` with a start event and no
  terminal, so the last turn renders as `running` with a live clock, the Now
  strip is populated, and the subagent rail shows 1 running / 1 finished

`chore/bump-node-deps` (connected, short/idle) and
`feature/webhook-retry-backoff` (error, with a bridge-error hub notice) cover
the sidebar's other states.

## Gotchas baked in

- Claws only report `connected` while a live WS session exists — the harness
  holds fake bridges on `/claw/ws` (`{type:"register", payload:{claw_id, name,
  token, gateway_ready:true}}`) for exactly that reason.
- The timeline endpoint returns `activity_summary` placeholders, and the web
  only auto-expands the **trailing two** summaries. The seed therefore packs the
  interesting tool calls into the last two activity gaps.
- `useSyncExternalStore` reads `localStorage` during the first client render, so
  the script writes `elasticclaw_selected_claw` on a first visit and then
  navigates again; the sidebar click is only a fallback.
- The app hardcodes `class="dark"` on `<html>` (there is no mounted
  `ThemeProvider`), so light mode is a class toggle, not a next-themes call.
- `body` is `overflow-hidden` and the transcript is its own scroller, so the
  "full page" capture grows the viewport until that scroller stops overflowing.

## Not covered

- **True streaming text.** The typewriter buffer comes from live WS `stream`
  frames, not from the DB, so an in-flight assistant message cannot be seeded.
  The in-progress turn is represented by running tool steps instead.
- **"You" vs "teammate" bubbles.** `/api/auth/me` returns no GitHub login for a
  shared hub token, so every user message renders in the teammate style.
