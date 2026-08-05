package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStageAndActivateVerifiedUpgrade(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "paxd")
	stateDir := filepath.Join(root, "updates")
	oldBinary := []byte("#!/bin/sh\nprintf 'paxd 1.2.2\\n'\n")
	newBinary := []byte("#!/bin/sh\nprintf 'paxd 1.2.3\\n'\n")
	require.NoError(t, os.WriteFile(executable, oldBinary, 0o755))

	sum := sha256.Sum256(newBinary)
	server := newUpdateServer(t, "1.2.3", hex.EncodeToString(sum[:]), newBinary)
	defer server.Close()

	update := New(Options{
		ResolverURL:    server.URL + "/resolve",
		CurrentVersion: "1.2.2",
		StateDir:       stateDir,
		ExecutablePath: func() (string, error) { return executable, nil },
		SmokeTimeout:   time.Second,
		Now: func() time.Time {
			return time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
		},
	})
	candidate, err := update.Stage(context.Background(), Request{
		CommandID: "cmd_upgrade", Version: "1.2.3", Tag: "stable",
	})
	require.NoError(t, err)
	require.Equal(t, "1.2.3", candidate.Version)
	require.Equal(t, int64(len(newBinary)), candidate.Size)

	beforeActivation, err := os.ReadFile(executable)
	require.NoError(t, err)
	require.Equal(t, oldBinary, beforeActivation, "staging must not change the running executable")

	record, err := update.Activate(candidate, "boot_old")
	require.NoError(t, err)
	require.Equal(t, "boot_old", record.RequestedBootID)
	activated, err := os.ReadFile(executable)
	require.NoError(t, err)
	require.Equal(t, newBinary, activated)
	previous, err := os.ReadFile(record.PreviousPath)
	require.NoError(t, err)
	require.Equal(t, oldBinary, previous)

	rawRecord, err := os.ReadFile(filepath.Join(stateDir, "activation.json"))
	require.NoError(t, err)
	var persisted struct {
		Phase string `json:"phase"`
	}
	require.NoError(t, json.Unmarshal(rawRecord, &persisted))
	require.Equal(t, "activated", persisted.Phase)
}

func TestStageRejectsChecksumMismatchWithoutChangingExecutable(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "paxd")
	oldBinary := []byte("#!/bin/sh\nprintf 'paxd 1.2.2\\n'\n")
	newBinary := []byte("#!/bin/sh\nprintf 'paxd 1.2.3\\n'\n")
	require.NoError(t, os.WriteFile(executable, oldBinary, 0o755))

	server := newUpdateServer(t, "1.2.3", strings.Repeat("0", 64), newBinary)
	defer server.Close()
	update := New(Options{
		ResolverURL:    server.URL + "/resolve",
		CurrentVersion: "1.2.2",
		StateDir:       filepath.Join(root, "updates"),
		ExecutablePath: func() (string, error) { return executable, nil },
	})

	_, err := update.Stage(context.Background(), Request{
		CommandID: "cmd_bad_sha", Version: "1.2.3",
	})
	require.ErrorContains(t, err, "does not match expected")
	current, readErr := os.ReadFile(executable)
	require.NoError(t, readErr)
	require.Equal(t, oldBinary, current)
	staged, globErr := filepath.Glob(filepath.Join(root, ".paxd.update-*"))
	require.NoError(t, globErr)
	require.Empty(t, staged)
}

func TestStageRejectsResolverVersionMismatchBeforeDownload(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "paxd")
	require.NoError(t, os.WriteFile(
		executable,
		[]byte("#!/bin/sh\nprintf 'paxd 1.2.2\\n'\n"),
		0o755,
	))
	artifact := []byte("#!/bin/sh\nprintf 'paxd 1.2.4\\n'\n")
	sum := sha256.Sum256(artifact)
	server := newUpdateServer(t, "1.2.4", hex.EncodeToString(sum[:]), artifact)
	defer server.Close()
	update := New(Options{
		ResolverURL:    server.URL + "/resolve",
		CurrentVersion: "1.2.2",
		ExecutablePath: func() (string, error) { return executable, nil },
	})

	_, err := update.Stage(context.Background(), Request{Version: "1.2.3"})
	require.ErrorContains(t, err, "does not match requested")
}

func newUpdateServer(t *testing.T, version, checksum string, artifact []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var server *httptest.Server
	mux.HandleFunc("/resolve", func(w http.ResponseWriter, r *http.Request) {
		require.NotEmpty(t, r.URL.Query().Get("platform"))
		require.NotEmpty(t, r.URL.Query().Get("tags"))
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"url":        server.URL + "/artifact",
				"sha256":     checksum,
				"version":    version,
				"size_bytes": len(artifact),
			},
		}))
	})
	mux.HandleFunc("/artifact", func(w http.ResponseWriter, _ *http.Request) {
		_, err := w.Write(artifact)
		require.NoError(t, err)
	})
	server = httptest.NewServer(mux)
	return server
}
