# aw-manager

Chat gateway for [aw](https://github.com/konono/aw) — run AI coding agents on Kubernetes from Slack or Discord.

```
Discord/Slack  →  aw-manager  →  K8s Pod (claude/codex/opencode/cursor)
```

Users send messages in chat. aw-manager creates an agent Pod per user+channel, executes the AI tool, and replies with the result.

## Quick Start

### Prerequisites

- Go 1.25+
- Kubernetes cluster (OpenShift supported)
- Redis
- Discord or Slack bot token
- [aw](https://github.com/konono/aw) binary (for `aw manifest`)
- `.aw.yml` with a K8s profile (e.g., `k8s-claude`)

### Local Development

```bash
# 1. Copy and edit config
cp .env.example .env
vi .env

# 2. Start Redis
podman run -d --name aw-redis -p 6379:6379 redis:7-alpine

# 3. Run
./aw-manager serve
```

### Deploy to Kubernetes

```bash
# Build and push image
./aw-manager build --push --registry ghcr.io/yourorg

# Deploy (secrets are auto-extracted from .aw.yml)
./aw-manager deploy \
  --adapter discord \
  --discord-token "your-token" \
  --image ghcr.io/yourorg/aw-manager:0.1.0 \
  --aw-config .aw.yml
```

This creates everything in one command: namespaces, RBAC, Redis, secrets, and the aw-manager Deployment.

## Configuration

All settings can be provided via CLI flags, environment variables, or a `.env` file. Run `aw-manager serve --help` for the full list.

| Setting | Env Var | Default | Description |
|---|---|---|---|
| `--adapter` | `CHAT_ADAPTER` | `slack` | Chat platform (`slack` or `discord`) |
| `--slack-bot-token` | `SLACK_BOT_TOKEN` | | Slack bot token |
| `--slack-app-token` | `SLACK_APP_TOKEN` | | Slack app token (Socket Mode) |
| `--discord-token` | `DISCORD_TOKEN` | | Discord bot token |
| `--redis-url` | `REDIS_URL` | `redis://localhost:6379` | Redis connection URL |
| `--aw-profile` | `AW_PROFILE` | `claude-k8s` | aw profile for agent pods |
| `--aw-namespace` | `AW_NAMESPACE` | `aw` | Namespace for agent pods |
| `--aw-binary` | `AW_BINARY` | `aw` | Path to aw binary |
| `--aw-tool` | `AW_TOOL` | `claude` | AI tool (`claude`, `codex`, `opencode`, `cursor`) |
| `--idle-timeout` | `IDLE_TIMEOUT` | `1h` | Idle timeout before agent pods are cleaned up |
| `--max-concurrent` | `MAX_CONCURRENT` | `10` | Maximum concurrent message handlers |
| `--metrics-addr` | `METRICS_ADDR` | `:9090` | Prometheus metrics endpoint |

## Architecture

### Session Model

Each **user + channel** pair gets a dedicated Pod. Messages in the same channel reuse the same Pod and continue the AI session with `--continue`. Different channels get separate Pods with isolated sessions.

### How It Works

1. Chat message received via Socket Mode (Slack) or WebSocket (Discord)
2. `aw manifest <profile> --name <hash>` generates K8s manifests (Deployment, ConfigMap, Secret, etc.)
3. Manifests applied via Server-Side Apply (client-go dynamic client)
4. Wait for Pod Ready
5. Execute AI tool via `kubectl exec` with message piped through stdin
6. Response sent back to chat

### Pod Lifecycle

- **Creation**: On first message per user+channel
- **Reuse**: Subsequent messages in the same channel reuse the existing Pod
- **Idle cleanup**: Pods idle longer than `--idle-timeout` are automatically deleted (5-minute check interval)
- **Orphan cleanup**: Pods without a Redis session (e.g., after Redis restart) are cleaned up with safety guards
- **Unhealthy recovery**: CrashLoopBackOff / ImagePullBackOff pods are deleted and recreated on next message

### Concurrency

- Per-session mutex serializes EnsurePod + ExecTool for the same user+channel
- Global semaphore limits concurrent handlers (default: 10)
- Excess messages get an immediate "server busy" response

## Supported Tools

| Tool | Binary | Session Continue |
|---|---|---|
| Claude Code | `claude` | `--continue` |
| Cursor | `agent` | `--continue` |
| Codex | `codex` | Not supported |
| OpenCode | `opencode` | Not supported |

## Observability

### Prometheus Metrics

| Metric | Type | Description |
|---|---|---|
| `aw_agent_pods_active` | Gauge | Currently active agent pods (synced from K8s) |
| `aw_agent_exec_duration_seconds` | Histogram | Exec operation duration |
| `aw_agent_exec_total` | Counter | Exec operations by status (`success`/`error`) |
| `aw_agent_pod_create_duration_seconds` | Histogram | Pod creation to Ready duration |
| `aw_agent_messages_total` | Counter | Received messages by adapter |
| `aw_agent_messages_rejected_total` | Counter | Messages rejected due to concurrency limit |

### Health Check

`GET /healthz` — Returns `200 ok` if Redis is reachable, `503 unavailable` otherwise.

## Production Notes

- **Single replica only** — aw-manager must run as `replicas: 1`. Socket Mode delivers events to all instances, causing duplicate processing.
- **External Redis recommended** — The built-in Redis has no persistence. Use `--redis-url` to point to a managed Redis for production.
- **Network policy** — `/metrics` and `/healthz` are unauthenticated. Restrict access with a NetworkPolicy.
- **Access control** — Any user who can mention the bot can create agent Pods. Use Slack/Discord channel permissions to control access.

## Commands

```
aw-manager serve    Start the server (default)
aw-manager deploy   Deploy to Kubernetes
aw-manager build    Build the container image locally
```

## License

MIT
