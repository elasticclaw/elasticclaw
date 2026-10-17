# ElasticClaw agent image for AWS Lambda MicroVMs. The release artifact built
# by `make build-lambda-microvm-artifact` places both Linux binaries next to
# this Dockerfile before uploading the zip to S3.
FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git gnupg python3 sudo \
    && curl -fsSL https://deb.nodesource.com/setup_24.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g openclaw@2026.7.1-2 --ignore-scripts \
    && useradd -m -s /bin/bash claw \
    && echo 'claw ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers \
    && rm -rf /var/lib/apt/lists/* /root/.npm

COPY claw-bridge /usr/local/bin/claw-bridge
COPY lambda-microvm-bridge /usr/local/bin/lambda-microvm-bridge
RUN chmod 0755 /usr/local/bin/claw-bridge /usr/local/bin/lambda-microvm-bridge \
    && mkdir -p /home/claw/workspace \
    && chown -R claw:claw /home/claw

ENV HOME=/home/claw
ENV ELASTICCLAW_GATEWAY=localhost:18789
ENV OPENCLAW_NO_RESPAWN=1
ENV OPENCLAW_DISABLE_BONJOUR=1

USER claw
WORKDIR /home/claw
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/lambda-microvm-bridge"]
