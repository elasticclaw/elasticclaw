# elasticclaw-web

Web UI for [ElasticClaw](https://github.com/elasticclaw/elasticclaw) — a real-time dashboard for managing and chatting with AI agent claws.

## What it does

- **Board view** — all running claws at a glance, each as a card with live status, streaming indicator, and inline chat
- **Conversation view** — full chat interface with markdown rendering, streaming typewriter effect, and context window usage
- **Spawn + kill** — provision new claws from templates, kill them when done
- **Real-time** — WebSocket connection to the hub; new messages, streaming chunks, and status changes appear instantly

## Running locally

### From source

```bash
pnpm install
NEXT_PUBLIC_HUB_URL=http://your-hub:port \
NEXT_PUBLIC_HUB_TOKEN=your-token \
pnpm dev
```

### From Docker

```bash
docker run -p 3000:3000 \
  -e NEXT_PUBLIC_HUB_URL=http://your-hub:port \
  -e NEXT_PUBLIC_HUB_TOKEN=your-token \
  ghcr.io/elasticclaw/elasticclaw-web:latest
```

### Without env vars

Leave `NEXT_PUBLIC_HUB_URL` unset and the app shows a setup screen on first load. Enter your hub URL and token there — they're saved to localStorage.

## Configuration

| Variable | Description |
|---|---|
| `NEXT_PUBLIC_HUB_URL` | ElasticClaw hub URL, e.g. `http://localhost:8080` |
| `NEXT_PUBLIC_HUB_TOKEN` | Operator token from your hub config |

## Releases

Docker images are published to `ghcr.io/elasticclaw/elasticclaw-web` on every semver tag. Stable releases also get a `latest` tag. Pre-release tags (e.g. `v0.1.0-beta.1`) are marked as pre-release and don't move `latest`.

## Stack

- Next.js 16, React 19, TypeScript
- Tailwind CSS v4 + shadcn/ui
- WebSocket for real-time hub events
- `react-markdown` + `remark-gfm` for message rendering
