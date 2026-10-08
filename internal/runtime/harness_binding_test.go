package runtime

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestManagedACPReceivesAbsoluteHarnessPathAndFailsWithoutIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell fixture")
	}
	for _, item := range []struct{ adapter, bin, key string }{{"codex-acp", "codex", "CODEX_PATH"}, {"claude-agent-acp", "claude", "CLAUDE_CODE_EXECUTABLE"}} {
		t.Run(item.adapter, func(t *testing.T) {
			root := t.TempDir()
			adapter, native := filepath.Join(root, item.adapter), filepath.Join(root, item.bin)
			require.NoError(t, os.WriteFile(adapter, []byte("#!/bin/sh\nprintf '%s' \"$"+item.key+"\"\n"), 0755))
			require.NoError(t, os.WriteFile(native, []byte("#!/bin/sh\necho version\n"), 0755))
			env := map[string]string{"PATH": root, item.key: ""}
			spec := LocalACPProcessSpec{Command: []string{adapter}, Env: env}
			proc, err := (ExecLocalACPProcessRunner{}).Start(context.Background(), spec)
			require.NoError(t, err)
			output, err := io.ReadAll(proc.Stdout())
			require.NoError(t, err)
			require.Equal(t, native, string(output))
			require.NoError(t, proc.Wait())
			require.Empty(t, env[item.key], "shared connection environment must not be mutated")
			spec.Env[item.key] = filepath.Join(root, "missing-explicit")
			_, err = (ExecLocalACPProcessRunner{}).Start(context.Background(), spec)
			require.Error(t, err, "an invalid explicit path must not fall back")
			spec.Env[item.key] = ""
			require.NoError(t, os.Remove(native))
			_, err = (ExecLocalACPProcessRunner{}).Start(context.Background(), spec)
			require.Error(t, err, "a missing global launcher must not select an adapter dependency")
		})
	}
}

func TestClaudeVersionProbeReadsTheBoundExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell fixture")
	}
	root := t.TempDir()
	path := filepath.Join(root, "claude")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nprintf '2.1.99 (Claude Code)\\n'\n"), 0755))
	got := fallbackClaudeRuntimeIdentity(context.Background(), []byte(`{"agentInfo":{"version":"0.87.0"}}`), LocalACPProcessSpec{Command: []string{"claude-agent-acp"}, Env: map[string]string{"CLAUDE_CODE_EXECUTABLE": path, "PATH": root}})
	require.NotNil(t, got)
	require.Equal(t, "claude-code", got.Name)
	require.Equal(t, "2.1.99", got.Version)
}
