# paxd — Pax Fleet Daemon

> Agent 时代的 kubelet。跑在每台 Agent 机器上，连接 Pax Cloud，接收消息，驱动本地 Hermes 执行。

## 什么是 paxd

paxd 是 Pax 平台的 agent 侧守护进程。它常驻在运行 Hermes（或任意兼容 Agent）的机器上，负责：

1. **注册到 Pax Cloud** — 上报机器身份、能力、状态
2. **接收消息** — WebSocket 实时接收或 HTTP 轮询 Cloud mailbox
3. **驱动执行** — 将消息转发给本地 Hermes，实时 streaming 事件回传
4. **会话连续性** — 维护 Hermes session，多轮对话保持上下文

paxd 本身不运行 Agent — 它是 Agent 和 Cloud 之间的**可靠消息中继**。

## 架构

```
┌─────────────┐     WebSocket / HTTP      ┌──────────────┐
│  pax-manager │ ◄──────────────────────► │    paxd      │
│  (Cloud)     │                           │  (daemon)    │
└─────────────┘                           └──────┬───────┘
                                                 │
                                          ┌──────▼───────┐
                                          │    Hermes     │
                                          │ (localhost)   │
                                          └──────────────┘
```

### 内部模块

```
cmd/paxd
├── main.go              CLI: configure | register | run | install-service | --version
│
internal/
├── config/config.go     YAML 配置 (~/.paxd/paxd.yaml)
├── store/store.go       本地 SQLite (agent 身份 + orphaned messages)
├── state/state.go       生命周期状态机
├── cloud/client.go      Cloud HTTP API (register, status)
├── cloud/ws.go          Cloud WebSocket (实时收消息)
├── hermes/client.go     Hermes API (chat, sessions, streaming)
├── collector/collector.go  定时上报 session + 系统状态
├── poller/poller.go     消息分发 + orphan 对账
└── executor/executor.go 消息执行 (chat/steer/command)

pkg/model/              共享数据结构
├── envelope.go          Envelope → SessionBase → TurnBase 层级
├── events.go            model.* 事件定义
├── constructors.go      事件构造函数
├── requests.go          API 请求/响应类型
└── types.go             SessionInfo, AgentState 等
```

## 状态机

```
STARTING → REGISTERING → RUNNING → STOPPING → STOPPED
```

| 状态 | 行为 |
|------|------|
| **STARTING** | 加载配置，检查 Hermes 可达性 |
| **REGISTERING** | 向 Cloud 注册，获取 node_id + api_key |
| **RUNNING** | 主循环：Status Collector + Message Poller + WS |
| **STOPPING** | 完成当前消息（30s 超时），flush offset |
| **STOPPED** | 退出 |

## 主循环

`RUNNING` 状态下的三个并发循环：

| 循环 | 周期 | 做什么 |
|------|------|--------|
| WebSocket | 实时 | 接收 Cloud 推送的 chat/steer/command 消息 |
| Status Collector | 10s | GET Hermes sessions → POST Cloud status |
| Orphan Reconcile | 30s | 重新投递被跳过的 orphaned messages |

## 消息类型

| type | 触发条件 | 行为 |
|------|---------|------|
| `chat` | session idle | 发送到 Hermes，实时 streaming 事件回传 Cloud |
| `chat` | session running | 跳过，存入 orphaned_messages 稍后重试 |
| `steer` | 任意 | 先 stop 当前 run，再发送新消息 |
| `command` | 本地 | 本地处理（upgrade, reconfig, status） |

## Streaming 事件

paxd 解析 Hermes SSE 流，产生结构化 `model.*` 事件，实时推送回 Cloud：

| entity_type | event_type | 含义 |
|-------------|-----------|------|
| `turn` | `started` | 新一轮对话开始（含 turnId） |
| `turn` | `done` | 本轮结束（含 status, usage, responseId） |
| `message` | `delta` | 逐字内容（assistant 或 tool role） |
| `tool` | `call` | 工具调用（name, arguments） |
| `agent` | `status` | 状态变化（thinking/working/idle） |
| `file` | `changed` | 文件变更（write_file / patch） |

所有事件携带 `sessionId` 用于前端会话连续性。

## 快速开始

```bash
# 前置条件：本地有 Hermes 在 localhost:8642 运行

# 注册 node + agent 并写入配置
paxd configure --cloud-url https://pax.example.com --registration-token token_xxx

# 运行守护进程
paxd run

# macOS 安装为 LaunchAgent
paxd install-service
launchctl load ~/Library/LaunchAgents/com.toddzheng.paxd.plist
```

### 家用服务器 Bootstrap

先探测这台机器已有的 harness 和 adapter：

```bash
scripts/paxd-bootstrap detect
```

安装 Codex / Claude Code 的 ACP adapter（二选一或都装）。这只安装 adapter，不安装 Codex/Claude 本体，也不配置订阅或登录态：

```bash
scripts/paxd-bootstrap install-adapter --harness codex --yes
scripts/paxd-bootstrap install-adapter --harness claude-code --yes
```

写入 `~/.paxd/paxd.yaml`。新机器只需要 `registration_token`，`node_id`、node `api_key`、`agent_id` 都由 paxd 通过 pax-manager 创建并写回：

```bash
paxd configure \
  --harness codex \
  --cloud-url https://app.example.com \
  --registration-token token_xxx
```

也可以一次注册多个 agent；同一个 harness 可以注册多个 agent：

```bash
paxd configure \
  --cloud-url https://app.example.com \
  --registration-token token_xxx \
  --agent work:codex:work \
  --agent review:claude-code:review
```

`paxd configure` 默认是 replace 语义：会把本地 `agents` 列表替换成本次命令注册出来的 agents。检测到已有 `~/.paxd/paxd.yaml` 且即将覆盖现有 agents 时，会要求确认；自动化脚本可以加 `-y` 或 `--yes` 跳过确认。

如果只想给已配置好的 node 增加 agent，用 `--append`。这种情况下会复用现有 `cloud.api_key` 注册新 agent，并把新 agent 追加到本地 `agents` 列表；不会复用旧的 `agent_id`。

```bash
paxd configure \
  --cloud-url https://app.example.com \
  --append \
  --agent review:claude-code:review
```

直接启动 forwarder，不写配置：

```bash
scripts/paxd-bootstrap run \
  --harness codex \
  --cloud-url https://app.example.com \
  --api-key pax_node_key_here \
  --agent-id agent_xxx
```

如果 pax-manager 的机器侧 tunnel 也经过 Cloudflare Access，再加 `--cf-client-id` 和 `--cf-client-secret`。

### ACP + Postman 快速路径

如果只想启动 ACP forwarder 并用 Postman 连用户侧 WebSocket，可以直接用配置或环境变量：

```bash
export PAX_CLOUD_URL="https://app.example.com"
export PAX_API_KEY="pax_node_key_here"
export PAX_AGENT_ID="agent_xxx"
export PAX_INSTANCE_ID="default"
export PAX_CLOUD_CF_CLIENT_ID="cf_service_token_client_id_here"
export PAX_CLOUD_CF_CLIENT_SECRET="cf_service_token_client_secret_here"
export PAX_ACP_HARNESS="codex"

paxd acp-forward
```

也可以不写环境变量，直接临时传参：

```bash
paxd acp-forward \
  --cloud-url https://app.example.com \
  --api-key pax_node_key_here \
  --agent-id agent_xxx \
  --harness codex \
  --cf-client-id cf_service_token_client_id_here \
  --cf-client-secret cf_service_token_client_secret_here
```

给 Postman 生成 WebSocket URL 和烟测消息：

```bash
paxd postman --cloud-url https://app.example.com --agent-id agent_xxx
```

ACP harness 预设：

| harness | 默认命令 |
|---------|----------|
| `hermes` | `hermes acp` |
| `codex` | `codex-acp`，若未安装则 `npx -y @zed-industries/codex-acp` |
| `claude` / `claude-code` | `claude-agent-acp`，若未安装则 `npx -y @agentclientprotocol/claude-agent-acp` |
| `gemini` | `gemini --acp` |
| `custom` | 必须显式配置 `command` |

`agents[].acp_forwarder.command` 或顶层 `acp_forwarder.command` 会覆盖 harness 预设。`paxd harnesses` 会检查本机实际可用的 adapter。Codex 使用 [zed-industries/codex-acp](https://github.com/zed-industries/codex-acp)，Claude 使用 [agentclientprotocol/claude-agent-acp](https://github.com/agentclientprotocol/claude-agent-acp)。传统 mailbox polling/executor 路径仍使用 Hermes HTTP API；Codex/Claude/Gemini ACP agent 不会调用 Hermes HTTP session list。

## 配置

默认路径：`~/.paxd/paxd.yaml`，旧的 `~/.pax/paxd.yaml` 仍作为兼容 fallback 读取。

```yaml
agent:
  machine_type: linux_x86    # 机器类型标签

cloud:
  api_url: https://pax-manager-xxxxx-uc.a.run.app
  node_id: "node_xxxxx"      # configure 后自动写入
  api_key: "key-xxxxx"       # node API key，configure 后自动写入
  cf_client_id: ""            # Cloudflare Access service token（可选）
  cf_client_secret: ""        # Cloudflare Access service token（可选）

hermes:
  api_endpoint: http://localhost:8642
  api_key_from_env: ~/.hermes/.env  # Hermes API key 存放的文件路径
  profile: ""                # Hermes profile 名称（可选）

agents:
  - agent_id: agent_codex
    instance_id: codex-main
    name: codex-main
    agent_type: codex
    enabled: true
    acp_forwarder:
      enabled: true
      harness: codex
      command: ["codex-acp"]  # 未安装时 configure 会写 npx fallback

  - agent_id: agent_review
    instance_id: review
    name: reviewer
    agent_type: claude-code
    enabled: true
    acp_forwarder:
      enabled: true
      harness: claude-code
      command: ["claude-agent-acp"]

daemon:
  poll_interval: 5s
  status_interval: 10s
  reconcile_interval: 30s
  log_level: info
  db_path: ~/.paxd/paxd.db

acp_forwarder:
  enabled: true               # 默认是否为 enabled agent 启动 ACP forwarder
  harness: ""                 # 可作为 agents[].acp_forwarder.harness 的默认值
  command: []                 # 可作为 agents[].acp_forwarder.command 的默认值
  working_dir: ""
  tunnel_path: /api/v1/agent/tunnel
  reconnect_interval: 2s
```

### Hermes API Key

从 `hermes.api_key_from_env` 指定的文件中读取 key。文件格式：
```
API_SERVER_KEY=your-key-here
```

## ACP Forwarder

paxd can also run a stateless Agent Client Protocol forwarder. It assumes
pax-manager exposes a WebSocket tunnel endpoint, defaulting to
`/api/v1/agent/tunnel`.

```
paxd acp-forward
```

The forwarder starts the configured ACP CLI locally, forwards every WebSocket
message from pax-manager to the CLI's stdin, and forwards every stdout line back
to the tunnel. It does not parse JSON-RPC or persist session state.

### ACP WebSocket Smoke Tester

`cmd/acp-smoke` connects to the user-side ACP tunnel, sends the standard
initialize/auth/session/prompt flow, prints every WebSocket frame exactly as it
was sent or received, and also prints an aggregate view of streaming
`session/update` text chunks.

```bash
go run ./cmd/acp-smoke \
  --url "$PAX_CLOUD_URL" \
  --agent-id "$PAX_AGENT_ID" \
  --cookie "$PAX_COOKIE" \
  --interactive
```

The cookie value can be either raw cookie pairs or a copied browser header such
as `Cookie: CF_Authorization=...; other=value`. A bare Cloudflare token can also
be passed with `--cf-authorization "$CF_AUTHORIZATION"`.

For local pax-manager tests:

```bash
go run ./cmd/acp-smoke \
  --url http://127.0.0.1:9879 \
  --agent-id "$PAX_AGENT_ID" \
  --user-email local@example.local
```

Useful options:

- `--interactive` creates a session and then lets you type prompts until
  `/quit`.
- `--prompt "..."` can be repeated to replace the built-in prompt sequence.
- `--messages-file test.ndjson` sends custom JSON-RPC messages after the smoke
  flow; use `{{sessionId}}` as a placeholder.
- `--raw-only` disables aggregate helper output while keeping raw frames.
- `--raw-stream stderr|stdout|off` controls where raw WebSocket frames are
  printed. The default is `stderr`, while aggregate output stays on `stdout`.
- `--no-color` disables ANSI colors.
- `--header 'Name: value'` can be repeated for extra auth or debugging headers.

Environment overrides:

| Env | Meaning |
|-----|---------|
| `PAX_ACP_FORWARD_ENABLED` | Enable ACP forwarding inside `paxd run` |
| `PAX_ACP_COMMAND` | Space-separated local ACP command |
| `PAX_ACP_WORKING_DIR` | Working directory for the ACP command |
| `PAX_ACP_TUNNEL_PATH` | pax-manager tunnel path |

## 本地 SQLite

数据库文件：`~/.paxd/paxd.db`

| 表 | 用途 |
|----|------|
| `agent_state` | Daemon 身份、Cloud 凭证、消息 offset |
| `orphaned_messages` | 因 session running 被跳过的消息，等待重试 |

## Session 连续性

paxd 维护 Hermes session 的全链路：

```
前端 send({sessionId}) → pax-manager routeMsg → paxd 消息
                                                       ↓
                                               POST /v1/chat/completions
                                               X-Hermes-Session-Id: api-xxx
                                                       ↓
                                                ← X-Hermes-Session-Id: api-xxx
                                                       ↓
前端 ← pax-manager ← model.* events (sessionId: api-xxx)
```

- 首次消息 → Hermes 创建 session → paxd 从 response header 捕获 session ID
- 后续消息 → paxd 在请求 header 带上 `X-Hermes-Session-Id` → Hermes 继续同一会话

## 开发

```bash
# 构建
go build -o paxd ./cmd/paxd/

# 本地测试（需要 Hermes + pax-manager 都在跑）
./paxd run

# 查看日志
tail -f ~/.pax/paxd.log
```

## Pax 生态

| 组件 | 说明 |
|------|------|
| [pax-manager](https://github.com/pax-beehive/pax-manager) | Cloud API Gateway（GCP Cloud Run） |
| **paxd** | Agent 侧守护进程（本仓库） |
| Hermes | 本地 Agent 运行时（已有产品） |
| Dashboard | Web 控制台（pax-manager 内嵌） |

## License

MIT
