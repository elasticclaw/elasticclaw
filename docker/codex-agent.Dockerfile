ARG OPENCLAW_IMAGE_VERSION=2026.7.1

FROM golang:1.25 AS bridge-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags "-X main.Version=dev -X main.Commit=local -X main.BuildDate=0" \
    -o /out/claw-bridge ./cmd/claw-bridge

FROM ghcr.io/openclaw/openclaw:${OPENCLAW_IMAGE_VERSION}

ARG OPENCLAW_VERSION=2026.7.1-2
ARG CODEX_CLI_VERSION=0.144.6
ARG CODEX_PLUGIN_VERSION=2026.7.1-1
# The plugin currently depends on this Codex CLI release in its shrinkwrap.
ARG CODEX_PLUGIN_CLI_VERSION=0.144.3

USER root

RUN apt-get update \
    && apt-get install -y --no-install-recommends sudo \
    && printf 'node ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/elasticclaw-node \
    && chmod 0440 /etc/sudoers.d/elasticclaw-node \
    && rm -rf /var/lib/apt/lists/*

USER node
ENV HOME=/home/node
ENV npm_config_prefix=/home/node/.local
ENV PATH=/home/node/.local/bin:${PATH}

# Install the exact runtime versions expected by claw-bridge in the same
# user-local prefix used by runtime bootstrap. This avoids overwriting the
# base image's /usr/local/bin/openclaw binary.
RUN npm install -g \
      "openclaw@${OPENCLAW_VERSION}" \
      "@openai/codex@${CODEX_CLI_VERSION}" \
      --ignore-scripts --no-audit --no-fund

# The Codex plugin shrinkwrap names every supported platform package. npm
# downloads all of those archives before materializing only the current one,
# which can exceed OpenClaw's five-minute plugin-install watchdog on a cold
# connection. Seed the archives outside that watchdog, install once into the
# image, verify discovery, and then discard the download cache.
RUN set -eux; \
    for platform in \
      linux-x64 linux-arm64 \
      darwin-x64 darwin-arm64 \
      win32-x64 win32-arm64; do \
        npm cache add "@openai/codex@${CODEX_PLUGIN_CLI_VERSION}-${platform}"; \
    done; \
    openclaw plugins install "npm:@openclaw/codex@${CODEX_PLUGIN_VERSION}"; \
    openclaw plugins info codex --json >/dev/null; \
    npm cache clean --force

# Keep the managed plugin project, but remove image-build configuration and
# migration state. Each agent must run onboarding with its own identity,
# gateway password, workspace, and model credential.
RUN rm -rf \
      /home/node/.openclaw/openclaw.json \
      /home/node/.openclaw/logs \
      /home/node/.openclaw/state \
      /home/node/.openclaw/workspace

COPY --from=bridge-builder /out/claw-bridge /usr/local/bin/claw-bridge

# claw-bridge performs per-agent onboarding after the Hub copies the scoped
# workspace and refreshed model credential into the container.
ENTRYPOINT ["sh", "-lc"]
CMD ["trap 'exit 0' TERM INT; while :; do sleep 3600; done"]
