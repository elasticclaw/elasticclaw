# stage 1: compile claw-bridge statically (no Go required on the host)
FROM golang:1.25 AS bridge-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags "-X main.Version=dev -X main.Commit=local -X main.BuildDate=0" \
    -o /out/claw-bridge ./cmd/claw-bridge

# stage 2: agent runtime — Ubuntu with Node + OpenClaw pre-installed
FROM ubuntu:22.04
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates git gnupg sudo python3 \
    && rm -rf /var/lib/apt/lists/*

# Install Node.js 24 (same version claw-bridge would install at bootstrap time)
RUN curl -fsSL https://deb.nodesource.com/setup_24.x | bash - \
    && apt-get install -y nodejs \
    && rm -rf /var/lib/apt/lists/*

# Pre-install OpenClaw globally so bootstrap skips the npm install step.
RUN npm install -g openclaw@2026.6.1 --ignore-scripts

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
