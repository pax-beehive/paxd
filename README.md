# paxd — Pax Fleet Daemon

常驻 agent 机器的守护进程。采集本地 Hermes session 状态、上报 Cloud、拉取并执行 mailbox 消息。

## Architecture

```
paxd (Go binary)
├── cmd/paxd          CLI entry: register, run, install-service
├── internal/
│   ├── config        YAML config loader (~/.pax/paxd.yaml)
│   ├── store         Local SQLite (agent_state, hermes_instances, orphaned_messages)
│   ├── cloud         Fleet Cloud API client (register, status, messages, upgrade)
│   ├── hermes        Local Hermes API Server client (sessions, chat/stream, stop)
│   ├── state         Lifecycle state machine (STARTING→REGISTERING→RUNNING→STOPPING→STOPPED)
│   ├── collector     Session status + system metrics → Cloud
│   ├── poller        Message fetch + dispatch + orphan reconciliation
│   └── executor      chat/steer/command execution against Hermes
```

## State Machine

| State | Behaviour |
|-------|-----------|
| STARTING | Load config, check Hermes |
| REGISTERING | POST /api/agent/register, save credentials |
| RUNNING | Main loop: Status Collector + Message Poller |
| STOPPING | Finish current message (30s timeout), flush offset |
| STOPPED | Exit |

## Main Loop

Two independent tickers:

- **Status Collector (10s)**: GET /api/sessions → POST /api/agent/status
- **Message Poller (5s)**: GET /api/agent/messages → execute → create outbound

Message types:
- `chat` + idle session → POST chat/stream
- `chat` + running session → skip, save as orphaned
- `steer` → stop run → POST chat/stream
- `command` → local processing (upgrade, reconfig, status)

## Quick Start

```bash
# Install
go install github.com/toddzheng/paxd@latest

# Register with Cloud
paxd register --cloud-url https://fleet.example.com

# Run daemon
paxd run

# macOS LaunchAgent
paxd install-service
launchctl load ~/Library/LaunchAgents/com.toddzheng.paxd.plist
```

## Configuration

Default: `~/.pax/paxd.yaml`

```yaml
agent:
  machine_type: mac_mini
  hostname: ""              # auto-detect

cloud:
  api_url: https://fleet.example.com
  api_key: ""               # written by register

hermes:
  api_endpoint: http://localhost:8642
  api_key_from_env: ~/.hermes/.env
  profile: ""

daemon:
  poll_interval: 5s
  status_interval: 10s
  log_level: info
  db_path: ~/.pax/paxd.db
```

## Database

Local SQLite at `~/.pax/paxd.db`:

| Table | Purpose |
|-------|---------|
| `agent_state` | Daemon identity + cloud credentials + message offset |
| `hermes_instances` | Local Hermes API Server endpoints (multi-agent ready) |
| `orphaned_messages` | Chat messages skipped due to steer conflicts |
