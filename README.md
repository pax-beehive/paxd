# paxd — Pax Fleet Daemon

> Agent 时代的 kubelet。跑在每台 Agent 机器上，连接 Pax Cloud，接收消息，驱动本地 Hermes 执行。

## 什么是 paxd

paxd 是 Pax 平台的 agent 侧守护进程。它常驻在运行 Hermes（或任意兼容 Agent）的机器上，负责：

1. **注册到 Pax Cloud** — 上报机器身份、能力、状态
2. **接收消息** — WebSocket 实时接收或 HTTP 轮询 Cloud mailbox
3. **驱动执行** — 将消息转发给本地 Hermes，实时 streaming 事件回传
4. **会话连续性** — 维护 Hermes session，多轮对话保持上下文

paxd 本身不运行 Agent — 它是 Agent 和 Cloud 之间的**可靠消息中继**。

## How to install

推荐先安装 `paxl`，再通过 `paxl setup --with-daemon` 安装和配置 `paxd`。`paxctl` 已退役，不再作为独立 binary 发布：

```bash
curl -fsSL --max-redirs 1 https://api.lakeward.net/api/v1/public/paxl/install.sh | bash
paxl setup --with-daemon
```

自托管 manager 使用自己的入口。release 生成的 installer 已默认指向发布它的
manager；显式传下载入口也兼容旧 installer，并能避免误回 hosted Pax：

```bash
export PAX_MANAGER_URL='https://pax.home.example'
curl -fsSL --max-redirs 1 "$PAX_MANAGER_URL/api/v1/public/paxl/install.sh" |
  PAXL_DOWNLOAD_URL="$PAX_MANAGER_URL" bash
paxl setup --with-daemon --cloud-url "$PAX_MANAGER_URL"
```

如果只需要直接安装 daemon runtime，`paxd` installer 仍然可用，但它只安装 `paxd` binary：

```text
https://api.lakeward.net/api/v1/public/paxd/install.sh
```

自托管环境直接安装 daemon 时，对应命令是：

```bash
curl -fsSL --max-redirs 1 "$PAX_MANAGER_URL/api/v1/public/paxd/install.sh" |
  PAX_DOWNLOAD_URL="$PAX_MANAGER_URL" bash
```

脚本实际存放在 pax-manager 配置的 S3-compatible object storage：

```text
s3://<bucket>/paxd/releases/<version>/install.sh
```

installer 入口允许且最多跟随 manager 返回的第一次 302；拿到脚本以后，resolver
请求和 resolver 返回的 signed object URL 都拒绝重定向。这样 Cloudflare 登录页或
对象存储的意外跳转不会被最终的 HTTP 200 掩盖，也不会把 bearer URL 带到下一跳。

支持的 stable `paxd` binary 平台链接：

```text
https://api.lakeward.net/api/v1/public/paxd/download?platform=darwin/arm64&tags=stable
https://api.lakeward.net/api/v1/public/paxd/download?platform=darwin/amd64&tags=stable
https://api.lakeward.net/api/v1/public/paxd/download?platform=linux/arm64&tags=stable
https://api.lakeward.net/api/v1/public/paxd/download?platform=linux/amd64&tags=stable
https://api.lakeward.net/api/v1/public/paxd/download?platform=windows/amd64&tags=stable
```

安装器和 binary 下载接口走 `https://api.lakeward.net`。paxd 默认也使用 `https://api.lakeward.net` 调用 Pax API；pairing/login 的用户入口由 pax-manager 返回，默认是 `https://ws.lakeward.net`。

如果你把这个仓库或安装链接交给一个 coding agent，可以直接让它运行 `paxl setup --with-daemon`。setup 会打印 Pax pairing URL 和 6 位 code；用户登录并 approve 后，paxd 会把 node API key 写入本机配置。

## Release artifacts

`scripts/release_paxd.sh` 使用 AWS CLI 把 binary 和 installer 上传到 AWS
S3 或 S3-compatible storage。每个 object 都设置 content type 与 `sha256`
metadata，并把同一 digest 的 base64 形式作为原生 S3 SHA-256 checksum；所有上传
都使用 `If-None-Match: *`，不会覆盖已有 object。PUT 失败时（包括上传已经落盘但
客户端丢失响应），脚本会通过 `aws s3api head-object` 严格比较 size、content type、
`sha256` metadata，以及服务返回时的原生 checksum；完全一致按幂等成功继续，任何
不一致都会终止 release。随后脚本以
`generation=0` 向 pax-manager 发布 bucket/object/sha/size/content-type；安装器
与 updater 只下载 manager resolver 返回的 HTTPS signed URL，因此 bucket
不需要公开。上传、发布或验证时必须显式设置 `PAX_RELEASE_BUCKET`，脚本不提供
默认 bucket；纯 build 可以不设置。

release 不会原样上传仓库中的通用 installer；它会在 `dist` 生成副本，把
`PAX_RELEASE_MANAGER_URL` 安全地写成 binary resolver 的默认 base URL，再对这个
副本计算 hash、上传和发布 metadata。运行 installer 时显式设置
`PAX_DOWNLOAD_URL` 仍然优先。带 credentials、query 或 fragment 的 manager URL
会在生成 installer 前被拒绝。

AWS S3 示例：

```bash
export AWS_REGION='us-west-2'
export AWS_ACCESS_KEY_ID='<access-key-id>'
export AWS_SECRET_ACCESS_KEY='<secret-access-key>'
export PAX_RELEASE_BUCKET='my-pax-releases'
export PAX_RELEASE_MANAGER_URL='https://api.example.com'
export PAX_RELEASE_TOKEN='<manager-admin-bearer-token>'
scripts/release_paxd.sh 0.2.0 stable
```

MinIO 示例：

```bash
export AWS_REGION='us-east-1'
export AWS_ACCESS_KEY_ID='<minio-access-key>'
export AWS_SECRET_ACCESS_KEY='<minio-secret-key>'
export PAX_RELEASE_S3_ENDPOINT='http://127.0.0.1:9000'
export PAX_RELEASE_BUCKET='pax-releases'
export PAX_RELEASE_MANAGER_URL='https://api.example.com'
export PAX_RELEASE_TOKEN='<manager-admin-bearer-token>'
scripts/release_paxd.sh 0.2.0 stable
```

pax-manager 必须配置同一个 bucket、endpoint、region 和 credentials。
`PAX_MANAGER_URL` 仍是 manager URL 的兼容别名，`PAX_RELEASE_ID_TOKEN` 仍是
`PAX_RELEASE_TOKEN` 的 deprecated alias；release script 不再调用 `gcloud`。

如果 manager 受 Cloudflare Access 保护，需要同时设置 service token 的两个值：

```bash
export PAX_CLOUD_CF_CLIENT_ID='<cloudflare-access-client-id>'
export PAX_CLOUD_CF_CLIENT_SECRET='<cloudflare-access-client-secret>'
```

release script 只会在 manager HTTP 请求中加入 `CF-Access-Client-Id` 和
`CF-Access-Client-Secret`；两者必须成对设置，不会加入 AWS/S3 请求，也不会写入日志。

## How to start

### 1. Pair this machine

`paxl setup --with-daemon` 会安装本地 agent hooks，确保 `paxd` 已安装，然后运行 daemon pairing/setup。也可以手动运行 daemon setup：

```bash
paxl daemon setup
```

`setup` 会创建 `~/.paxd`，打开浏览器 pairing 页面，等待用户 approve，然后把 node API key 存成本机 owner-only secret，并安装/启动后台 service。

如果要连非默认环境：

```bash
paxl daemon setup --cloud-url https://api.example.com
```

当前内测阶段，如果目标 Pax API 入口仍在 Cloudflare Access 后面，需要显式提供 Cloudflare Access service token，确保 `setup/login` 以及后续 daemon 连接都能通过访问层：

```bash
PAX_CLOUD_CF_CLIENT_ID="cf-service-token-client-id" \
PAX_CLOUD_CF_CLIENT_SECRET="cf-service-token-client-secret" \
paxl daemon setup --cloud-url https://api.example.com
```

这两个值来自 Cloudflare Access service token，不是用户登录 token。`paxd setup` 会在 pairing 成功后把 secret 写成本机 owner-only file secret，并把 remote auth 写入本机 daemon DB；后续后台 service 重启后仍会继续使用它。

也可以只做登录，不安装或启动后台 service：

```bash
paxd login --remote default --cloud-url https://api.example.com
```

### 2. Verify the daemon

```bash
paxd service status
paxl daemon status
paxl daemon remote list
```

Update paths:

```bash
paxl update check
paxl update
paxl daemon update
paxd update check
paxd update
```

`paxd update` 默认从本机 `default` remote 的 `cloud_api_url` 解析 release；多
remote 环境可用 `paxd update --remote home`。只有需要绕过 remote 配置时才传
`--resolver-url https://manager.example/api/v1/public/paxd/download`。

本机 daemon 的数据在：

```text
~/.paxd/paxd.db
~/.paxd/secrets/remotes/<remote>/node_key
```

### 3. Connect a local agent

`paxl daemon ...` 通过本机 control API 管理 daemon 的 desired state。常见路径：

```bash
paxl daemon harness discover --probe codex claude
paxl daemon agent create --harness codex --name work
paxl daemon agent list
```

多 remote 时显式指定 remote：

```bash
paxl daemon agent create --remote staging --harness codex --name review
```

常用 agent 操作：

```bash
paxl daemon agent restart work
paxl daemon agent stop work
paxl daemon agent remove work
```

### 4. Run in foreground for development

开发或排障时可以不走 service，直接前台跑：

```bash
paxd service stop
paxd run
```

如需本地 HTTP debug control API：

```bash
paxd run --debug-http 127.0.0.1:8765
```

默认生产入口是 `https://api.lakeward.net`。`paxd setup/login` 使用 browser approval flow；CLI 本身不需要用户手动提供 node secret。

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
├── main.go              CLI: setup | login | run | service | --version
│
internal/
├── config/config.go     YAML 配置 (~/.paxd/paxd.yaml)
├── daemonstore/         本地 SQLite desired state + runtime status
├── state/state.go       生命周期状态机
├── cloud/client.go      Cloud HTTP API (register, status)
├── runtime/             node-control / agent tunnel runtime
├── control/             本机 control command/query model
├── collector/collector.go  定时上报 session + 系统状态
└── executor/executor.go 本地命令执行 helper

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

## Harness presets

ACP harness 预设：

| harness | 默认命令 |
|---------|----------|
| `hermes` | `hermes acp` |
| `codex` | `codex-acp`，若未安装则 `npx -y @agentclientprotocol/codex-acp` |
| `claude` / `claude-code` | `claude-agent-acp`，若未安装则 `npx -y @agentclientprotocol/claude-agent-acp` |
| `gemini` | `gemini --acp` |
| `kimi` | `kimi acp`（需先在本机 `kimi login`） |
| `pi` | `pi-acp`，若未安装则 `npx -y pi-acp` |
| `custom` | 必须显式配置 `command` |

`agents[].acp_forwarder.command` 或顶层 `acp_forwarder.command` 会覆盖 harness 预设。`paxd harnesses` 会检查本机实际可用的 adapter。Codex 使用 [agentclientprotocol/codex-acp](https://github.com/agentclientprotocol/codex-acp)，Claude 使用 [agentclientprotocol/claude-agent-acp](https://github.com/agentclientprotocol/claude-agent-acp)。传统 mailbox polling/executor 路径仍使用 Hermes HTTP API；Codex/Claude/Gemini ACP agent 不会调用 Hermes HTTP session list。

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
  log_file: ~/.paxd/logs/paxd.log   # 滚动日志路径，留空则只写 stderr
  log_max_size_mb: 20               # 超过后轮转 paxd.log.1..N
  log_max_backups: 3                # 保留的轮转备份数
  db_path: ~/.paxd/paxd.db

acp_forwarder:
  enabled: true               # 默认是否为 enabled agent 启动 ACP forwarder
  harness: ""                 # 可作为 agents[].acp_forwarder.harness 的默认值
  command: []                 # 可作为 agents[].acp_forwarder.command 的默认值
  working_dir: ""
  tunnel_path: /api/v1/agent/tunnel
  reconnect_interval: 2s
```

只有 `agent_type: hermes` 的 legacy/Hermes HTTP agent 需要 `hermes.api_endpoint`、`api_key_from_env` 和 `profile`；Codex/Claude/Gemini ACP agent 不需要这些字段。

### Hermes API Key

从 `hermes.api_key_from_env` 指定的文件中读取 key。文件格式：
```
API_SERVER_KEY=your-key-here
```

## ACP Forwarder

### Staged E2EE payload transport

Set the same development root key in paxd and in the Console Security page:

```bash
PAX_E2EE_ROOT_KEY=<base64-encoded 32-byte key>
```

paxd accepts the development root key only through `PAX_E2EE_ROOT_KEY` and
validates it during startup. The root key is used only at the Browser and paxd
endpoints; never add it to pax-manager configuration. This manual shared-key
flow is the first rollout stage and will be replaced by local-confirmation ECDH
pairing and OS Keychain storage.

With E2EE enabled, the existing reliable ACP WebSocket remains the live
transport. paxd decrypts encrypted commands immediately before local ACP
dispatch and encrypts ACP events before reliable WebSocket journaling. Legacy
non-E2EE traffic remains available during the staged workbench migration.

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
  --cookie "$PAX_COOKIE" \
  --interactive
```

The cookie value can be either raw cookie pairs or a copied browser header such
as `Cookie: CF_Authorization=...; other=value`. A bare Cloudflare token can also
be passed with `--cf-authorization "$CF_AUTHORIZATION"`.

List the agents visible to the cookie-backed user:

```bash
go run ./cmd/acp-smoke \
  --url "$PAX_CLOUD_URL" \
  --cookie "$PAX_COOKIE" \
  --list-agents
```

If exactly one online agent is visible, `cmd/acp-smoke` selects it automatically.
When multiple online agents are visible, pass either the exact `--agent-id` or
the agent's configured name with `--agent-name`:

```bash
go run ./cmd/acp-smoke \
  --url "$PAX_CLOUD_URL" \
  --cookie "$PAX_COOKIE" \
  --agent-name review \
  --interactive
```

For local pax-manager tests:

```bash
go run ./cmd/acp-smoke \
  --url http://127.0.0.1:9879 \
  --agent-name codex \
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

## 日志与诊断

paxd 自己维护滚动日志 `~/.paxd/logs/paxd.log`（大小与备份数见上面 `daemon` 配置；前台运行时同时输出到终端）。ACP harness 子进程的 stderr 会逐行带前缀写入该日志，进程异常退出时最后 2KB stderr 会写进 slot status 的 `last_error_message`，`paxl daemon agent list` 与云端上报均可见。

本机诊断快照（transport 队列积压/未 ack 帧、重连状态、slot 阶段、日志文件大小）：

```bash
curl -s --unix-socket ~/.paxd/paxd.sock http://paxd/v1/diagnostics
```

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
