# stage 1: compile claw-bridge statically (no Go required on the host)
FROM golang:1.25@sha256:699337d620559a59b4a2bb298ad59611e535d2ee755a34cf2d2a98f37578dc80 AS bridge-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags "-X main.Version=dev -X main.Commit=local -X main.BuildDate=0" \
    -o /out/claw-bridge ./cmd/claw-bridge

# stage 2: agent runtime — Ubuntu with Node + OpenClaw pre-installed
FROM ubuntu:22.04@sha256:9abcaffc92a5279fff3a6a41635c0800ea33e528425fb389658806476e7defde
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates git gnupg sudo python3 \
    && rm -rf /var/lib/apt/lists/*

# Install Node.js 24 (same version claw-bridge would install at bootstrap time)
# and fail the build below the CVE-2026-111497 fix. The check is
# cliversion.NodeCompatibleCheck verbatim; a test keeps the two in sync.
RUN set -eu; key=$(mktemp) \
    && curl -fsSL --proto '=https' https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key -o "$key" \
    && key_info=$(gpg --batch --show-keys --with-colons "$key") \
    && printf '%s\n' "$key_info" | awk -F: '$1 == "pub" { primary=1; next } $1 == "sub" { primary=0 } $1 == "fpr" && primary { print; primary=0 }' | grep -Fx 'fpr:::::::::6F71F525282841EEDAF851B42F59B5F99B1BE0B4:' \
    && install -d /etc/apt/keyrings \
    && gpg --batch --yes --dearmor -o /etc/apt/keyrings/nodesource.gpg "$key" \
    && rm -f "$key" \
    && echo 'deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_24.x nodistro main' > /etc/apt/sources.list.d/nodesource.list \
    && apt-get update \
    && apt-get install -y nodejs \
    && rm -rf /var/lib/apt/lists/*
RUN node -e 'const [major, minor, patch] = process.versions.node.split(".").map(Number); process.exit((major === 24 && (minor > 22 || (minor === 22 && patch >= 1))) || major > 26 || (major === 26 && (minor > 12 || (minor === 12 && patch >= 1))) ? 0 : 1)'

# Pre-install OpenClaw from the same embedded lockfile the bridge uses, and
# write the install marker (sha256 of the lock) so bootstrap reuses it. Do not
# use --ignore-scripts: since 2026.9.x the package lifecycle scripts must run.
COPY pkg/cliversion/openclaw/package.json pkg/cliversion/openclaw/package-lock.json /usr/local/lib/openclaw-runtime/
RUN cd /usr/local/lib/openclaw-runtime \
    && npm ci --no-audit --no-fund \
    && sha256sum package-lock.json | cut -d' ' -f1 > .elasticclaw-install-complete \
    && ln -sfn /usr/local/lib/openclaw-runtime/node_modules/.bin/openclaw /usr/local/bin/openclaw

# Create the 'claw' user (bridge runs as non-root; bootstrap expects this user)
RUN useradd -m -s /bin/bash claw \
    && echo 'claw ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers

# Copy the pre-built bridge binary
COPY --from=bridge-builder /out/claw-bridge /usr/local/bin/claw-bridge
RUN chmod +x /usr/local/bin/claw-bridge

ENV ELASTICCLAW_GATEWAY=localhost:18789
ENV OPENCLAW_NO_RESPAWN=1
ENV OPENCLAW_DISABLE_BONJOUR=1

USER claw
WORKDIR /home/claw

ENTRYPOINT ["/usr/local/bin/claw-bridge", "--bootstrap"]
