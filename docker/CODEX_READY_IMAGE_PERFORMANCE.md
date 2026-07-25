# Codex-Ready Agent Image: Local Performance Comparison

## Executive summary

ElasticClaw creates a fresh, isolated container for every task. The Codex-ready
image preserves that isolation while moving shared tool installation from
per-task bootstrap into a versioned Docker image.

In our local test, this reduced startup overhead for a small Codex task from an
estimated **5–11+ minutes** to approximately **30–60 seconds**.

> [!IMPORTANT]
> These are local observations and planning estimates, not a controlled
> production benchmark. Network speed, Docker cache state, host performance,
> registry location, and model latency will affect results.

## Approximate comparison

| Phase | Generic image | Codex-ready image |
|---|---:|---:|
| First image pull | 1–4 min | 2–5 min because the image is approximately 1.57 GB |
| Tool installation | 5–10+ min per task | None per task |
| Container bootstrap | Included in the installation time above | 25–40 sec |
| Simple Codex response | 4–20 sec | 4–20 sec |
| **Total for a tiny task** | **5–11+ min** | **30–60 sec** |
| Reliability | Can hit npm or plugin watchdog timeouts | Predictable after the image is available |

## Observed ready-image run

The following measurements came from a local Docker/WSL test using:

- Image: `elasticclaw/openclaw-codex-ready:2026.7.1-2`
- Model: `codex/gpt-5.5`
- Image size: approximately 1.57 GB

| Event | Approximate time |
|---|---:|
| Container and gateway ready | 25 sec |
| Initial Codex introduction | 16 sec |
| `CODEX_IMAGE_OK` response | 4 sec |
| **First complete interaction** | **41 sec** |
| Subsequent simple prompts | 4–15 sec |

The image test successfully returned:

```text
CODEX_IMAGE_OK
```

No authentication-refresh or plugin-install timeout occurred.

## Why the ready image is faster

### Generic-image path

Each task starts a fresh container. Docker reuses the cached base-image layers,
but missing or mismatched tools are installed inside the task container:

1. Start a container from the generic OpenClaw image.
2. Install or update the pinned OpenClaw CLI when required.
3. Install the Codex CLI.
4. Download and install the Codex OpenClaw plugin.
5. Configure per-agent identity and authentication.
6. Start the gateway and connect to the ElasticClaw Hub.

Runtime installations are part of the disposable container. They are lost when
that container is destroyed, so the expensive installation can repeat for the
next task.

### Codex-ready path

The shared image already contains:

- The pinned OpenClaw CLI
- The pinned Codex CLI
- The Codex OpenClaw plugin
- The ElasticClaw bridge

Each task still receives a clean container. Only task-specific state is added at
runtime:

- Agent identity
- Short-lived credentials
- Gateway password and configuration
- Scoped repository or workspace files, when requested

No credentials, repositories, or task workspaces are baked into the image.

## One-time team setup

With a shared ready image:

| Setup activity | Planning estimate |
|---|---:|
| CI build and publish | 8–15 min per image release |
| First developer or server pull | 2–5 min |
| Later tasks on the same machine | No additional pull or tool installation |

The image should be built once in CI and published to a shared registry. Team
members and ElasticClaw Hubs pull the same immutable version; individual
developers should not rebuild it for every task.

## Example: ten short tasks

### Generic image

```text
10 tasks × 5–10 minutes of setup
= 50–100 minutes of setup overhead
```

### Codex-ready image

```text
Initial image pull:       2–5 minutes
10 tasks × ~30 seconds:   ~5 minutes
Total setup overhead:     ~7–10 minutes
```

This represents an estimated **40–90 minute reduction in setup overhead** across
ten short tasks. Actual coding and model execution time is additional in both
cases.

## Operational trade-offs

### Benefits

- Faster and more predictable agent startup
- Fewer npm and external-network dependencies during task provisioning
- Version consistency across developers, CI, and agent environments
- Preserved per-task container isolation
- Runtime fallback remains available for older or generic images

### Costs and limitations

- The first pull is larger than the generic image.
- A new image must be published when pinned tool versions change.
- Registry availability becomes part of the deployment path.
- The current bridge build targets `linux/amd64`; a team rollout should also
  publish `linux/arm64`.

## Recommended rollout

1. Build the ready image in CI.
2. Publish versioned AMD64 and ARM64 images to a shared registry.
3. Pin the image by release tag or digest in the Docker provider configuration.
4. Keep credentials and repository data runtime-only.
5. Retain the generic runtime-install path as a compatibility fallback.
6. Track cold-pull, warm-start, gateway-ready, and first-response timings in CI.

## Bottom line

The ready image does not remove task isolation or reuse an old agent. It creates
the same fresh agent container while avoiding repeated installation of the
shared Codex toolchain.

The practical result is a reduction from several minutes of environment setup
per task to roughly half a minute of agent bootstrap after the image is present.
