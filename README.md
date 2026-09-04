# ElasticClaw

[![Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Docs](https://img.shields.io/badge/docs-elasticclaw.ai-cyan.svg)](https://elasticclaw.ai/docs)

**Open source workflow automation for coding agents.**

ElasticClaw turns issue tracker events into governed coding workflows: start the right agent, provision an isolated workspace, inject the issue context, mint scoped GitHub credentials, open the PR, watch review and CI, and clean everything up when the work is done.

Remote coding agents give you a shell. ElasticClaw gives you the workflow around it.

<p align="center">
  <a href="https://www.youtube.com/watch?v=2h_-3HsV9Bo">
    <img src="https://img.youtube.com/vi/2h_-3HsV9Bo/maxresdefault.jpg" alt="ElasticClaw walkthrough video" width="720">
  </a>
</p>

## Why ElasticClaw exists

AI coding agents are getting good enough to do real work, but real work does not begin with a prompt and end with a terminal transcript. It starts in Linear, GitHub Issues, Shortcut, webhooks, releases, customer escalations, dependency alerts, and private operational queues. It needs credentials with boundaries. It needs branch and PR policy. It needs logs, lifecycle state, review handling, CI awareness, and cleanup.

ElasticClaw is the control plane for that loop.

Instead of manually launching agents one at a time, you define **workspaces** and **workflows**: repeatable workstreams that know when to start, what access to grant, which model and tools are allowed, how to drive the PR, and when to tear the sandbox down.

## What it does

- **Workspaces** define the runtime: repos, bootstrap files, instructions, environment, MCP servers, model defaults, and provider settings.
- **Workflows** define the automation: trigger rules, stages, issue movement, lifecycle policy, and cleanup.
- **Scoped GitHub App credentials** give each agent temporary repo access instead of broad personal tokens.
- **Issue tracker integrations** turn Linear, GitHub Issues, Shortcut, and webhook events into structured work.
- **Sandbox providers** run each agent in an isolated workspace using Daytona, Replicated CMX, or exe.dev.
- **Dual-driver browser evidence** verifies UI flows with either the Playwright-backed OpenClaw browser or Browser Use and carries safe screenshots, E2E videos, and traces into the pull request.
- **Single-binary ElasticClaw Server** gives you the API, web UI, state, settings, and workflow automation in one self-hosted Go service.

Each running agent runs [OpenClaw](https://github.com/openclaw/openclaw), connects back to ElasticClaw Server through the connector, clones the allowed repos, receives the issue context, and works inside an ephemeral VM.

For browser-visible changes, ElasticClaw prepares both OpenClaw's Playwright-backed Chromium runtime and the Browser Use CLI. The injected workflow requires a readiness check before page control, named sessions, branch/run-scoped manifests, screenshots plus video only when behavior warrants it, console and page-error checks, cleanup, and truthful submission status. Agents may compare the two drivers while keeping their runs separate, then commit reviewer-safe evidence under `.github/pr-evidence/` so image and video or trace links also work in private repositories. If a selected driver or route cannot run, the PR records that blocker instead of claiming verification.

For Jira-triggered investigations, attachment contents stay out of the task prompt. Sandboxed agents use their scoped Jira credentials to discover and download only the screenshots, recordings, logs, or HAR exports needed for the task, while treating those files as untrusted and potentially sensitive.

## The Workflow Loop

```mermaid
flowchart LR
    A[Issue or event] --> B{Factory filters}
    B -->|match| C[Workspace]
    C --> D[Sandbox]
    D --> E[Agent works]
    E --> F[Pull request]
    F --> G[CI and review]
    G --> H[Merge or close]
    H --> I[Cleanup]
    B -->|no match| J[Ignore]
```

ElasticClaw is designed for the work that should happen again and again:

- Bug lanes from Linear statuses
- Dependency update workflows
- Docs and migration queues
- Release follow-up tasks
- Customer escalation workflows
- Background work that should produce PRs, not meetings

## Quick start

Install the CLI:

```bash
brew tap elasticclaw/elasticclaw
brew install elasticclaw
```

Prerelease builds publish a separate formula so stable installs stay on
non-prerelease tags only:

```bash
brew install elasticclaw/elasticclaw/elasticclaw-beta
```

Deploy ElasticClaw Server to an Ubuntu VPS:

```bash
elasticclaw install \
  --server ssh://root@my-server.com \
  --domain server.mycompany.com \
  --ssh-key ~/.ssh/id_ed25519
```

Then configure the pieces your first workflow needs:

1. A sandbox provider such as Daytona, Replicated CMX, or exe.dev.
2. A GitHub App so ElasticClaw can mint scoped installation tokens.
3. A workspace that defines repos, instructions, tools, and model defaults.
4. An issue source such as Linear, GitHub Issues, Shortcut, or a webhook.
5. A workflow that ties the trigger, workspace access, and lifecycle together.

Watch the installation walkthrough: [YouTube quick start](https://www.youtube.com/watch?v=1joBaUrtwOA).

Full setup guide: [elasticclaw.ai/docs/installation](https://elasticclaw.ai/docs/installation)

### Linear workflow triggers

`trigger.linear` supports `event`, `workspace`, `team`, `projects`, `states`, `labels`, `exclude_labels`, `assigned_to`, and `agent_status_error`. When `projects` is omitted or empty, issues in any Linear project—including issues with no project—can match. Otherwise, an issue must belong to one of the listed project IDs or names.

```yaml
trigger:
  linear:
    event: status_changed
    team: ADV
    projects:
      - Adversary Labs
    states:
      - Todo
```

See the [Linear issue workflow example](examples/workflows/linear-issue.yaml).

For deterministic plan approval (schema gate instead of freeform chat), see
[linear-issue-plan-gate.yaml](examples/workflows/linear-issue-plan-gate.yaml)
and the docs on [plan approval](https://elasticclaw.ai/docs/workflows#plan-approval).

## Three ways to start work

**From an issue tracker**

Move a ticket to a trigger status, apply a label, assign it, or let your normal process send the event. The workflow filters the event and starts an agent with the issue body, links, labels, repos, and instructions.

**From the web UI**

Create and inspect agents, manage workspaces and workflows, configure providers, review logs, and connect issue trackers from the embedded dashboard.

**From your own automation**

Use webhooks and the ElasticClaw Server API to connect private queues, release events, internal systems, or scheduled work.

## Architecture

ElasticClaw has four main moving parts:

- **ElasticClaw Server**: the self-hosted control point for settings, secrets, workspaces, workflows, lifecycle state, and the web UI.
- **Provider**: the execution backend that creates isolated workspaces.
- **Bridge**: the connector running inside the workspace that links OpenClaw to ElasticClaw Server.
- **Agent**: the sandboxed coding session doing the actual work.

ElasticClaw Server owns policy. Providers own compute. OpenClaw owns the coding session. ElasticClaw connects them into a repeatable issue-to-PR workflow.

## Documentation

- [Overview](https://elasticclaw.ai/docs)
- [Installation](https://elasticclaw.ai/docs/installation)
- [Workspaces](https://elasticclaw.ai/docs/workspaces)
- [Workflows](https://elasticclaw.ai/docs/workflows)
- [Plan approval (`plan_gate`)](https://elasticclaw.ai/docs/workflows#plan-approval)
- [Providers](https://elasticclaw.ai/docs/providers)
- [GitHub integration](https://elasticclaw.ai/docs/github-integration)
- [Linear integration](https://elasticclaw.ai/docs/linear-integration)
- [CLI reference](https://elasticclaw.ai/docs/cli-reference)

## Project status

ElasticClaw is early, open source, and moving quickly. The core loop is usable today: deploy ElasticClaw Server, connect a provider, configure GitHub and an issue tracker, and run agents from real work queues.

The vision is broader: software workflows that are owned by teams, wired into existing systems, and governed by explicit stages instead of ad hoc prompts.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup and workflow.

If ElasticClaw looks useful, stars and public feedback help other developers find the project. Try it, break it, post what was confusing, and show what you built.

## License

[Apache 2.0](LICENSE)
