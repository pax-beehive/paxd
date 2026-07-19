package localapi

import (
	"net/http"
	"strings"
)

const docsHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>paxd debug API</title>
  <style>
    :root { color-scheme: light dark; --bg: #f7f7f5; --fg: #171717; --muted: #616161; --line: #d8d8d2; --panel: #ffffff; --accent: #0f766e; --code: #f0f0ec; }
    @media (prefers-color-scheme: dark) { :root { --bg: #111210; --fg: #eeeeea; --muted: #aaa9a3; --line: #33342f; --panel: #191a17; --accent: #5eead4; --code: #252620; } }
    * { box-sizing: border-box; }
    body { margin: 0; font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: var(--bg); color: var(--fg); }
    main { max-width: 1120px; margin: 0 auto; padding: 32px 20px 56px; }
    header { display: flex; align-items: baseline; justify-content: space-between; gap: 16px; padding-bottom: 20px; border-bottom: 1px solid var(--line); }
    h1 { margin: 0; font-size: 28px; letter-spacing: 0; }
    h2 { margin: 32px 0 12px; font-size: 18px; letter-spacing: 0; }
    a { color: var(--accent); }
    .muted { color: var(--muted); }
    .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(320px, 1fr)); gap: 12px; }
    .endpoint { background: var(--panel); border: 1px solid var(--line); border-radius: 8px; padding: 14px; }
    .topline { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
    .method { min-width: 58px; text-align: center; border-radius: 999px; padding: 3px 8px; font-weight: 700; font-size: 12px; background: var(--accent); color: var(--bg); }
    code { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 13px; background: var(--code); padding: 2px 5px; border-radius: 4px; }
    details { margin-top: 10px; }
    summary { cursor: pointer; color: var(--muted); }
    ul { margin: 8px 0 0 18px; padding: 0; }
    li { margin: 4px 0; }
    pre { overflow: auto; background: var(--code); padding: 12px; border-radius: 6px; line-height: 1.45; }
    .sample { margin-top: 8px; }
    .sample-title { margin: 12px 0 6px; color: var(--muted); font-size: 12px; font-weight: 700; text-transform: uppercase; }
  </style>
</head>
<body>
<main>
  <header>
    <div>
      <h1>paxd debug API</h1>
      <div class="muted">Local control endpoints exposed by this daemon.</div>
    </div>
    <a href="/openapi.json">OpenAPI JSON</a>
  </header>
  <h2>Endpoints</h2>
  <div id="endpoints" class="grid"></div>
  <h2>Examples</h2>
  <pre>curl http://127.0.0.1:8765/v1/status
curl -X POST http://127.0.0.1:8765/v1/harnesses/discover \
  -H 'Content-Type: application/json' \
  -d '{"probe":true}'</pre>
</main>
<script>
fetch('/openapi.json')
  .then((response) => response.json())
  .then((spec) => {
    const root = document.getElementById('endpoints');
    Object.entries(spec.paths).forEach(([path, methods]) => {
      Object.entries(methods).forEach(([method, op]) => {
        const node = document.createElement('article');
        node.className = 'endpoint';
        const params = (op.parameters || []).map((p) => '<li><code>' + p.name + '</code> ' + (p.in || '') + (p.required ? ' required' : '') + '</li>').join('');
        const media = op.requestBody && op.requestBody.content && op.requestBody.content['application/json'];
        const example = media && media.example;
        const body = media ? '<li><code>body</code> ' + schemaName(media.schema) + '</li>' : '';
        const requestSample = example ? '<div class="sample"><div class="sample-title">Request JSON</div><pre>' + escapeHTML(JSON.stringify(example, null, 2)) + '</pre></div>' : '';
        const curlSample = '<div class="sample"><div class="sample-title">curl</div><pre>' + escapeHTML(curlFor(path, method, example)) + '</pre></div>';
        node.innerHTML =
          '<div class="topline"><span class="method">' + method.toUpperCase() + '</span><code>' + path + '</code></div>' +
          '<p class="muted">' + (op.summary || '') + '</p>' +
          ((params || body) ? '<details><summary>Parameters</summary><ul>' + params + body + '</ul></details>' : '') +
          '<details ' + (example ? 'open' : '') + '><summary>Example</summary>' + requestSample + curlSample + '</details>';
        root.appendChild(node);
      });
    });
  });

function schemaName(schema) {
  if (!schema) return 'object';
  if (schema.$ref) return schema.$ref.replace('#/components/schemas/', '');
  return schema.type || 'object';
}

function curlFor(path, method, example) {
  const target = 'http://127.0.0.1:8765' + examplePath(path);
  const lines = ['curl'];
  if (method.toUpperCase() !== 'GET') lines.push('-X ' + method.toUpperCase());
  lines.push(target);
  if (example) {
    lines.push("-H 'Content-Type: application/json'");
    lines.push("-d '" + JSON.stringify(example).replaceAll("'", "'\\''") + "'");
  }
  return lines.join(' \\\n  ');
}

function examplePath(path) {
  if (path.includes('/agent-connections/{id}')) return path.replace('{id}', 'conn_codex');
  if (path.includes('/commands/{id}')) return path.replace('{id}', 'cmd_local_example');
  if (path.includes('/local/sessions/{id}')) return path.replace('{id}', 'codex:sess_1');
  if (path.includes('/remotes/{id}')) return path.replace('{id}', 'default');
  return path;
}

function escapeHTML(value) {
  return String(value)
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;');
}
</script>
</body>
</html>`

type apiEndpoint struct {
	Method      string
	Path        string
	Summary     string
	OperationID string
	Parameters  []apiParameter
	RequestBody string
	Response    string
	Tags        []string
}

type apiParameter struct {
	Name        string
	In          string
	Type        string
	Required    bool
	Description string
}

func (h *Handler) routeDocsGet(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/docs" {
		h.routeNotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(docsHTML))
}

func (h *Handler) routeOpenAPIGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, openAPISpec())
}

func isDocumentationRequest(r *http.Request) bool {
	return r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/docs" || r.URL.Path == "/openapi.json")
}

func localAPIEndpoints() []apiEndpoint {
	pathID := apiParameter{Name: "id", In: "path", Type: "string", Required: true}
	commandID := apiParameter{Name: commandIDHeader, In: "header", Type: "string", Description: "Optional idempotency/audit id for command endpoints."}
	includeDisabled := apiParameter{Name: "include_disabled", In: "query", Type: "boolean"}
	includeMissing := apiParameter{Name: "include_missing", In: "query", Type: "boolean"}

	return []apiEndpoint{
		{Method: "GET", Path: "/v1/status", Summary: "Get daemon, remote, agent, harness, and local session status.", OperationID: "getStatus", Response: "QueryResult", Tags: []string{"status"}},

		{Method: "GET", Path: "/v1/remotes", Summary: "List configured remotes.", OperationID: "listRemotes", Parameters: []apiParameter{includeDisabled}, Response: "QueryResult", Tags: []string{"remotes"}},
		{Method: "POST", Path: "/v1/remotes", Summary: "Create a remote.", OperationID: "createRemote", Parameters: []apiParameter{commandID}, RequestBody: "CreateRemoteCommand", Response: "CommandAck", Tags: []string{"remotes"}},
		{Method: "GET", Path: "/v1/remotes/{id}", Summary: "Get one remote.", OperationID: "getRemote", Parameters: []apiParameter{pathID}, Response: "QueryResult", Tags: []string{"remotes"}},
		{Method: "PATCH", Path: "/v1/remotes/{id}", Summary: "Update a remote.", OperationID: "updateRemote", Parameters: []apiParameter{pathID, commandID}, RequestBody: "UpdateRemoteCommand", Response: "CommandAck", Tags: []string{"remotes"}},
		{Method: "DELETE", Path: "/v1/remotes/{id}", Summary: "Delete a remote.", OperationID: "deleteRemote", Parameters: []apiParameter{pathID, commandID, {Name: "cascade_agent_connections", In: "query", Type: "boolean"}}, Response: "CommandAck", Tags: []string{"remotes"}},
		{Method: "POST", Path: "/v1/remotes/{id}/restart", Summary: "Restart a remote control connection.", OperationID: "restartRemote", Parameters: []apiParameter{pathID, commandID}, Response: "CommandAck", Tags: []string{"remotes"}},
		{Method: "PUT", Path: "/v1/remotes/{id}/auth", Summary: "Configure remote authentication.", OperationID: "configureRemoteAuth", Parameters: []apiParameter{pathID, commandID}, RequestBody: "ConfigureRemoteAuthCommand", Response: "CommandAck", Tags: []string{"remotes"}},
		{Method: "DELETE", Path: "/v1/remotes/{id}/auth", Summary: "Clear remote authentication.", OperationID: "clearRemoteAuth", Parameters: []apiParameter{pathID, commandID}, Response: "CommandAck", Tags: []string{"remotes"}},

		{Method: "GET", Path: "/v1/agent-connections", Summary: "List desired agent tunnel connections.", OperationID: "listAgentConnections", Parameters: []apiParameter{{Name: "remote_id", In: "query", Type: "string"}, includeDisabled}, Response: "QueryResult", Tags: []string{"agent connections"}},
		{Method: "POST", Path: "/v1/agent-connections", Summary: "Create an agent tunnel connection.", OperationID: "createAgentConnection", Parameters: []apiParameter{commandID}, RequestBody: "CreateAgentConnectionCommand", Response: "CommandAck", Tags: []string{"agent connections"}},
		{Method: "GET", Path: "/v1/agent-connections/{id}", Summary: "Get one agent tunnel connection.", OperationID: "getAgentConnection", Parameters: []apiParameter{pathID}, Response: "QueryResult", Tags: []string{"agent connections"}},
		{Method: "PATCH", Path: "/v1/agent-connections/{id}", Summary: "Update an agent tunnel connection.", OperationID: "updateAgentConnection", Parameters: []apiParameter{pathID, commandID}, RequestBody: "UpdateAgentConnectionCommand", Response: "CommandAck", Tags: []string{"agent connections"}},
		{Method: "DELETE", Path: "/v1/agent-connections/{id}", Summary: "Delete an agent tunnel connection.", OperationID: "deleteAgentConnection", Parameters: []apiParameter{pathID, commandID, {Name: "deregister", In: "query", Type: "boolean"}}, Response: "CommandAck", Tags: []string{"agent connections"}},
		{Method: "POST", Path: "/v1/agent-connections/{id}/restart", Summary: "Restart an agent tunnel connection.", OperationID: "restartAgentConnection", Parameters: []apiParameter{pathID, commandID}, Response: "CommandAck", Tags: []string{"agent connections"}},

		{Method: "GET", Path: "/v1/harnesses", Summary: "List known harnesses.", OperationID: "listHarnesses", Parameters: []apiParameter{includeMissing}, Response: "QueryResult", Tags: []string{"harnesses"}},
		{Method: "POST", Path: "/v1/harnesses/discover", Summary: "Discover harness availability.", OperationID: "discoverHarnesses", RequestBody: "DiscoverHarnessesQuery", Response: "QueryResult", Tags: []string{"harnesses"}},

		{Method: "GET", Path: "/v1/local/overview", Summary: "Get local session and harness overview.", OperationID: "getLocalOverview", Response: "QueryResult", Tags: []string{"local"}},
		{Method: "GET", Path: "/v1/local/sessions", Summary: "List local sessions.", OperationID: "listLocalSessions", Parameters: []apiParameter{{Name: "agent", In: "query", Type: "string"}, {Name: "limit", In: "query", Type: "integer"}}, Response: "QueryResult", Tags: []string{"local"}},
		{Method: "POST", Path: "/v1/local/sessions/sync", Summary: "Refresh local session cache.", OperationID: "syncLocalSessions", RequestBody: "SyncLocalSessionsQuery", Response: "QueryResult", Tags: []string{"local"}},
		{Method: "GET", Path: "/v1/local/sessions/{id}", Summary: "Get one local session.", OperationID: "getLocalSession", Parameters: []apiParameter{pathID}, Response: "QueryResult", Tags: []string{"local"}},

		{Method: "GET", Path: "/v1/commands/{id}", Summary: "Get one command audit record.", OperationID: "getCommand", Parameters: []apiParameter{pathID}, Response: "QueryResult", Tags: []string{"commands"}},
	}
}

func openAPISpec() map[string]any {
	paths := map[string]any{}
	for _, endpoint := range localAPIEndpoints() {
		operations, ok := paths[endpoint.Path].(map[string]any)
		if !ok {
			operations = map[string]any{}
			paths[endpoint.Path] = operations
		}
		operations[methodKey(endpoint.Method)] = openAPIOperation(endpoint)
	}
	return map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "paxd debug control API",
			"version":     "0.1.0",
			"description": "Local-only HTTP API for inspecting and controlling paxd.",
		},
		"servers": []map[string]any{{"url": "/"}},
		"paths":   paths,
		"components": map[string]any{
			"schemas": openAPISchemas(),
		},
	}
}

func openAPIOperation(endpoint apiEndpoint) map[string]any {
	successStatus := "200"
	if endpoint.Response == "CommandAck" {
		successStatus = "202"
	}
	operation := map[string]any{
		"summary":     endpoint.Summary,
		"operationId": endpoint.OperationID,
		"tags":        endpoint.Tags,
		"responses": map[string]any{
			successStatus: openAPIResponse(endpoint.Response),
			"400":         openAPIErrorResponse("Bad request"),
			"404":         openAPIErrorResponse("Not found"),
			"409":         openAPIErrorResponse("Conflict"),
			"500":         openAPIErrorResponse("Internal error"),
		},
	}
	if len(endpoint.Parameters) > 0 {
		params := make([]map[string]any, 0, len(endpoint.Parameters))
		for _, param := range endpoint.Parameters {
			params = append(params, openAPIParameter(param))
		}
		operation["parameters"] = params
	}
	if endpoint.RequestBody != "" {
		operation["requestBody"] = map[string]any{
			"required": true,
			"content": map[string]any{
				"application/json": map[string]any{
					"schema":  schemaRef(endpoint.RequestBody),
					"example": requestExample(endpoint.RequestBody),
				},
			},
		}
	}
	return operation
}

func requestExample(schema string) map[string]any {
	switch schema {
	case "CreateRemoteCommand":
		return map[string]any{
			"remote": map[string]any{
				"id":                "default",
				"name":              "Default",
				"cloud_api_url":     "https://api.example.test",
				"node_control_path": "/api/v1/node/control",
				"agent_tunnel_path": "/api/v1/agent/tunnel",
				"node_id":           "node_123",
				"enabled":           true,
			},
			"cloud_api_key_ref": "env:PAX_NODE_API_KEY",
		}
	case "UpdateRemoteCommand":
		return map[string]any{
			"remote": map[string]any{
				"name":              "Production",
				"cloud_api_url":     "https://api.example.test",
				"node_control_path": "/api/v1/node/control",
				"agent_tunnel_path": "/api/v1/agent/tunnel",
				"enabled":           true,
			},
		}
	case "ConfigureRemoteAuthCommand":
		return map[string]any{
			"kind": "cloudflare_access",
			"cloudflare_access": map[string]any{
				"client_id":         "cf-client-id",
				"client_secret_ref": "env:PAX_CF_CLIENT_SECRET",
			},
		}
	case "CreateAgentConnectionCommand":
		return map[string]any{
			"id":             "conn_codex",
			"remote_id":      "default",
			"name":           "Codex",
			"cloud_agent_id": "agent_123",
			"instance_id":    "codex-main",
			"agent_type":     "codex",
			"harness":        "codex",
			"command":        []any{"codex-acp"},
			"working_dir":    "/Users/me/workspace",
			"env":            map[string]any{"PAX_LOG_LEVEL": "info"},
			"enabled":        true,
			"desired_state":  "running",
			"desired_slots":  2,
		}
	case "UpdateAgentConnectionCommand":
		return map[string]any{
			"name":          "Codex Main",
			"command":       []any{"npx", "-y", "@agentclientprotocol/codex-acp"},
			"working_dir":   "/Users/me/workspace",
			"enabled":       true,
			"desired_state": "running",
		}
	case "DiscoverHarnessesQuery":
		return map[string]any{
			"probe": true,
			"names": []any{"codex", "gemini"},
		}
	case "SyncLocalSessionsQuery":
		return map[string]any{
			"agent":          "codex",
			"limit":          20,
			"timeout_millis": 3000,
		}
	default:
		return nil
	}
}

func openAPIParameter(param apiParameter) map[string]any {
	item := map[string]any{
		"name":     param.Name,
		"in":       param.In,
		"required": param.Required,
		"schema": map[string]any{
			"type": param.Type,
		},
	}
	if param.Description != "" {
		item["description"] = param.Description
	}
	return item
}

func openAPIResponse(schema string) map[string]any {
	if schema == "" {
		schema = "QueryResult"
	}
	return map[string]any{
		"description": "Success",
		"content": map[string]any{
			"application/json": map[string]any{"schema": schemaRef(schema)},
		},
	}
}

func openAPIErrorResponse(description string) map[string]any {
	return map[string]any{
		"description": description,
		"content": map[string]any{
			"application/json": map[string]any{"schema": schemaRef("ControlError")},
		},
	}
}

func openAPISchemas() map[string]any {
	stringSchema := map[string]any{"type": "string"}
	boolSchema := map[string]any{"type": "boolean"}
	stringArray := map[string]any{"type": "array", "items": stringSchema}
	stringMap := map[string]any{"type": "object", "additionalProperties": stringSchema}

	return map[string]any{
		"ControlError": objectSchema(map[string]any{
			"code":    stringSchema,
			"message": stringSchema,
			"target":  stringSchema,
		}, "code", "message"),
		"CommandAck": objectSchema(map[string]any{
			"command_id":         stringSchema,
			"ok":                 boolSchema,
			"status":             enumSchema("unknown", "received", "rejected", "applied", "failed"),
			"target_type":        stringSchema,
			"target_id":          stringSchema,
			"desired_generation": map[string]any{"type": "integer", "format": "int64"},
			"error":              schemaRef("ControlError"),
		}, "command_id", "ok", "status"),
		"QueryResult": objectSchema(map[string]any{
			"type":               stringSchema,
			"error":              schemaRef("ControlError"),
			"status":             map[string]any{"type": "object"},
			"remotes":            map[string]any{"type": "object"},
			"remote":             map[string]any{"type": "object"},
			"agent_connections":  map[string]any{"type": "object"},
			"agent_connection":   map[string]any{"type": "object"},
			"harnesses":          map[string]any{"type": "object"},
			"local_overview":     map[string]any{"type": "object"},
			"local_sessions":     map[string]any{"type": "object"},
			"local_session":      map[string]any{"type": "object"},
			"local_session_sync": map[string]any{"type": "object"},
			"command":            map[string]any{"type": "object"},
		}, "type"),
		"Remote": objectSchema(map[string]any{
			"id":                stringSchema,
			"name":              stringSchema,
			"cloud_api_url":     stringSchema,
			"node_control_path": stringSchema,
			"agent_tunnel_path": stringSchema,
			"node_id":           stringSchema,
			"enabled":           boolSchema,
		}, "name", "cloud_api_url"),
		"RemotePatch": objectSchema(map[string]any{
			"name":              stringSchema,
			"cloud_api_url":     stringSchema,
			"node_control_path": stringSchema,
			"agent_tunnel_path": stringSchema,
			"node_id":           stringSchema,
			"enabled":           boolSchema,
		}),
		"CreateRemoteCommand": objectSchema(map[string]any{
			"remote":            schemaRef("Remote"),
			"cloud_api_key_ref": stringSchema,
		}, "remote"),
		"UpdateRemoteCommand": objectSchema(map[string]any{
			"remote_id":         stringSchema,
			"remote":            schemaRef("RemotePatch"),
			"cloud_api_key_ref": stringSchema,
		}),
		"ConfigureRemoteAuthCommand": objectSchema(map[string]any{
			"remote_id": stringSchema,
			"kind":      enumSchema("none", "cloudflare_access"),
			"cloudflare_access": objectSchema(map[string]any{
				"client_id":         stringSchema,
				"client_secret_ref": stringSchema,
			}, "client_id", "client_secret_ref"),
		}, "kind"),
		"CreateAgentConnectionCommand": objectSchema(map[string]any{
			"id":             stringSchema,
			"remote_id":      stringSchema,
			"name":           stringSchema,
			"cloud_agent_id": stringSchema,
			"instance_id":    stringSchema,
			"agent_type":     stringSchema,
			"harness":        stringSchema,
			"command":        stringArray,
			"working_dir":    stringSchema,
			"env":            stringMap,
			"enabled":        boolSchema,
			"desired_state":  enumSchema("running", "stopped", "deleted"),
			"desired_slots":  map[string]any{"type": "integer", "minimum": 1, "maximum": 16, "default": 2},
		}, "remote_id", "name", "instance_id", "agent_type", "harness", "command"),
		"UpdateAgentConnectionCommand": objectSchema(map[string]any{
			"connection_id":  stringSchema,
			"name":           stringSchema,
			"cloud_agent_id": stringSchema,
			"instance_id":    stringSchema,
			"agent_type":     stringSchema,
			"harness":        stringSchema,
			"command":        stringArray,
			"working_dir":    stringSchema,
			"env":            stringMap,
			"enabled":        boolSchema,
			"desired_state":  enumSchema("running", "stopped", "deleted"),
			"desired_slots":  map[string]any{"type": "integer", "minimum": 1, "maximum": 16},
		}),
		"DiscoverHarnessesQuery": objectSchema(map[string]any{
			"probe": boolSchema,
			"names": stringArray,
		}),
		"SyncLocalSessionsQuery": objectSchema(map[string]any{
			"agent":          stringSchema,
			"limit":          map[string]any{"type": "integer"},
			"timeout_millis": map[string]any{"type": "integer", "format": "int64"},
		}),
	}
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func enumSchema(values ...string) map[string]any {
	enum := make([]any, 0, len(values))
	for _, value := range values {
		enum = append(enum, value)
	}
	return map[string]any{"type": "string", "enum": enum}
}

func schemaRef(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

func methodKey(method string) string {
	return strings.ToLower(method)
}
