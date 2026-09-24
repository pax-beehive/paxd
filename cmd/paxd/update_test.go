package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPaxdUpdateCheckReportsAvailableVersion(t *testing.T) {
	restore := stubPaxdUpdateHTTPClient(func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "/api/v1/public/paxd/download", req.URL.Path)
		assert.Equal(t, "darwin/arm64", req.URL.Query().Get("platform"))
		return paxdUpdateJSONResponse(`{
			"data": {
				"url": "https://download.test/paxd",
				"sha256": "abc123",
				"size_bytes": 42,
				"version": "0.2.0"
			}
		}`), nil
	})
	defer restore()
	oldVersion := version
	version = "0.1.0"
	defer func() { version = oldVersion }()
	var stdout bytes.Buffer
	app := newApp()
	app.Writer = &stdout

	err := app.Run(context.Background(), []string{
		"paxd", "update", "check",
		"--resolver-url", defaultPaxdUpdateResolverURL,
		"--platform", "darwin/arm64",
		"--format", "json",
	})

	require.NoError(t, err)
	assert.Contains(t, stdout.String(), `"current_version":"0.1.0"`)
	assert.Contains(t, stdout.String(), `"latest_version":"0.2.0"`)
	assert.Contains(t, stdout.String(), `"update_available":true`)
	assert.NotContains(t, stdout.String(), "download_url")
	assert.NotContains(t, stdout.String(), "https://download.test/paxd")
}

func TestDefaultPaxdUpdateResolverUsesPaxWorkspaceHostedManager(t *testing.T) {
	assert.Equal(
		t,
		"https://api.paxworkspace.net/api/v1/public/paxd/download",
		defaultPaxdUpdateResolverURL,
	)
}

func TestPaxdUpdateCheckUsesSelectedLocalRemoteWhenResolverIsNotOverridden(t *testing.T) {
	restoreResolver := stubPaxdUpdateRemoteResolver(
		func(_ context.Context, remoteID string) (string, error) {
			assert.Equal(t, "home", remoteID)
			return "https://home.example/base/api/v1/public/paxd/download", nil
		},
	)
	defer restoreResolver()
	restoreClient := stubPaxdUpdateHTTPClient(func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "home.example", req.URL.Host)
		assert.Equal(t, "/base/api/v1/public/paxd/download", req.URL.Path)
		return paxdUpdateJSONResponse(`{
			"data": {
				"url": "https://objects.home.example/paxd",
				"sha256": "abc123",
				"size_bytes": 42,
				"version": "0.2.0"
			}
		}`), nil
	})
	defer restoreClient()
	oldVersion := version
	version = "0.1.0"
	defer func() { version = oldVersion }()

	err := newApp().Run(context.Background(), []string{
		"paxd", "update", "check",
		"--remote", "home",
		"--platform", "linux/amd64",
	})

	require.NoError(t, err)
}

func TestPaxdUpdateCheckExplicitResolverOverridesSelectedLocalRemote(t *testing.T) {
	remoteResolverCalled := false
	restoreResolver := stubPaxdUpdateRemoteResolver(
		func(context.Context, string) (string, error) {
			remoteResolverCalled = true
			return "", errors.New("must not resolve local remote")
		},
	)
	defer restoreResolver()
	restoreClient := stubPaxdUpdateHTTPClient(func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "override.example", req.URL.Host)
		return paxdUpdateJSONResponse(`{
			"data": {
				"url": "https://objects.example/paxd",
				"sha256": "abc123",
				"size_bytes": 42,
				"version": "0.2.0"
			}
		}`), nil
	})
	defer restoreClient()
	oldVersion := version
	version = "0.1.0"
	defer func() { version = oldVersion }()

	err := newApp().Run(context.Background(), []string{
		"paxd", "update", "check",
		"--remote", "home",
		"--resolver-url", "https://override.example/resolve",
		"--platform", "linux/amd64",
	})

	require.NoError(t, err)
	assert.False(t, remoteResolverCalled)
}

func TestDefaultPaxdUpdateRemoteResolverUsesPersistedDefaultRemote(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "paxd.db")
	t.Setenv("PAXD_DB_PATH", databasePath)
	store, err := daemonstore.OpenSQLite(databasePath)
	require.NoError(t, err)
	require.NoError(t, store.Migrate(context.Background()))
	_, err = store.CreateRemote(context.Background(), control.CreateRemoteCommand{
		Remote: control.Remote{
			ID: "default", Name: "Home", CloudAPIURL: "https://home.example/base/",
		},
	})
	require.NoError(t, err)
	require.NoError(t, store.Close())

	resolverURL, err := defaultPaxdUpdateResolverForRemote(
		context.Background(), "default",
	)

	require.NoError(t, err)
	assert.Equal(
		t,
		"https://home.example/base/api/v1/public/paxd/download",
		resolverURL,
	)
}

func TestDefaultPaxdUpdateRemoteResolverFallsBackOnlyForUnconfiguredDefault(t *testing.T) {
	t.Setenv("PAXD_DB_PATH", filepath.Join(t.TempDir(), "missing.db"))

	resolverURL, err := defaultPaxdUpdateResolverForRemote(
		context.Background(), "default",
	)
	require.NoError(t, err)
	assert.Equal(t, defaultPaxdUpdateResolverURL, resolverURL)

	_, err = defaultPaxdUpdateResolverForRemote(context.Background(), "home")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `local remote "home" does not exist`)
}

func TestPaxdUpdateDownloadsVerifiesAndReplacesExecutable(t *testing.T) {
	binary := []byte("new paxd binary")
	sum := sha256.Sum256(binary)
	sha := hex.EncodeToString(sum[:])
	restoreClient := stubPaxdUpdateHTTPClient(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/v1/public/paxd/download":
			return paxdUpdateJSONResponse(fmt.Sprintf(`{
				"data": {
					"url": "https://download.test/paxd",
					"sha256": "%s",
					"size_bytes": %d,
					"version": "0.2.0"
				}
			}`, sha, len(binary))), nil
		case "/paxd":
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(binary))}, nil
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(bytes.NewReader(nil))}, nil
		}
	})
	defer restoreClient()
	executable := filepath.Join(t.TempDir(), "paxd")
	require.NoError(t, os.WriteFile(executable, []byte("old paxd binary"), 0o755))
	restoreExecutable := stubPaxdExecutablePath(executable)
	defer restoreExecutable()
	oldVersion := version
	version = "0.1.0"
	defer func() { version = oldVersion }()
	var stdout bytes.Buffer
	app := newApp()
	app.Writer = &stdout

	err := app.Run(context.Background(), []string{
		"paxd", "update",
		"--resolver-url", defaultPaxdUpdateResolverURL,
		"--platform", "linux/amd64",
		"--format", "json",
	})

	require.NoError(t, err)
	assert.Contains(t, stdout.String(), `"updated":true`)
	raw, err := os.ReadFile(executable)
	require.NoError(t, err)
	assert.Equal(t, binary, raw)
	info, err := os.Stat(executable)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestPaxdUpdateSkipsReplacementWhenAlreadyCurrent(t *testing.T) {
	restore := stubPaxdUpdateHTTPClient(func(req *http.Request) (*http.Response, error) {
		return paxdUpdateJSONResponse(`{
			"data": {
				"url": "https://download.test/paxd",
				"sha256": "abc123",
				"size_bytes": 42,
				"version": "0.2.0"
			}
		}`), nil
	})
	defer restore()
	oldVersion := version
	version = "0.2.0"
	defer func() { version = oldVersion }()
	var stdout bytes.Buffer
	app := newApp()
	app.Writer = &stdout

	err := app.Run(context.Background(), []string{
		"paxd", "update",
		"--resolver-url", defaultPaxdUpdateResolverURL,
		"--platform", "linux/amd64",
	})

	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "paxd is already up to date")
}

func TestPaxdUpdateSkipsReplacementWithJSONOutput(t *testing.T) {
	restore := stubPaxdUpdateHTTPClient(func(req *http.Request) (*http.Response, error) {
		return paxdUpdateJSONResponse(`{
			"data": {
				"url": "https://download.test/paxd",
				"sha256": "abc123",
				"size_bytes": 42,
				"version": "0.2.0"
			}
		}`), nil
	})
	defer restore()
	oldVersion := version
	version = "0.2.0"
	defer func() { version = oldVersion }()
	var stdout bytes.Buffer
	app := newApp()
	app.Writer = &stdout

	err := app.Run(context.Background(), []string{
		"paxd", "update",
		"--resolver-url", defaultPaxdUpdateResolverURL,
		"--platform", "linux/amd64",
		"--format", "json",
	})

	require.NoError(t, err)
	assert.Contains(t, stdout.String(), `"updated":false`)
}

func TestPaxdUpdateDoesNotDowngradeAheadVersion(t *testing.T) {
	restore := stubPaxdUpdateHTTPClient(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/paxd" {
			t.Fatalf("did not expect download for ahead version")
		}
		return paxdUpdateJSONResponse(`{
			"data": {
				"url": "https://download.test/paxd",
				"sha256": "abc123",
				"size_bytes": 42,
				"version": "0.2.0"
			}
		}`), nil
	})
	defer restore()
	oldVersion := version
	version = "0.3.0"
	defer func() { version = oldVersion }()
	var stdout bytes.Buffer
	app := newApp()
	app.Writer = &stdout

	err := app.Run(context.Background(), []string{
		"paxd", "update",
		"--resolver-url", defaultPaxdUpdateResolverURL,
		"--platform", "linux/amd64",
	})

	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "No paxd update applied")
	assert.Contains(t, stdout.String(), "ahead")
}

func TestPaxdUpdateSkipsDevelopmentVersion(t *testing.T) {
	restore := stubPaxdUpdateHTTPClient(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/paxd" {
			t.Fatalf("did not expect download for development version")
		}
		return paxdUpdateJSONResponse(`{
			"data": {
				"url": "https://download.test/paxd",
				"sha256": "abc123",
				"size_bytes": 42,
				"version": "0.4.0"
			}
		}`), nil
	})
	defer restore()
	oldVersion := version
	version = "0.3.0-dev"
	defer func() { version = oldVersion }()
	var stdout bytes.Buffer
	app := newApp()
	app.Writer = &stdout

	err := app.Run(context.Background(), []string{
		"paxd", "update",
		"--resolver-url", defaultPaxdUpdateResolverURL,
		"--platform", "linux/amd64",
	})

	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "No paxd update applied")
	assert.Contains(t, stdout.String(), "development")
}

func TestPaxdUpdateRejectsChecksumMismatch(t *testing.T) {
	restore := stubPaxdUpdateHTTPClient(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/v1/public/paxd/download":
			return paxdUpdateJSONResponse(`{
				"data": {
					"url": "https://download.test/paxd",
					"sha256": "bad",
					"size_bytes": 4,
					"version": "0.2.0"
				}
			}`), nil
		case "/paxd":
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader([]byte("paxd")))}, nil
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(bytes.NewReader(nil))}, nil
		}
	})
	defer restore()
	oldVersion := version
	version = "0.1.0"
	defer func() { version = oldVersion }()

	err := newApp().Run(context.Background(), []string{
		"paxd", "update",
		"--resolver-url", defaultPaxdUpdateResolverURL,
		"--platform", "linux/amd64",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "verify update")
	assert.Contains(t, err.Error(), "sha256")
}

func TestPaxdUpdateRendersTextCheck(t *testing.T) {
	restore := stubPaxdUpdateHTTPClient(func(req *http.Request) (*http.Response, error) {
		return paxdUpdateJSONResponse(`{
			"data": {
				"url": "https://download.test/paxd",
				"sha256": "abc123",
				"size_bytes": 42,
				"version": "0.2.0"
			}
		}`), nil
	})
	defer restore()
	oldVersion := version
	version = "0.1.0"
	defer func() { version = oldVersion }()
	var stdout bytes.Buffer
	app := newApp()
	app.Writer = &stdout

	err := app.Run(context.Background(), []string{
		"paxd", "update", "check",
		"--resolver-url", defaultPaxdUpdateResolverURL,
		"--platform", "linux/amd64",
	})

	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "Current: 0.1.0")
	assert.Contains(t, stdout.String(), "Latest:  0.2.0")
	assert.Contains(t, stdout.String(), "update_available")
}

func TestPaxdUpdateNormalizesGCSArtifactURL(t *testing.T) {
	got := normalizePaxdArtifactURL("gs://pax-tech-bucket/paxd/releases/0.2.0/paxd")

	assert.Equal(t, "https://storage.googleapis.com/pax-tech-bucket/paxd/releases/0.2.0/paxd", got)
}

func TestResolvePaxdUpdateArtifactRejectsMissingSHA(t *testing.T) {
	restore := stubPaxdUpdateHTTPClient(func(req *http.Request) (*http.Response, error) {
		return paxdUpdateJSONResponse(`{
			"data": {
				"url": "https://download.test/paxd",
				"version": "0.2.0"
			}
		}`), nil
	})
	defer restore()

	_, err := resolvePaxdUpdateArtifact(
		context.Background(),
		defaultPaxdUpdateResolverURL,
		"linux/amd64",
		"stable",
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "sha256")
}

func TestResolvePaxdUpdateArtifactRejectsMissingDownloadURL(t *testing.T) {
	restore := stubPaxdUpdateHTTPClient(func(req *http.Request) (*http.Response, error) {
		return paxdUpdateJSONResponse(`{
			"data": {
				"sha256": "abc123",
				"version": "0.2.0"
			}
		}`), nil
	})
	defer restore()

	_, err := resolvePaxdUpdateArtifact(
		context.Background(),
		defaultPaxdUpdateResolverURL,
		"linux/amd64",
		"stable",
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "download URL")
}

func TestPaxdUpdateRejectsInvalidLatestVersion(t *testing.T) {
	restore := stubPaxdUpdateHTTPClient(func(req *http.Request) (*http.Response, error) {
		return paxdUpdateJSONResponse(`{
			"data": {
				"url": "https://download.test/paxd",
				"sha256": "abc123",
				"size_bytes": 42,
				"version": "latest"
			}
		}`), nil
	})
	defer restore()
	oldVersion := version
	version = "0.1.0"
	defer func() { version = oldVersion }()

	err := newApp().Run(context.Background(), []string{
		"paxd", "update", "check",
		"--resolver-url", defaultPaxdUpdateResolverURL,
		"--platform", "linux/amd64",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse latest version")
}

func TestComparePaxdUpdateVersions(t *testing.T) {
	status, available, err := comparePaxdUpdateVersions("0.1.0", "0.2.0")
	require.NoError(t, err)
	assert.Equal(t, paxdUpdateStatusAvailable, status)
	assert.True(t, available)

	status, available, err = comparePaxdUpdateVersions("0.2.0", "0.2.0")
	require.NoError(t, err)
	assert.Equal(t, paxdUpdateStatusUpToDate, status)
	assert.False(t, available)

	status, available, err = comparePaxdUpdateVersions("0.3.0", "0.2.0")
	require.NoError(t, err)
	assert.Equal(t, paxdUpdateStatusAhead, status)
	assert.False(t, available)

	status, available, err = comparePaxdUpdateVersions("0.3.0-dev", "0.4.0")
	require.NoError(t, err)
	assert.Equal(t, paxdUpdateStatusDevelopment, status)
	assert.False(t, available)
}

func TestDownloadPaxdUpdateRejectsSizeMismatch(t *testing.T) {
	restore := stubPaxdUpdateHTTPClient(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader([]byte("paxd")))}, nil
	})
	defer restore()

	_, err := downloadPaxdUpdate(context.Background(), "https://download.test/paxd", 5)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "download size")
}

func TestDownloadPaxdUpdateReturnsRequestError(t *testing.T) {
	restore := stubPaxdUpdateHTTPClient(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("network unavailable")
	})
	defer restore()

	_, err := downloadPaxdUpdate(context.Background(), "https://download.test/paxd", 0)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "request download")
	assert.NotContains(t, err.Error(), "network unavailable")
}

func TestResolvePaxdUpdateArtifactGivenManagerRedirectThenDoesNotFollowIt(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetCalls.Add(1)
		_, _ = io.WriteString(w, `{"data":{"url":"https://objects.example.test/paxd","sha256":"abc123","size_bytes":5,"version":"1.2.3"}}`)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	restore := stubPaxdUpdateHTTPDoer(redirect.Client())
	defer restore()
	secret := "resolver-query-secret"

	_, err := resolvePaxdUpdateArtifact(
		context.Background(),
		redirect.URL+"/resolve?access_token="+secret,
		"linux/amd64",
		"stable",
	)

	require.ErrorContains(t, err, "HTTP 302")
	assert.Zero(t, targetCalls.Load(), "a manager resolver redirect must never be contacted")
	assert.NotContains(t, err.Error(), secret)
}

func TestDownloadPaxdUpdateGivenSignedURLRedirectThenDoesNotFollowIt(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetCalls.Add(1)
		_, _ = io.WriteString(w, "paxd")
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	restore := stubPaxdUpdateHTTPDoer(redirect.Client())
	defer restore()
	secret := "signed-download-secret"

	_, err := downloadPaxdUpdate(
		context.Background(),
		redirect.URL+"/paxd?X-Amz-Signature="+secret,
		4,
	)

	require.ErrorContains(t, err, "HTTP 302")
	assert.Zero(t, targetCalls.Load(), "a signed binary redirect must never be contacted")
	assert.NotContains(t, err.Error(), secret)
}

func TestDownloadPaxdUpdateRedactsSignedURLFromTransportErrors(t *testing.T) {
	secretURL := "https://objects.example.test/paxd?X-Amz-Signature=top-secret"
	restore := stubPaxdUpdateHTTPClient(func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{
			Op:  http.MethodGet,
			URL: secretURL,
			Err: context.DeadlineExceeded,
		}
	})
	defer restore()

	_, err := downloadPaxdUpdate(context.Background(), secretURL, 4)

	require.Error(t, err)
	assert.Equal(t, "request download failed", err.Error())
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotContains(t, err.Error(), secretURL)
	assert.NotContains(t, strings.ToLower(err.Error()), "x-amz-signature")
	assert.NotContains(t, err.Error(), "top-secret")
}

func TestReplacePaxdExecutableRejectsInvalidPath(t *testing.T) {
	err := replacePaxdExecutable("", []byte("paxd"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "executable path")

	err = replacePaxdExecutable(filepath.Join(t.TempDir(), "missing"), []byte("paxd"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stat executable")
}

func TestPaxdUpdateRenderRejectsUnsupportedFormats(t *testing.T) {
	err := renderPaxdUpdateCheck(io.Discard, &paxdUpdateCheckResponse{}, "yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported format")

	err = renderPaxdApplyUpdate(io.Discard, &paxdApplyUpdateResponse{}, "yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported format")
}

func TestResolvePaxdUpdateArtifactReturnsHTTPError(t *testing.T) {
	restore := stubPaxdUpdateHTTPClient(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})
	defer restore()

	_, err := resolvePaxdUpdateArtifact(context.Background(), defaultPaxdUpdateResolverURL, "linux/amd64", "stable")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolver returned HTTP 502")
}

func TestDownloadPaxdUpdateReturnsHTTPError(t *testing.T) {
	restore := stubPaxdUpdateHTTPClient(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})
	defer restore()

	_, err := downloadPaxdUpdate(context.Background(), "https://download.test/paxd", 0)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "download returned HTTP 502")
}

func TestPaxdContextWithTimeoutRejectsInvalidDuration(t *testing.T) {
	_, _, err := paxdContextWithTimeout(context.Background(), "forever")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse timeout")
}

func TestRenderPaxdApplyUpdateWritesUpdatedText(t *testing.T) {
	var stdout bytes.Buffer

	err := renderPaxdApplyUpdate(&stdout, &paxdApplyUpdateResponse{
		CurrentVersion: "0.1.0",
		LatestVersion:  "0.2.0",
		Updated:        true,
		Path:           "/usr/local/bin/paxd",
	}, "text")

	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "Updated paxd 0.1.0 -> 0.2.0")
	assert.Contains(t, stdout.String(), "/usr/local/bin/paxd")
}

func stubPaxdUpdateHTTPClient(fn func(*http.Request) (*http.Response, error)) func() {
	previous := paxdUpdateHTTPClient
	paxdUpdateHTTPClient = roundTripFunc(fn)
	return func() {
		paxdUpdateHTTPClient = previous
	}
}

func stubPaxdUpdateHTTPDoer(client paxdUpdateHTTPDoer) func() {
	previous := paxdUpdateHTTPClient
	paxdUpdateHTTPClient = client
	return func() {
		paxdUpdateHTTPClient = previous
	}
}

func stubPaxdUpdateRemoteResolver(
	fn func(context.Context, string) (string, error),
) func() {
	previous := paxdUpdateResolverForRemote
	paxdUpdateResolverForRemote = fn
	return func() {
		paxdUpdateResolverForRemote = previous
	}
}

func stubPaxdExecutablePath(path string) func() {
	previous := paxdExecutablePath
	paxdExecutablePath = func() (string, error) {
		return path, nil
	}
	return func() {
		paxdExecutablePath = previous
	}
}

func paxdUpdateJSONResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}
