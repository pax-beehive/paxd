package acpcommand

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Exercise the real npm resolver against a private, local fixture registry.
// A newer release must not trigger another binary download after installation.
func TestNPMFallbackReusesInstalledVersionAfterNewRelease(t *testing.T) {
	if testing.Short() {
		t.Skip("real npm integration")
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm unavailable")
	}
	const pkg = "@agentclientprotocol/claude-agent-acp"
	var latest atomic.Int32
	latest.Store(1)
	var downloads atomic.Int32
	var registryURL string
	tarball := func(version string) []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		manifest, marshalErr := json.Marshal(map[string]any{"name": pkg, "version": version, "bin": map[string]string{"claude-agent-acp": "cli.js"}})
		require.NoError(t, marshalErr)
		for _, file := range []struct {
			name, body string
			mode       int64
		}{
			{"package/package.json", string(manifest), 0644},
			{"package/cli.js", "#!/usr/bin/env node\nconsole.log('" + version + "')\n", 0755},
		} {
			require.NoError(t, tw.WriteHeader(&tar.Header{Name: file.name, Mode: file.mode, Size: int64(len(file.body))}))
			_, writeErr := tw.Write([]byte(file.body))
			require.NoError(t, writeErr)
		}
		require.NoError(t, tw.Close())
		require.NoError(t, gz.Close())
		return buf.Bytes()
	}
	v1, v2 := tarball("1.0.0"), tarball("2.0.0")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".tgz") {
			downloads.Add(1)
			if strings.Contains(r.URL.Path, "2.0.0") {
				_, _ = w.Write(v2)
			} else {
				_, _ = w.Write(v1)
			}
			return
		}
		versions := map[string]any{}
		for i := int32(1); i <= latest.Load(); i++ {
			version := fmt.Sprintf("%d.0.0", i)
			versions[version] = map[string]any{"name": pkg, "version": version, "bin": map[string]string{"claude-agent-acp": "cli.js"}, "dist": map[string]string{"tarball": registryURL + "/adapter-" + version + ".tgz"}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"name": pkg, "dist-tags": map[string]string{"latest": fmt.Sprintf("%d.0.0", latest.Load())}, "versions": versions})
	}))
	defer server.Close()
	registryURL = server.URL
	root := t.TempDir()
	run := func(command []string) string {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, command[0], command[1:]...)
		cmd.Dir = root
		// Isolate npm config and cache; never touch the developer's installed packages.
		cmd.Env = append(os.Environ(), "npm_config_cache="+filepath.Join(root, "cache"), "npm_config_registry="+server.URL,
			"npm_config_userconfig="+filepath.Join(root, "npmrc"), "npm_config_globalconfig="+filepath.Join(root, "global-npmrc"),
			"npm_config_audit=false", "npm_config_fund=false", "npm_config_update_notifier=false", "npm_config_offline=false")
		output, runErr := cmd.CombinedOutput()
		require.NoError(t, runErr, "%s", output)
		return strings.TrimSpace(string(output))
	}
	command := PreferCached([]string{npm, "exec", "--yes", pkg})
	assert.Equal(t, "1.0.0", run(command))
	assert.EqualValues(t, 1, downloads.Load())
	latest.Store(2)
	for range 3 {
		assert.Equal(t, "1.0.0", run(command))
	}
	assert.EqualValues(t, 1, downloads.Load(), "restarts must not download the new binary")
	// Explicit upgrades still work, proving the registry really has a new release.
	assert.Equal(t, "2.0.0", run([]string{npm, "exec", "--yes", pkg + "@latest"}))
	assert.EqualValues(t, 2, downloads.Load())
}
