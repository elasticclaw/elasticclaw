# elasticclaw-web

Web UI for [ElasticClaw](https://github.com/elasticclaw/elasticclaw) — a real-time dashboard for managing and chatting with sandboxed agents.

## What it does

- **Board view** — all running agents at a glance, each as a card with live status, streaming indicator, and inline chat
- **Conversation view** — full chat interface with markdown rendering, streaming typewriter effect, and context window usage
- **Spawn + kill** — provision new agents from workspaces and workflows, kill them when done
- **Real-time** — WebSocket connection to ElasticClaw Server; new messages, streaming chunks, and status changes appear instantly

## Running locally

### From source

```bash
cd web
cp .env.example .env.local
# Edit .env.local with your ElasticClaw Server URL, token, and a UI password
npm install
npm run dev
```

Then open http://localhost:3000 and enter the value of `ELASTICCLAW_UI_TOKEN` as the password.

### Environment variables

| Variable | Description |
|---|---|
| `ELASTICCLAW_UI_TOKEN` | Password for the web UI login page |
| `ELASTICCLAW_HUB_URL` | URL of your running ElasticClaw Server |
| `ELASTICCLAW_HUB_TOKEN` | Your server user token (from `~/.elasticclaw/config.yaml`) |

### From Docker

```bash
docker run -p 3000:3000 \
  -e ELASTICCLAW_UI_TOKEN=your-password \
  -e ELASTICCLAW_HUB_URL=http://your-server:18788 \
  -e ELASTICCLAW_HUB_TOKEN=your-token \
  marc/elasticclaw-web:latest
```

### Without env vars

Leave `NEXT_PUBLIC_HUB_URL` unset and the app shows a setup screen on first load. Enter your ElasticClaw Server URL and token there. They are saved to localStorage.

## Configuration

| Variable | Description |
|---|---|
| `NEXT_PUBLIC_HUB_URL` | ElasticClaw Server URL, e.g. `http://localhost:8080` |
| `NEXT_PUBLIC_HUB_TOKEN` | Operator token from your server config |

## Releases

Docker images are published to `ghcr.io/elasticclaw/elasticclaw-web` on every CalVer tag (`YYYY.M.D`). Stable releases also get a `latest` tag. Prerelease tags (e.g. `2026.7.7-rc1`) are marked as pre-release and don't move `latest`.

## Stack

- Next.js 16, React 19, TypeScript
- Tailwind CSS v4 + shadcn/ui
- WebSocket for real-time server events
- `react-markdown` + `remark-gfm` for message rendering
