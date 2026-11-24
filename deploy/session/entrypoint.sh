#!/bin/sh
# Render the per-task OpenCode config without ever writing a model key to disk.
set -eu

: "${PADI_MODEL:=padi/agent-chat}"
: "${PADI_TASK_ID:=unknown}"
: "${PADI_MODEL_BASE_URL:=}"
: "${SESSION_ID:=${PADI_SESSION_ID:-}}"
: "${TASK_ID:=${PADI_TASK_ID}}"
: "${OPENCODE_BASE_CONFIG:=/opt/agent/opencode.base.json}"

CONFIG_DIR="${HOME:-/home/agent}/.config/opencode"
mkdir -p "$CONFIG_DIR"

jq \
  --arg model "$PADI_MODEL" \
  --arg task "$TASK_ID" \
  --arg session "$SESSION_ID" \
  --arg base "$PADI_MODEL_BASE_URL" \
  '
    ($model | split("/")[0]) as $provider
    | .model = $model
    | .providers[$provider] = ((.providers[$provider] // {})
        | .headers = ((.headers // {}) + {
            "X-Task-Id": $task,
            "X-Session-Id": $session
          }))
    | if $base != "" then
        .providers[$provider].settings = ((.providers[$provider].settings // {}) + {
          "baseURL": $base
        })
      else . end
  ' "$OPENCODE_BASE_CONFIG" > "$CONFIG_DIR/opencode.json"

chmod 600 "$CONFIG_DIR/opencode.json"

# The router token stays in this process environment; OpenCode reads it from there.
exec opencode serve --hostname 0.0.0.0 --port 4096
