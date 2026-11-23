# M1 session image. Built for TARGETARCH (amd64 locally, arm64 on the VM).
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

COPY opencode.base.json /opt/agent/opencode.base.json
COPY AGENTS.target.md /opt/agent/AGENTS.target.md
COPY entrypoint.sh /entrypoint.sh
RUN chmod 755 /entrypoint.sh && chown -R agent:agent /opt/agent /entrypoint.sh

USER agent
WORKDIR /workspace

# The throwaway model key is supplied only as a runtime env var by the gateway.
ENTRYPOINT ["/entrypoint.sh"]
