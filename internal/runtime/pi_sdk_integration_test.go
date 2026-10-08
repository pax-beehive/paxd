//go:build !windows

package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Run against an unpacked published adapter, with its dependencies installed.
// Only the SDK is a fixture; process launch and ACP initialization are real.
func TestPiSDKPublishedAdapter(t *testing.T) {
	adapter := os.Getenv("PI_ACP_E2E_ENTRY")
	if adapter == "" {
		t.Skip("set PI_ACP_E2E_ENTRY to the published adapter's dist/index.mjs")
	}
	t.Setenv("PI_ACP_SDK_ROOT", "")
	require.NoError(t, os.Unsetenv("PI_ACP_SDK_ROOT"))
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(bin, 0755))
	command := filepath.Join(bin, "pi-acp")
	require.NoError(t, os.Symlink(adapter, command))
	for _, version := range []string{"9.8.6", "9.8.7", "9.8.6"} {
		t.Run(version, func(t *testing.T) {
			sdk := filepath.Join(root, version)
			entry := writePiSDKFixture(t, sdk, version)
			require.NoError(t, os.WriteFile(filepath.Join(sdk, "package.json"), []byte(`{"name":"@earendil-works/pi-coding-agent","version":"`+version+`","type":"module","exports":"./sdk.js","bin":{"pi":"dist/cli.js"}}`), 0644))
			require.NoError(t, os.WriteFile(filepath.Join(sdk, "sdk.js"), []byte(piSDKIntegrationFixture), 0644))
			launcher := filepath.Join(bin, "pi")
			_ = os.Remove(launcher)
			require.NoError(t, os.Symlink(entry, launcher))
			proc, err := (ExecLocalACPProcessRunner{}).Start(t.Context(), LocalACPProcessSpec{Command: []string{command}, Env: map[string]string{"PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME": root}})
			require.NoError(t, err)
			t.Cleanup(func() { _ = proc.Terminate(context.Background()) })
			go func() { _, _ = io.Copy(io.Discard, proc.Stderr()) }()
			_, err = io.WriteString(proc.Stdin(), "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":1,\"clientCapabilities\":{}}}\n")
			require.NoError(t, err)
			response := make(chan []byte, 1)
			go func() {
				line, _ := bufio.NewReader(proc.Stdout()).ReadBytes('\n')
				response <- line
			}()
			select {
			case line := <-response:
				var message struct{ Result json.RawMessage }
				require.NoError(t, json.Unmarshal(line, &message), string(line))
				identity := implementationIdentityFromResult(message.Result)
				require.NotNil(t, identity)
				require.NotNil(t, identity.Runtime)
				require.Equal(t, "pi", identity.Runtime.Name)
				require.Equal(t, version, identity.Runtime.Version)
				require.NotNil(t, identity.ACPAgent)
				require.NotEqual(t, version, identity.ACPAgent.Version)
			case <-time.After(15 * time.Second):
				t.Fatal("published Pi adapter did not initialize")
			}
		})
	}
}

const piSDKIntegrationFixture = `
export async function createAgentSession() { throw new Error('fixture'); }
export function defineTool(tool) { return tool; }
export class SessionManager {
 static open() {} static forkFrom() {} static list() { return []; } static listAll() { return []; }
}
export class AgentSession {
 prompt() {} abort() {} subscribe() {} dispose() {} setModel() {} setThinkingLevel() {}
 getAvailableThinkingLevels() {} getContextUsage() {} getSessionStats() {} compact() {}
 exportToHtml() {} setSessionName() {} setAutoCompactionEnabled() {} setSteeringMode() {} setFollowUpMode() {}
}
`
