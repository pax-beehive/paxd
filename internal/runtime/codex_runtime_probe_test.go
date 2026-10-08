package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexRuntimeProbeTrustBoundary(t *testing.T) {
	t.Run("Given known Codex ACP commands when checked then only direct and official npx adapters are trusted", func(t *testing.T) {
		assert.True(t, isTrustedCodexACPCommand([]string{"codex-acp"}))
		assert.True(t, isTrustedCodexACPCommand([]string{"/usr/local/bin/codex-acp", "--stdio"}))
		assert.True(t, isTrustedCodexACPCommand([]string{"npx", "-y", "@agentclientprotocol/codex-acp"}))
		assert.True(t, isTrustedCodexACPCommand([]string{"npx", "--yes", "@agentclientprotocol/codex-acp", "--stdio"}))
	})

	t.Run("Given arbitrary or lookalike ACP commands when checked then probing is rejected", func(t *testing.T) {
		commands := [][]string{
			nil,
			{"codex", "--acp"},
			{"sh", "-c", "codex-acp"},
			{"codex-acp-malicious"},
			{"npx", "-y", "@zed-industries/codex-acp"},
			{"npx", "--package", "malicious", "@agentclientprotocol/codex-acp"},
		}
		for _, command := range commands {
			assert.False(t, isTrustedCodexACPCommand(command), "command = %#v", command)
		}
	})
}

func TestCodexCLIVersionParsing(t *testing.T) {
	t.Run("Given the expected Codex output when parsed then only the safe version token is returned", func(t *testing.T) {
		version, err := parseCodexCLIVersion([]byte("  codex-cli 1.2.3-alpha.1+build_7\n"))

		require.NoError(t, err)
		assert.Equal(t, "1.2.3-alpha.1+build_7", version)
	})

	t.Run("Given unexpected or unsafe output when parsed then it is rejected without echoing output", func(t *testing.T) {
		outputs := [][]byte{
			[]byte("codex 1.2.3"),
			[]byte("codex-cli 1.2.3 extra"),
			[]byte("codex-cli ../../secret"),
			[]byte("token=do-not-leak"),
		}
		for _, output := range outputs {
			_, err := parseCodexCLIVersion(output)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), string(output))
		}
	})
}

func TestFallbackCodexRuntimeIdentity(t *testing.T) {
	t.Run("Given missing adapter runtime metadata when probing succeeds then a sanitized Codex runtime is supplied", func(t *testing.T) {
		prober := &recordingCodexRuntimeVersionProber{version: " 0.58.0 "}
		spec := LocalACPProcessSpec{
			Command:    []string{"codex-acp"},
			WorkingDir: "/workspace",
			Env:        map[string]string{"CODEX_PROFILE": "test"},
		}

		fallback := fallbackCodexRuntimeIdentity(
			context.Background(),
			[]byte(`{"agentInfo":{"name":"@agentclientprotocol/codex-acp","version":"1.1.7"}}`),
			spec,
			prober,
		)

		require.NotNil(t, fallback)
		assert.Equal(t, &ACPRuntimeImplementation{Name: "codex", Version: "0.58.0"}, fallback)
		calls := prober.Calls()
		require.Len(t, calls, 1)
		assert.Equal(t, spec.Command, calls[0].Command)
		assert.Equal(t, spec.WorkingDir, calls[0].WorkingDir)
		assert.Equal(t, spec.Env, calls[0].Env)
	})

	t.Run("Given adapter runtime metadata when projecting identity then the adapter value wins and the probe is not invoked", func(t *testing.T) {
		prober := &recordingCodexRuntimeVersionProber{version: "9.9.9"}
		result := []byte(`{
			"agentInfo":{"name":"@agentclientprotocol/codex-acp","version":"1.1.7"},
			"_meta":{"pax":{"runtime":{"name":"codex","version":"0.58.0","channel":"stable"}}}
		}`)

		fallback := fallbackCodexRuntimeIdentity(context.Background(), result, LocalACPProcessSpec{
			Command: []string{"codex-acp"},
		}, prober)
		identity := implementationIdentityWithRuntimeFallback(result, fallback)

		assert.Nil(t, fallback)
		require.NotNil(t, identity)
		require.NotNil(t, identity.Runtime)
		assert.Equal(t, "0.58.0", identity.Runtime.Version)
		assert.Equal(t, "stable", identity.Runtime.Channel)
		assert.Empty(t, prober.Calls())
	})

	t.Run("Given an arbitrary ACP command or unsafe returned version when probing then no runtime is invented", func(t *testing.T) {
		arbitrary := &recordingCodexRuntimeVersionProber{version: "0.58.0"}
		assert.Nil(t, fallbackCodexRuntimeIdentity(context.Background(), []byte(`{}`), LocalACPProcessSpec{
			Command: []string{"hermes", "acp"},
		}, arbitrary))
		assert.Empty(t, arbitrary.Calls())

		unsafe := &recordingCodexRuntimeVersionProber{version: "0.58.0 secret=value"}
		assert.Nil(t, fallbackCodexRuntimeIdentity(context.Background(), []byte(`{}`), LocalACPProcessSpec{
			Command: []string{"codex-acp"},
		}, unsafe))
		require.Len(t, unsafe.Calls(), 1)
	})
}

func TestExecCodexRuntimeVersionProberUsesACPWorkingDirectoryAndEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	t.Run("Given an isolated Codex executable when probed then cwd and environment match the ACP process", func(t *testing.T) {
		root := t.TempDir()
		binDir := filepath.Join(root, "bin")
		workingDir := filepath.Join(root, "workspace")
		require.NoError(t, os.MkdirAll(binDir, 0o755))
		require.NoError(t, os.MkdirAll(workingDir, 0o755))
		resolvedWorkingDir, err := filepath.EvalSymlinks(workingDir)
		require.NoError(t, err)
		script := filepath.Join(binDir, "codex")
		require.NoError(t, os.WriteFile(script, []byte(
			"#!/bin/sh\n"+
				"test \"$PWD\" = \"$EXPECTED_CODEX_PROBE_DIR\" || exit 11\n"+
				"test \"$CODEX_PROBE_MARKER\" = \"present\" || exit 12\n"+
				"printf 'codex-cli 0.99.0\\n'\n",
		), 0o755))
		version, err := (ExecCodexRuntimeVersionProber{}).ProbeCodexRuntimeVersion(
			context.Background(),
			LocalACPProcessSpec{
				Command:    []string{"codex-acp"},
				WorkingDir: workingDir,
				Env: map[string]string{
					"EXPECTED_CODEX_PROBE_DIR": resolvedWorkingDir,
					"CODEX_PROBE_MARKER":       "present",
					"CODEX_PATH":               script,
					"PATH":                     binDir,
				},
			},
		)

		require.NoError(t, err)
		assert.Equal(t, "0.99.0", version)
	})

	t.Run("Given a hanging Codex executable when probed then the short deadline stops its process group without leaking stderr", func(t *testing.T) {
		root := t.TempDir()
		script := filepath.Join(root, "codex")
		require.NoError(t, os.WriteFile(script, []byte(
			"#!/bin/sh\n"+
				"printf 'secret-provider-token' >&2\n"+
				"/bin/sleep 5\n",
		), 0o755))
		startedAt := time.Now()

		_, err := (ExecCodexRuntimeVersionProber{Timeout: 25 * time.Millisecond}).ProbeCodexRuntimeVersion(
			context.Background(),
			LocalACPProcessSpec{
				Command: []string{"codex-acp"},
				Env:     map[string]string{"PATH": root, "CODEX_PATH": script},
			},
		)

		require.Error(t, err)
		assert.Less(t, time.Since(startedAt), time.Second)
		assert.NotContains(t, err.Error(), "secret-provider-token")
	})
}

type recordingCodexRuntimeVersionProber struct {
	mu      sync.Mutex
	version string
	err     error
	calls   []LocalACPProcessSpec
}

func (p *recordingCodexRuntimeVersionProber) ProbeCodexRuntimeVersion(
	ctx context.Context,
	spec LocalACPProcessSpec,
) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cloned := LocalACPProcessSpec{
		Command:    append([]string(nil), spec.Command...),
		WorkingDir: spec.WorkingDir,
		Env:        make(map[string]string, len(spec.Env)),
	}
	for key, value := range spec.Env {
		cloned.Env[key] = value
	}
	p.calls = append(p.calls, cloned)
	if p.err != nil {
		return "", p.err
	}
	return p.version, nil
}

func (p *recordingCodexRuntimeVersionProber) Calls() []LocalACPProcessSpec {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]LocalACPProcessSpec(nil), p.calls...)
}

var _ CodexRuntimeVersionProber = (*recordingCodexRuntimeVersionProber)(nil)

func TestCodexRuntimeProbeUsesManagedPATHBinding(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "codex"), []byte("#!/bin/sh\necho 'codex-cli 99.99.99'\n"), 0755))
	version, err := (ExecCodexRuntimeVersionProber{}).ProbeCodexRuntimeVersion(t.Context(), LocalACPProcessSpec{
		Command: []string{"codex-acp"}, Env: map[string]string{"PATH": root, "CODEX_PATH": ""},
	})
	require.NoError(t, err)
	require.Equal(t, "99.99.99", version)
}

func TestCodexRuntimeProbeDoesNotUseAdapterDependencyWhenManaged(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node is required for adapter dependency resolution")
	}
	root := t.TempDir()
	adapter := filepath.Join(root, "node_modules", "@agentclientprotocol", "codex-acp")
	dependency := filepath.Join(adapter, "node_modules", "@openai", "codex", "bin")
	require.NoError(t, os.MkdirAll(dependency, 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "bin"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(adapter, "entry.js"), []byte("#!/usr/bin/env node\n"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dependency, "codex.js"), []byte("console.log('codex-cli 0.160.1');\n"), 0644))
	require.NoError(t, os.Symlink(filepath.Join(adapter, "entry.js"), filepath.Join(root, "bin", "codex-acp")))
	require.NoError(t, os.WriteFile(filepath.Join(root, "bin", "codex"), []byte("#!/bin/sh\necho 'codex-cli 99.99.99'\n"), 0755))
	version, err := (ExecCodexRuntimeVersionProber{Timeout: 10 * time.Second}).ProbeCodexRuntimeVersion(t.Context(), LocalACPProcessSpec{
		Command: []string{filepath.Join(root, "bin", "codex-acp")},
		Env:     map[string]string{"PATH": filepath.Join(root, "bin") + string(os.PathListSeparator) + os.Getenv("PATH"), "CODEX_PATH": ""},
	})
	require.NoError(t, err)
	require.Equal(t, "99.99.99", version)
}
