package paxlinstall

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/pax-beehive/paxd/internal/updater"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestProbeActualExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paxl")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nprintf '{\"version\":\"1.2.3\",\"commit\":\"abc\"}\\n'\n"), 0755))
	t.Setenv("PAXD_PAXL_COMMAND", path)
	got := Probe(t.Context())
	require.Equal(t, "installed", got.Status)
	require.Equal(t, "1.2.3", got.Version)
	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	require.Equal(t, resolved, got.Path)
	require.Equal(t, "abc", got.Commit)
}

func TestUpgradeVerifiesInstalledVersionAndKeepsOldBinary(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "paxl")
	old := []byte("#!/bin/sh\nprintf '{\"version\":\"1.2.2\"}\\n'\n")
	next := []byte("#!/bin/sh\nprintf '{\"version\":\"1.2.3\"}\\n'\n")
	require.NoError(t, os.WriteFile(path, old, 0755))
	t.Setenv("PAXD_PAXL_COMMAND", path)
	sum := sha256.Sum256(next)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/binary" {
			_, _ = w.Write(next)
			return
		}
		require.Equal(t, "paxl", r.URL.Query().Get("product"))
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"url": server.URL + "/binary", "version": "1.2.3", "size_bytes": len(next), "sha256": hex.EncodeToString(sum[:])}})
	}))
	defer server.Close()
	installer := &Installer{Options: updater.Options{ResolverURL: server.URL, StateDir: filepath.Join(root, "state")}}
	got, err := installer.Upgrade(t.Context(), "cmd1", "remote1", "1.2.3", "stable", func(string) {})
	require.NoError(t, err)
	require.Equal(t, "1.2.3", got.Version)
	require.Equal(t, "1.2.3", Probe(t.Context()).Version)
	preserved, err := os.ReadFile(filepath.Join(root, "state", "previous", "paxl-1.2.2"))
	require.NoError(t, err)
	require.Equal(t, old, preserved)
}

func TestUpgradeFailurePreservesInstalledBinary(t *testing.T) {
	for _, scenario := range []string{"checksum", "download", "wrong_version", "verification"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "paxl")
			old := []byte("#!/bin/sh\nprintf '{\"version\":\"1.2.2\"}\\n'\n")
			next := []byte("#!/bin/sh\nprintf '{\"version\":\"1.2.3\"}\\n'\n")
			if scenario == "wrong_version" {
				next = old
			}
			if scenario == "verification" {
				next = []byte("#!/bin/sh\ncase \"$0\" in */paxl) printf '{\"version\":\"9.9.9\"}\\n';; *) printf '{\"version\":\"1.2.3\"}\\n';; esac\n")
			}
			require.NoError(t, os.WriteFile(path, old, 0755))
			t.Setenv("PAXD_PAXL_COMMAND", path)
			sum := sha256.Sum256(next)
			checksum := hex.EncodeToString(sum[:])
			if scenario == "checksum" {
				checksum = hex.EncodeToString(make([]byte, 32))
			}
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/binary" {
					if scenario == "download" {
						w.WriteHeader(503)
						return
					}
					_, _ = w.Write(next)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"url": server.URL + "/binary", "version": "1.2.3", "size_bytes": len(next), "sha256": checksum}})
			}))
			defer server.Close()
			installer := &Installer{Options: updater.Options{ResolverURL: server.URL, StateDir: filepath.Join(root, "state")}}
			_, err := installer.Upgrade(t.Context(), "cmd", "remote", "1.2.3", "stable", func(string) {})
			require.Error(t, err)
			actual, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, old, actual)
		})
	}
}

func TestProbeMissingAndUnknown(t *testing.T) {
	t.Setenv("PAXD_PAXL_COMMAND", filepath.Join(t.TempDir(), "missing"))
	require.Equal(t, "missing", Probe(t.Context()).Status)
	path := filepath.Join(t.TempDir(), "paxl")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\necho not-json\n"), 0755))
	t.Setenv("PAXD_PAXL_COMMAND", path)
	require.Equal(t, "probe_failed", Probe(t.Context()).Status)
	t.Setenv("PAXD_PAXL_COMMAND", path+" wrapper")
	require.Contains(t, Probe(t.Context()).Error, "wrappers")
}

func TestUpgradeRejectsConcurrentWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paxl")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nprintf '{\"version\":\"1.2.2\"}\\n'\n"), 0755))
	t.Setenv("PAXD_PAXL_COMMAND", path)
	observed := Probe(t.Context())
	unlock, err := lockExecutable(observed.Path)
	require.NoError(t, err)
	defer unlock()
	installer := &Installer{}
	_, err = installer.Upgrade(t.Context(), "cmd", "remote", "1.2.3", "stable", func(string) {})
	require.ErrorContains(t, err, "busy")
}

func TestUpgradeRejectsPackageManagedExecutable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Cellar", "paxl", "1.2.2", "bin")
	require.NoError(t, os.MkdirAll(root, 0755))
	path := filepath.Join(root, "paxl")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nprintf '{\"version\":\"1.2.2\"}\\n'\n"), 0755))
	t.Setenv("PAXD_PAXL_COMMAND", path)
	_, err := (&Installer{}).Upgrade(t.Context(), "cmd", "remote", "1.2.3", "stable", func(string) {})
	require.ErrorContains(t, err, "package manager")
}
