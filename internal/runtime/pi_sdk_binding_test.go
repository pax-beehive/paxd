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

func writePiSDKFixture(t *testing.T, root, version string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "dist"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"name":"@earendil-works/pi-coding-agent","version":"`+version+`","bin":{"pi":"dist/cli.js"}}`), 0644))
	entry := filepath.Join(root, "dist", "cli.js")
	require.NoError(t, os.WriteFile(entry, []byte("#!/bin/sh\nexit 0\n"), 0755))
	return entry
}

func TestPiSDKBindingFollowsLauncherUpgradeAndRollback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix launcher fixture")
	}
	// Restore the caller's environment while testing the unset default.
	t.Setenv("PI_ACP_SDK_ROOT", "")
	require.NoError(t, os.Unsetenv("PI_ACP_SDK_ROOT"))
	root := t.TempDir()
	oldRoot, newRoot := filepath.Join(root, "old"), filepath.Join(root, "new")
	oldEntry := writePiSDKFixture(t, oldRoot, "1.2.2")
	newEntry := writePiSDKFixture(t, newRoot, "1.2.3")
	launcher, adapter := filepath.Join(root, "pi"), filepath.Join(root, "pi-acp")
	require.NoError(t, os.WriteFile(adapter, []byte("#!/bin/sh\nprintf '%s' \"$PI_ACP_SDK_ROOT\"\n"), 0755))
	env := map[string]string{"PATH": root}
	spec := LocalACPProcessSpec{Command: []string{adapter}, Env: env}
	for _, entry := range []string{oldEntry, newEntry, oldEntry} {
		_ = os.Remove(launcher)
		require.NoError(t, os.Symlink(entry, launcher))
		resolved, followed, err := ResolvePiSDKBinding(spec)
		require.NoError(t, err)
		want, err := filepath.EvalSymlinks(filepath.Dir(filepath.Dir(entry)))
		require.NoError(t, err)
		require.Equal(t, want, resolved)
		require.Equal(t, launcher, followed)
		proc, err := (ExecLocalACPProcessRunner{}).Start(context.Background(), spec)
		require.NoError(t, err)
		output, err := io.ReadAll(proc.Stdout())
		require.NoError(t, err)
		require.NoError(t, proc.Wait())
		require.Equal(t, want, string(output))
		require.NotContains(t, env, "PI_ACP_SDK_ROOT")
	}
}

func TestPiSDKExplicitRootPrecedenceAndNoFallback(t *testing.T) {
	root := t.TempDir()
	writePiSDKFixture(t, root, "1.2.3")
	t.Setenv("PI_ACP_SDK_ROOT", root)
	got, launcher, err := ResolvePiSDKBinding(LocalACPProcessSpec{})
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Empty(t, launcher)
	for _, invalid := range []string{"", "relative", filepath.Join(root, "missing"), filepath.Join(root, "dist")} {
		_, _, err := ResolvePiSDKBinding(LocalACPProcessSpec{Env: map[string]string{"PI_ACP_SDK_ROOT": invalid, "PATH": root}})
		require.Error(t, err, invalid)
	}
	other := t.TempDir()
	writePiSDKFixture(t, other, "2.0.0")
	got, launcher, err = ResolvePiSDKBinding(LocalACPProcessSpec{Env: map[string]string{"PI_ACP_SDK_ROOT": other}})
	require.NoError(t, err)
	want, err = filepath.EvalSymlinks(other)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Empty(t, launcher)
	require.NoError(t, os.WriteFile(filepath.Join(other, "package.json"), []byte(`{"name":"wrong-package","version":"1.0.0"}`), 0644))
	_, _, err = ResolvePiSDKBinding(LocalACPProcessSpec{Env: map[string]string{"PI_ACP_SDK_ROOT": other}})
	require.ErrorContains(t, err, piSDKPackage)
}

func TestPiSDKDefaultRejectsWrappersAndMissingCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix launcher fixture")
	}
	t.Setenv("PI_ACP_SDK_ROOT", "")
	require.NoError(t, os.Unsetenv("PI_ACP_SDK_ROOT"))
	root := t.TempDir()
	spec := LocalACPProcessSpec{Env: map[string]string{"PATH": root}}
	_, _, err := ResolvePiSDKBinding(spec)
	require.Error(t, err)
	writePiSDKFixture(t, root, "1.2.3")
	require.NoError(t, os.WriteFile(filepath.Join(root, "pi"), []byte("#!/bin/sh\nexit 0\n"), 0755))
	_, _, err = ResolvePiSDKBinding(spec)
	require.ErrorContains(t, err, "does not match")
}
