package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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

func TestStageGivenRemoteWhenNoOverrideThenUsesRemoteResolver(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "paxd")
	oldBinary := []byte("#!/bin/sh\nprintf 'paxd 1.2.2\\n'\n")
	newBinary := []byte("#!/bin/sh\nprintf 'paxd 1.2.3\\n'\n")
	require.NoError(t, os.WriteFile(executable, oldBinary, 0o755))
	sum := sha256.Sum256(newBinary)
	server := newUpdateServer(t, "1.2.3", hex.EncodeToString(sum[:]), newBinary)
	defer server.Close()
	var resolvedRemoteID string
	update := New(Options{
		CurrentVersion: "1.2.2",
		ExecutablePath: func() (string, error) { return executable, nil },
		ResolverURLForRemote: func(_ context.Context, remoteID string) (string, error) {
			resolvedRemoteID = remoteID
			return server.URL + "/resolve", nil
		},
	})

	candidate, err := update.Stage(context.Background(), Request{
		RemoteID: "remote_home", Version: "1.2.3",
	})

	require.NoError(t, err)
	assert.Equal(t, "remote_home", resolvedRemoteID)
	update.Cleanup(candidate)
}

func TestStageGivenExplicitResolverOverrideThenDoesNotResolveRemoteURL(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "paxd")
	oldBinary := []byte("#!/bin/sh\nprintf 'paxd 1.2.2\\n'\n")
	newBinary := []byte("#!/bin/sh\nprintf 'paxd 1.2.3\\n'\n")
	require.NoError(t, os.WriteFile(executable, oldBinary, 0o755))
	sum := sha256.Sum256(newBinary)
	server := newUpdateServer(t, "1.2.3", hex.EncodeToString(sum[:]), newBinary)
	defer server.Close()
	remoteResolverCalled := false
	update := New(Options{
		ResolverURL:    server.URL + "/resolve",
		CurrentVersion: "1.2.2",
		ExecutablePath: func() (string, error) { return executable, nil },
		ResolverURLForRemote: func(context.Context, string) (string, error) {
			remoteResolverCalled = true
			return "", nil
		},
	})

	candidate, err := update.Stage(context.Background(), Request{
		RemoteID: "remote_home", Version: "1.2.3",
	})

	require.NoError(t, err)
	assert.False(t, remoteResolverCalled)
	update.Cleanup(candidate)
}

func TestStageGivenNoResolverConfigurationThenDoesNotUseLegacyHostedDefault(t *testing.T) {
	update := New(Options{CurrentVersion: "1.2.2"})

	_, err := update.Stage(context.Background(), Request{
		RemoteID: "remote_home", Version: "1.2.3",
	})

	require.ErrorContains(t, err, "update resolver is not configured")
}

func TestStageReportsRemoteResolverFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resolver ResolverURLForRemoteFunc
		want     string
	}{
		{
			name: "resolver error",
			resolver: func(context.Context, string) (string, error) {
				return "", errors.New("remote lookup failed")
			},
			want: "remote lookup failed",
		},
		{
			name: "empty resolver",
			resolver: func(context.Context, string) (string, error) {
				return "  ", nil
			},
			want: "is empty",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			update := New(Options{
				CurrentVersion:       "1.2.2",
				ResolverURLForRemote: tc.resolver,
			})

			_, err := update.Stage(context.Background(), Request{
				RemoteID: "remote_home", Version: "1.2.3",
			})

			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestResolverURLFromCloudAPIURL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		baseURL string
		want    string
	}{
		{
			name:    "origin",
			baseURL: "https://pax.home.example",
			want:    "https://pax.home.example/api/v1/public/paxd/download",
		},
		{
			name:    "trailing slash",
			baseURL: "https://pax.home.example/",
			want:    "https://pax.home.example/api/v1/public/paxd/download",
		},
		{
			name:    "path prefix",
			baseURL: "https://home.example/pax-api/",
			want:    "https://home.example/pax-api/api/v1/public/paxd/download",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolverURLFromCloudAPIURL(tc.baseURL)

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	_, err := ResolverURLFromCloudAPIURL("://invalid")
	require.ErrorContains(t, err, "parse cloud API URL")
	_, err = ResolverURLFromCloudAPIURL("ftp://pax.home.example")
	require.ErrorContains(t, err, "absolute HTTP(S)")
}

func TestResolveGivenManagerRedirectThenDoesNotFollowIt(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetCalls.Add(1)
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"url":        "https://objects.example.test/paxd",
				"sha256":     strings.Repeat("a", 64),
				"version":    "1.2.3",
				"size_bytes": 5,
			},
		}))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	secret := "resolver-query-secret"
	update := New(Options{HTTPClient: redirect.Client()})

	_, err := update.resolve(
		context.Background(),
		redirect.URL+"/resolve?access_token="+secret,
		DefaultTag,
	)

	require.ErrorContains(t, err, "HTTP 302")
	assert.Zero(t, targetCalls.Load(), "a manager resolver redirect must never be contacted")
	assert.NotContains(t, err.Error(), secret)
}

func TestDownloadGivenSignedURLRedirectThenDoesNotFollowIt(t *testing.T) {
	content := []byte("bytes")
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetCalls.Add(1)
		_, _ = w.Write(content)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	secret := "signed-download-secret"
	update := New(Options{HTTPClient: redirect.Client()})
	targetFile, err := os.Create(filepath.Join(t.TempDir(), "paxd.update"))
	require.NoError(t, err)
	defer targetFile.Close()

	_, _, err = update.download(context.Background(), artifact{
		URL:  redirect.URL + "/paxd?X-Amz-Signature=" + secret,
		Size: int64(len(content)),
	}, targetFile)

	require.ErrorContains(t, err, "HTTP 302")
	assert.Zero(t, targetCalls.Load(), "a signed binary redirect must never be contacted")
	assert.NotContains(t, err.Error(), secret)
}

func TestDownloadRedactsSignedURLFromTransportErrors(t *testing.T) {
	secretURL := "https://objects.example.test/paxd?X-Amz-Signature=top-secret"
	update := New(Options{HTTPClient: updateHTTPDoerFunc(
		func(*http.Request) (*http.Response, error) {
			return nil, &url.Error{
				Op:  http.MethodGet,
				URL: secretURL,
				Err: context.DeadlineExceeded,
			}
		},
	)})
	targetFile, err := os.Create(filepath.Join(t.TempDir(), "paxd.update"))
	require.NoError(t, err)
	defer targetFile.Close()

	_, _, err = update.download(context.Background(), artifact{URL: secretURL, Size: 5}, targetFile)

	require.Error(t, err)
	assert.Equal(t, "download paxd update failed", err.Error())
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotContains(t, err.Error(), secretURL)
	assert.NotContains(t, strings.ToLower(err.Error()), "x-amz-signature")
	assert.NotContains(t, err.Error(), "top-secret")
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

type updateHTTPDoerFunc func(*http.Request) (*http.Response, error)

func (f updateHTTPDoerFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}
