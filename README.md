# ElasticClaw

Open source platform for provisioning and managing AI agent VMs (claws).

ElasticClaw runs a **hub** — a single Go binary with an embedded web UI — that connects to cloud VM providers and manages the lifecycle of AI agents. Each agent (claw) runs [OpenClaw](https://github.com/openclaw/openclaw) in an ephemeral VM, with scoped GitHub access, LLM keys, and persistent memory.

## Features

- **Single binary** — hub + embedded web UI, no Docker required
- **One-command install** — `elasticclaw install` sets up a full hub on any Ubuntu VPS
- **Pluggable providers** — Replicated CMX, Daytona (more coming)
- **Factory automation** — Linear integration to auto-create claws from issue status changes
- **Template registry** — community templates at [elasticclaw/elasticclaw-templates](https://github.com/elasticclaw/elasticclaw-templates)
- **Profile management** — connect to multiple hubs

## Documentation

**→ [elasticclaw.ai/docs](https://elasticclaw.ai/docs)**

- [Installation](https://elasticclaw.ai/docs/installation)
- [Quick start](https://elasticclaw.ai/docs/quick-start)
- [Hub configuration](https://elasticclaw.ai/docs/hub)
- [GitHub integration](https://elasticclaw.ai/docs/github-integration)

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup and workflow.

## License

[Apache 2.0](LICENSE)
