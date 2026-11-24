# M3a session image. Built for TARGETARCH (amd64 locally, arm64 on the VM).
FROM golang:1.26-alpine AS verifybuild
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/agent-verify ./cmd/agent-verify
COPY internal/verify ./internal/verify
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -o /agent-verify ./cmd/agent-verify

FROM ubuntu:24.04

ARG OPENCODE_VERSION=2.4.0
ARG TARGETARCH
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git jq ripgrep build-essential golang-go \
      python3 nodejs npm \
    && rm -rf /var/lib/apt/lists/*

RUN useradd --create-home --shell /bin/bash agent \
    && mkdir -p /opt/agent /home/agent/.config/opencode /workspace \
    && chown -R agent:agent /home/agent /opt/agent /workspace

# Install a pinned OpenCode v2 binary as the unprivileged agent user.
RUN curl -fsSL https://opencode.ai/v2/install -o /tmp/opencode-install.sh \
    && chmod 755 /tmp/opencode-install.sh \
    && HOME=/home/agent bash /tmp/opencode-install.sh --version "${OPENCODE_VERSION}" --no-modify-path \
    && rm -f /tmp/opencode-install.sh

ENV PATH="/home/agent/.opencode/bin:${PATH}"

COPY deploy/session/opencode.base.json /opt/agent/opencode.base.json
COPY deploy/session/AGENTS.target.md /opt/agent/AGENTS.target.md
COPY deploy/session/entrypoint.sh /entrypoint.sh
COPY deploy/session/padi-export-bundle /usr/local/bin/padi-export-bundle
COPY --from=verifybuild /agent-verify /usr/local/bin/agent-verify
RUN chmod 755 /entrypoint.sh /usr/local/bin/padi-export-bundle /usr/local/bin/agent-verify \
    && chown -R agent:agent /opt/agent /entrypoint.sh

USER agent
WORKDIR /workspace

# Only ROUTER_TOKEN is supplied at runtime; provider keys stay with the router.
ENTRYPOINT ["/entrypoint.sh"]
