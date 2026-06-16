---
name: paxd-deployment
description: Deploy and smoke-test paxd with pax-manager ACP WebSocket forwarding, Hermes ACP, Cloudflare Access headers, node registration, node-agent creation, paxd.yaml configuration, Postman testing, and common debugging for 502/1006/tunnel path/auth issues.
---

# Paxd Deployment

## Overview

Use this skill to guide a tester through deploying `paxd` with ACP forwarding through `pax-manager` and validating a Hermes-backed ACP session. Keep secrets out of transcripts and docs; use placeholders for Cloudflare service tokens, registration tokens, node API keys, and Pax keys.

The expected architecture is:

```text
ACP client/Postman
  -> Cloudflare Access
  -> pax-manager /api/v1/user/{user_id}/agents/{agent_id}/tunnel
  -> pax-manager in-memory ACP tunnel hub
  -> paxd /api/v1/agent/tunnel
  -> hermes acp
```

## Prerequisites

Confirm these before starting:

- `pax-manager` is deployed from `origin/main` with ACP tunnel endpoints.
- The Cloud Run request timeout is long enough for testing, usually at least `300s`.
- Cloudflare Access allows service-token auth for node/paxd requests.
- The target Linux server has `hermes` installed and configured.
- The tester has a Cloudflare-authenticated user identity or cookie for user-side APIs.
- The tester has or can obtain:
  - `PAX_CLOUD_URL`, for example `https://app.example.com`
  - `CF_ACCESS_CLIENT_ID`
  - `CF_ACCESS_CLIENT_SECRET`
  - a node registration token
  - the node API key returned by registration
  - the `agent_id` returned by node-agent creation

## Manager Checks

Verify the tunnel path is versioned:

```text
GET /api/v1/agent/tunnel
GET /api/v1/user/{user_id}/agents/{agent_id}/tunnel
```

Do not use the old path:

```text
/api/agent/tunnel
```

If logs show `static/api/agent/tunnel`, the client is still hitting the old path or the deployed manager revision does not include the v1 route.

For Cloud Run, WebSockets are long-running HTTP requests. If the agent tunnel disconnects at about 30 seconds, increase request timeout for the service or deploy the tunnel as a separate service:

```bash
gcloud run services update pax-manager \
  --region <region> \
  --timeout=300
```

Do not assume a 30-second WebSocket disconnect is a paxd bug until Cloud Run timeout is checked.

## Registration Flow

Use the Cloudflare-authenticated user side to create a node registration token:

```bash
curl -sS -X POST "$PAX_CLOUD_URL/api/v1/user/self/node-registration-tokens" \
  -H 'Content-Type: application/json' \
  -H "CF-Access-Client-Id: $CF_ACCESS_CLIENT_ID" \
  -H "CF-Access-Client-Secret: $CF_ACCESS_CLIENT_SECRET" \
  -d '{}'
```

Register the server as a node:

```bash
curl -sS -X POST "$PAX_CLOUD_URL/api/v1/node/register" \
  -H 'Content-Type: application/json' \
  -H "CF-Access-Client-Id: $CF_ACCESS_CLIENT_ID" \
  -H "CF-Access-Client-Secret: $CF_ACCESS_CLIENT_SECRET" \
  -H "X-Registration-Token: $REGISTRATION_TOKEN" \
  -d '{"name":"hermes-node","hostname":"'"$(hostname)"'","machine_type":"server","os":"linux","arch":"amd64","paxd_version":"0.1.0"}'
```

Save the returned `nodeId` and `apiKey`. The `apiKey` is the paxd `cloud.api_key` / `PAX_API_KEY`; do not use the registration token after node registration.

Create a node agent:

```bash
curl -sS -X POST "$PAX_CLOUD_URL/api/v1/user/self/nodes/$NODE_ID/agents" \
  -H 'Content-Type: application/json' \
  -H "CF-Access-Client-Id: $CF_ACCESS_CLIENT_ID" \
  -H "CF-Access-Client-Secret: $CF_ACCESS_CLIENT_SECRET" \
  -d '{"name":"hermes-agent","agent_type":"hermes"}'
```

Save the returned `agent.agentId`. The ACP forwarder must send this as `agent_id` on `/api/v1/agent/tunnel`.

## Paxd Config

For home-server setup, prefer the bootstrap helper when it is available:

```bash
scripts/paxd-bootstrap detect
scripts/paxd-bootstrap install-adapter --harness codex --yes
scripts/paxd-bootstrap configure \
  --harness codex \
  --cloud-url "$PAX_CLOUD_URL" \
  --api-key "$PAX_API_KEY" \
  --agent-id "$PAX_AGENT_ID"
```

Use `--harness claude-code`, `--harness gemini`, or `--harness hermes` for other local runtimes. Add `--cf-client-id` and `--cf-client-secret` when the machine-side tunnel is protected by Cloudflare Access.

Create or update `~/.pax/paxd.yaml` on the server. `cloud.api_url` is the canonical key; `cloud.url` is accepted as a compatibility alias:

```yaml
cloud:
  api_url: https://app.example.com
  api_key: pax_node_key_here
  cf_client_id: cf_service_token_client_id_here
  cf_client_secret: cf_service_token_client_secret_here

agent_id: agent_xxx
instance_id: default

acp_forwarder:
  enabled: true
  harness: hermes
  command: ["hermes", "acp"]
  working_dir: ""
  tunnel_path: /api/v1/agent/tunnel
  reconnect_interval: 2s
```

Equivalent environment overrides are acceptable for smoke tests:

```bash
export PAX_CLOUD_URL="https://app.example.com"
export PAX_API_KEY="pax_node_key_here"
export PAX_AGENT_ID="agent_xxx"
export PAX_INSTANCE_ID="default"
export PAX_CLOUD_CF_CLIENT_ID="cf_service_token_client_id_here"
export PAX_CLOUD_CF_CLIENT_SECRET="cf_service_token_client_secret_here"
export PAX_ACP_HARNESS="hermes"
export PAX_ACP_COMMAND="hermes acp"
export PAX_ACP_TUNNEL_PATH="/api/v1/agent/tunnel"
export PAX_ACP_RECONNECT_INTERVAL="2s"
```

Supported ACP harness presets are `hermes` (`hermes acp`), `gemini` (`gemini --acp`), `codex` (`codex-acp` or `npx -y @zed-industries/codex-acp`), and `claude-code` (`claude-agent-acp` or `npx -y @agentclientprotocol/claude-agent-acp`). Run `paxd harnesses` on the target machine to inspect local adapter support. Use `custom` plus `command` for other ACP-compatible adapters. Explicit `command` overrides the preset.

Prefer a short reconnect interval while testing. A healthy forwarder should reset exponential backoff after any successful tunnel connection.

Start forwarding:

```bash
paxd acp-forward
```

For one-off local testing, pass the same values as flags instead of creating a config file:

```bash
paxd acp-forward \
  --cloud-url "$PAX_CLOUD_URL" \
  --api-key "$PAX_API_KEY" \
  --agent-id "$PAX_AGENT_ID" \
  --instance-id "${PAX_INSTANCE_ID:-default}" \
  --harness "${PAX_ACP_HARNESS:-hermes}" \
  --cf-client-id "$CF_ACCESS_CLIENT_ID" \
  --cf-client-secret "$CF_ACCESS_CLIENT_SECRET"
```

Expected log shape:

```text
[acp-forwarder] connected wss://.../api/v1/agent/tunnel?agent_id=agent_xxx&instance_id=default -> hermes
[acp-forwarder stderr] ... Starting hermes-agent ACP adapter
[acp-forwarder stderr] ... ACP client connected
```

## Postman WebSocket Smoke Test

Generate the Postman URL and smoke-test JSON messages from the same config:

```bash
paxd postman --cloud-url "$PAX_CLOUD_URL" --agent-id "$PAX_AGENT_ID"
```

Connect Postman to the printed user tunnel. The URL shape is:

```text
wss://app.example.com/api/v1/user/self/agents/<agent_id>/tunnel
```

Include the same Cloudflare user auth material that makes `/api/v1/user/self/me` work. If using cookies, ensure Postman sends the Cloudflare Access cookie for the `app.example.com` domain.

Send `initialize`:

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{},"clientInfo":{"name":"postman","version":"0.1.0"}}}
```

A successful response includes:

```json
{
  "agentInfo": {"name": "hermes-agent"},
  "protocolVersion": 1,
  "authMethods": [{"id": "deepseek"}]
}
```

Authenticate if the initialize response advertises a configured runtime method:

```json
{"jsonrpc":"2.0","id":2,"method":"authenticate","params":{"methodId":"deepseek"}}
```

Create a session. Include `mcpServers` even when empty because some Hermes ACP versions require the field:

```json
{"jsonrpc":"2.0","id":3,"method":"session/new","params":{"cwd":"/tmp","mcpServers":[]}}
```

Copy the returned `sessionId`, then send a prompt:

```json
{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"session_id_here","prompt":[{"type":"text","text":"Say hello in one short sentence."}]}}
```

Streaming `session/update` notifications without `id` are normal. The prompt is complete when a response with the same request id arrives.

To validate session continuity, send a memory probe:

```json
{"jsonrpc":"2.0","id":5,"method":"session/prompt","params":{"sessionId":"session_id_here","prompt":[{"type":"text","text":"Remember the passphrase blue-mango. Reply only OK."}]}}
```

Then:

```json
{"jsonrpc":"2.0","id":6,"method":"session/prompt","params":{"sessionId":"session_id_here","prompt":[{"type":"text","text":"What passphrase did I ask you to remember?"}]}}
```

The expected answer should mention `blue-mango`.

## Troubleshooting

- **`400 EOF` on JSON APIs**: send a valid JSON body, usually `{}`, and `Content-Type: application/json`.
- **Cloudflare signin HTML or 302**: the request did not satisfy Cloudflare Access. Use a Service Auth policy for service tokens, not only an interactive login policy.
- **`static/api/agent/tunnel`**: the caller is hitting old `/api/agent/tunnel`. Use `/api/v1/agent/tunnel`.
- **`websocket: bad handshake`**: inspect Cloud Run request logs for status. `404` usually means wrong path or old revision; `401/403` means auth; `502` can mean upstream closed during handshake.
- **agent tunnel closes around 30 seconds**: check Cloud Run request timeout. Use at least `300s` for smoke tests.
- **Postman user WSS closes with `1006`**: check whether the agent tunnel also disconnects. If both disconnect at the same interval, suspect Cloud Run timeout or server WebSocket handling.
- **`mcpServers` missing**: send `"mcpServers":[]` in `session/new`.
- **`protocolVersion` missing**: include `"protocolVersion":1` in `initialize.params`.
- **`agent tunnel not connected`**: paxd is not currently connected or is in reconnect backoff. Wait for `[acp-forwarder] connected` or lower `reconnect_interval`.
- **node key cannot authenticate tunnel**: ensure `agent_id` is present and belongs to the node registered with the node API key.
- **origin IP receives bot scans**: Cloud Run may have `allUsers roles/run.invoker`; this is separate from ACP testing. Do not put secrets in static files or logs.

## Handoff Checklist

When handing the setup to another tester, provide:

- Cloud URL
- Cloudflare auth method to use in Postman
- instructions for obtaining a fresh node registration token
- target server SSH access
- expected `paxd.yaml` template with placeholders
- the node `agent_id` after creation
- the exact user WebSocket URL
- the four smoke-test JSON-RPC messages
- known timeout/reconnect expectations
