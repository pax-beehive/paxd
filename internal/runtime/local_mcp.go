package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Local launch descriptions are resolved only on the agent machine, after
// decryption. No tool-specific state belongs to paxd.
func injectLocalMCP(payload []byte, agentID, sessionID string) ([]byte, error) {
	var header struct{ Method string }
	if json.Unmarshal(payload, &header) != nil || (header.Method != "session/new" && header.Method != "session/resume") {
		return payload, nil
	}
	filename := os.Getenv("PAXD_LOCAL_MCP_CONFIG")
	if filename == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		filename = filepath.Join(home, ".paxd", "mcp.json")
	}
	data, err := os.ReadFile(filename)
	if os.IsNotExist(err) {
		return payload, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read local MCP configuration: %w", err)
	}
	return expandLocalMCP(payload, data, agentID, sessionID)
}

func expandLocalMCP(payload, data []byte, agentID, sessionID string) ([]byte, error) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return nil, err
	}
	var method string
	_ = json.Unmarshal(msg["method"], &method)
	if method != "session/new" && method != "session/resume" {
		return payload, nil
	}
	var config struct {
		Servers []map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("invalid local MCP configuration: %w", err)
	}
	if len(config.Servers) == 0 {
		return payload, nil
	}
	if agentID == "" || sessionID == "" {
		return nil, fmt.Errorf("local MCP injection requires agent and session identity")
	}
	identity, _ := json.Marshal([]string{agentID, sessionID})
	digest := sha256.Sum256(identity)
	key := hex.EncodeToString(digest[:])
	replace := strings.NewReplacer("${PAX_AGENT_ID}", agentID, "${PAX_SESSION_ID}", sessionID, "${PAX_SESSION_KEY}", key)
	var params map[string]json.RawMessage
	if err := json.Unmarshal(msg["params"], &params); err != nil {
		return nil, err
	}
	if params == nil {
		return nil, fmt.Errorf("session lifecycle params required")
	}
	var servers []map[string]json.RawMessage
	if raw, ok := firstJSONField(params, "mcpServers", "mcp_servers"); ok {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return nil, err
		}
	}
	for _, server := range config.Servers {
		var name, command string
		_ = json.Unmarshal(server["name"], &name)
		_ = json.Unmarshal(server["command"], &command)
		if name == "" || command == "" {
			return nil, fmt.Errorf("local MCP requires name and command")
		}
		// Names are scoped to a stable logical session, not an ACP slot.
		server["name"], _ = json.Marshal(name + "-" + key[:16])
		var env []map[string]string
		if raw := server["env"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &env); err != nil {
				return nil, err
			}
		}
		for _, item := range env {
			if item == nil || item["name"] == "" {
				return nil, fmt.Errorf("local MCP environment entry requires name")
			}
			item["value"] = replace.Replace(item["value"])
		}
		server["env"], _ = json.Marshal(env)
		filtered := servers[:0]
		for _, existing := range servers {
			if string(existing["name"]) != string(server["name"]) {
				filtered = append(filtered, existing)
			}
		}
		servers = append(filtered, server)
	}
	params["mcpServers"], _ = json.Marshal(servers)
	delete(params, "mcp_servers")
	msg["params"], _ = json.Marshal(params)
	return json.Marshal(msg)
}
