package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pax-beehive/paxd/internal/e2ee"
	"github.com/pax-beehive/paxkit/reliablemq"
	"github.com/stretchr/testify/require"
)

func TestLocalMCPInjectsStableIdentityWithoutReplacingClientServers(t *testing.T) {
	templates := []byte(`{"mcpServers":[{"name":"browser","command":"/local/browser","args":[],"env":[{"name":"AGENT_BROWSER_SESSION_KEY","value":"${PAX_SESSION_KEY}"},{"name":"SESSION","value":"${PAX_SESSION_ID}"}]}]}`)
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/tmp","mcpServers":[{"name":"other","command":"other"}]}}`)
	got, err := expandLocalMCP(frame, templates, "agent-a", "session-a")
	require.NoError(t, err)
	var msg struct {
		Params struct {
			Servers []struct {
				Name string
				Env  []map[string]string
			} `json:"mcpServers"`
		}
	}
	require.NoError(t, json.Unmarshal(got, &msg))
	require.Len(t, msg.Params.Servers, 2)
	require.Equal(t, "other", msg.Params.Servers[0].Name)
	require.Equal(t, "session-a", msg.Params.Servers[1].Env[1]["value"])
	again, err := expandLocalMCP(got, templates, "agent-a", "session-a")
	require.NoError(t, err)
	require.JSONEq(t, string(got), string(again))
	other, err := expandLocalMCP(frame, templates, "agent-a", "session-b")
	require.NoError(t, err)
	require.NotEqual(t, string(got), string(other))
}

func TestLocalMCPLeavesOrdinaryOperationsUnchanged(t *testing.T) {
	frame := []byte(`{"method":"session/prompt","params":{"sessionId":"native"}}`)
	got, err := expandLocalMCP(frame, []byte(`invalid`), "a", "s")
	require.NoError(t, err)
	require.Equal(t, frame, got)
}

func TestLocalMCPRejectsNullEnvironmentEntry(t *testing.T) {
	_, err := expandLocalMCP(
		[]byte(`{"method":"session/new","params":{"mcpServers":[]}}`),
		[]byte(`{"mcpServers":[{"name":"browser","command":"browser","env":[null]}]}`), "a", "s")
	require.ErrorContains(t, err, "environment entry requires name")
}

func TestLocalMCPPreservesLegacyClientServers(t *testing.T) {
	got, err := expandLocalMCP(
		[]byte(`{"method":"session/resume","params":{"mcp_servers":[{"name":"other","command":"other"}]}}`),
		[]byte(`{"mcpServers":[{"name":"browser","command":"browser"}]}`), "a", "s")
	require.NoError(t, err)
	require.Contains(t, string(got), `"name":"other"`)
	require.NotContains(t, string(got), `mcp_servers`)
}

func TestE2EELocalMCPUsesEnvelopeIdentityAfterDecryption(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "mcp.json")
	require.NoError(t, os.WriteFile(filename, []byte(`{"mcpServers":[{"name":"browser","command":"/local/browser","env":[{"name":"SESSION","value":"${PAX_SESSION_ID}"}]}]}`), 0600))
	t.Setenv("PAXD_LOCAL_MCP_CONFIG", filename)
	root := make([]byte, 32)
	bridge := newE2EETransportBridge(root, "agent_1", "queue_1",
		&fakeE2EECommandStore{seen: make(map[string]bool)},
		func(context.Context, []byte, reliablemq.Metadata) error { return nil })
	envelope, err := e2ee.Encrypt(root, e2ee.DirectionCommand, e2ee.Metadata{
		RecordID: "cmd-local", AgentID: "agent_1", SessionID: "session-trusted", Kind: "acp_command", KeyEpoch: 1,
	}, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/tmp","mcpServers":[]}}`))
	require.NoError(t, err)
	payload, err := json.Marshal(envelope)
	require.NoError(t, err)
	called := false
	handled, err := bridge.handleCommand(context.Background(), reliablemq.Frame{
		Payload: payload, Metadata: reliablemq.Metadata{"connection_epoch": "1", "command_id": "cmd-local"},
	}, func(_ context.Context, sessionID string, plaintext []byte) (string, error) {
		called = true
		expanded, err := injectLocalMCP(plaintext, "agent_1", sessionID)
		require.NoError(t, err)
		require.Contains(t, string(expanded), `"value":"session-trusted"`)
		require.Contains(t, string(expanded), `"command":"/local/browser"`)
		return "", nil
	})
	require.NoError(t, err)
	require.True(t, handled)
	require.True(t, called)
}

func TestLocalMCPMissingConfigurationIsNoopAndMalformedConfigurationFails(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "mcp.json")
	t.Setenv("PAXD_LOCAL_MCP_CONFIG", filename)
	frame := []byte(`{"method":"session/resume","params":{"sessionId":"native","mcpServers":[]}}`)
	got, err := injectLocalMCP(frame, "a", "s")
	require.NoError(t, err)
	require.Equal(t, frame, got)
	require.NoError(t, os.WriteFile(filename, []byte(`invalid`), 0600))
	_, err = injectLocalMCP(frame, "a", "s")
	require.ErrorContains(t, err, "invalid local MCP")
}
