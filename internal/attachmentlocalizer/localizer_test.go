package attachmentlocalizer_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"time"

	"github.com/pax-beehive/paxd/internal/attachmentlocalizer"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalizerGivenPartialFileWhenEnsuredThenItResumesAndPublishesReady(t *testing.T) {
	content := "hello world"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "bytes=6-", r.Header.Get("Range"))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 6-10/%d", len(content)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(content[6:]))
	}))
	defer server.Close()

	root := t.TempDir()
	dir := filepath.Join(root, "local", "att_1")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "content.part"), []byte(content[:6]), 0o600))

	states := make(chan control.AttachmentLocalState, 16)
	localizer := attachmentlocalizer.New(attachmentlocalizer.Options{
		Context: context.Background(),
		RootDir: root,
		OnState: func(state control.AttachmentLocalState) { states <- state },
	})
	require.NoError(t, localizer.Ensure(context.Background(), control.Source{Kind: control.SourceLocal}, control.EnsureAttachmentLocalCommand{
		Attachment: control.AttachmentDescriptor{
			AttachmentID: "att_1",
			Filename:     "notes.txt",
			SizeBytes:    int64(len(content)),
		},
		Download: control.AttachmentDownloadTicket{URL: server.URL},
	}))

	deadline := time.After(5 * time.Second)
	var ready control.AttachmentLocalState
	for ready.State != control.AttachmentLocalReady {
		select {
		case state := <-states:
			ready = state
		case <-deadline:
			t.Fatal("timed out waiting for ready state")
		}
	}
	assert.Equal(t, int64(len(content)), ready.BytesDownloaded)
	assert.True(t, strings.HasPrefix(ready.LocalURI, "file://"))

	items, err := localizer.Status(context.Background(), control.Source{Kind: control.SourceLocal}, []string{"att_1"})
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, control.AttachmentLocalReady, items[0].State)
	path := strings.TrimPrefix(items[0].LocalURI, "file://")
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, content, string(got))
}

func TestLocalizerGivenRestartWhenStatusQueriedThenReadyManifestIsAuthoritative(t *testing.T) {
	content := "persisted"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(content))
	}))
	defer server.Close()

	root := t.TempDir()
	first := attachmentlocalizer.New(attachmentlocalizer.Options{Context: context.Background(), RootDir: root})
	require.NoError(t, first.Ensure(context.Background(), control.Source{Kind: control.SourceLocal}, control.EnsureAttachmentLocalCommand{
		Attachment: control.AttachmentDescriptor{
			AttachmentID: "att_restart",
			Filename:     "persisted.txt",
			SizeBytes:    int64(len(content)),
		},
		Download: control.AttachmentDownloadTicket{URL: server.URL},
	}))
	require.Eventually(t, func() bool {
		items, err := first.Status(context.Background(), control.Source{Kind: control.SourceLocal}, []string{"att_restart"})
		return err == nil && len(items) == 1 && items[0].State == control.AttachmentLocalReady
	}, 5*time.Second, 10*time.Millisecond)

	restarted := attachmentlocalizer.New(attachmentlocalizer.Options{Context: context.Background(), RootDir: root})
	items, err := restarted.Status(context.Background(), control.Source{Kind: control.SourceLocal}, []string{"att_restart", "missing"})
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, control.AttachmentLocalReady, items[0].State)
	assert.Equal(t, control.AttachmentLocalUnknown, items[1].State)
}

func TestLocalizerGivenSignedURLRedirectThenDoesNotFollowIt(t *testing.T) {
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
	secret := "top-secret-attachment-query"
	states := make(chan control.AttachmentLocalState, 16)
	localizer := attachmentlocalizer.New(attachmentlocalizer.Options{
		Context: context.Background(),
		RootDir: t.TempDir(),
		Client:  redirect.Client(),
		OnState: func(state control.AttachmentLocalState) { states <- state },
	})

	require.NoError(t, localizer.Ensure(context.Background(), control.Source{Kind: control.SourceLocal}, integrityCommand(
		"att_redirect",
		redirect.URL+"/object?X-Amz-Signature="+secret,
		int64(len(content)),
		"",
	)))
	terminal := waitForAttachmentTerminal(t, states)

	assert.Equal(t, control.AttachmentLocalFailed, terminal.State)
	assert.ErrorContains(t, errors.New(terminal.ErrorMessage), "HTTP 302")
	assert.Zero(t, targetCalls.Load(), "a signed attachment redirect must never be contacted")
	assert.NotContains(t, terminal.ErrorMessage, secret)
}

func TestLocalizerRedactsSignedURLFromTransportErrors(t *testing.T) {
	secretURL := "https://objects.example.test/attachment?X-Amz-Signature=top-secret"
	client := &http.Client{Transport: attachmentRoundTripperFunc(
		func(*http.Request) (*http.Response, error) {
			return nil, &url.Error{
				Op:  http.MethodGet,
				URL: secretURL,
				Err: context.DeadlineExceeded,
			}
		},
	)}
	states := make(chan control.AttachmentLocalState, 16)
	localizer := attachmentlocalizer.New(attachmentlocalizer.Options{
		Context: context.Background(),
		RootDir: t.TempDir(),
		Client:  client,
		OnState: func(state control.AttachmentLocalState) { states <- state },
	})

	require.NoError(t, localizer.Ensure(context.Background(), control.Source{Kind: control.SourceLocal}, integrityCommand(
		"att_transport", secretURL, 5, "",
	)))
	terminal := waitForAttachmentTerminal(t, states)

	assert.Equal(t, control.AttachmentLocalFailed, terminal.State)
	assert.Equal(t, "download attachment failed", terminal.ErrorMessage)
	assert.NotContains(t, terminal.ErrorMessage, secretURL)
	assert.NotContains(t, strings.ToLower(terminal.ErrorMessage), "x-amz-signature")
	assert.NotContains(t, terminal.ErrorMessage, "top-secret")
}

func TestLocalizerGivenSameAttachmentIDAcrossRemotesWhenEnsuredThenCachesRemainIsolated(t *testing.T) {
	prodServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("prod"))
	}))
	defer prodServer.Close()
	stagingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("staging"))
	}))
	defer stagingServer.Close()

	type scopedState struct {
		source control.Source
		state  control.AttachmentLocalState
	}
	events := make(chan scopedState, 32)
	root := t.TempDir()
	localizer := attachmentlocalizer.New(attachmentlocalizer.Options{
		Context: context.Background(),
		RootDir: root,
		OnScopedState: func(source control.Source, state control.AttachmentLocalState) {
			events <- scopedState{source: source, state: state}
		},
	})
	prod := control.Source{Kind: control.SourceRemote, RemoteID: "remote_prod"}
	staging := control.Source{Kind: control.SourceRemote, RemoteID: "remote_staging"}

	require.NoError(t, localizer.Ensure(context.Background(), prod, integrityCommand(
		"att_shared", prodServer.URL, int64(len("prod")), "",
	)))
	require.NoError(t, localizer.Ensure(context.Background(), staging, integrityCommand(
		"att_shared", stagingServer.URL, int64(len("staging")), "",
	)))
	ready := map[string]control.AttachmentLocalState{}
	require.Eventually(t, func() bool {
		for {
			select {
			case event := <-events:
				if event.state.State == control.AttachmentLocalReady {
					ready[event.source.RemoteID] = event.state
				}
			default:
				return len(ready) == 2
			}
		}
	}, 5*time.Second, 10*time.Millisecond)

	assert.NotEqual(t, ready[prod.RemoteID].LocalURI, ready[staging.RemoteID].LocalURI)
	prodBytes, err := os.ReadFile(fileURIPath(t, ready[prod.RemoteID].LocalURI))
	require.NoError(t, err)
	assert.Equal(t, "prod", string(prodBytes))
	stagingBytes, err := os.ReadFile(fileURIPath(t, ready[staging.RemoteID].LocalURI))
	require.NoError(t, err)
	assert.Equal(t, "staging", string(stagingBytes))

	prodStatus, err := localizer.Status(context.Background(), prod, []string{"att_shared"})
	require.NoError(t, err)
	require.Len(t, prodStatus, 1)
	assert.Equal(t, ready[prod.RemoteID].LocalURI, prodStatus[0].LocalURI)
	stagingStatus, err := localizer.Status(context.Background(), staging, []string{"att_shared"})
	require.NoError(t, err)
	require.Len(t, stagingStatus, 1)
	assert.Equal(t, ready[staging.RemoteID].LocalURI, stagingStatus[0].LocalURI)

	restarted := attachmentlocalizer.New(attachmentlocalizer.Options{
		Context: context.Background(),
		RootDir: root,
	})
	restartedProd, err := restarted.Status(context.Background(), prod, []string{"att_shared"})
	require.NoError(t, err)
	require.Len(t, restartedProd, 1)
	assert.Equal(t, control.AttachmentLocalReady, restartedProd[0].State)
	restartedStaging, err := restarted.Status(context.Background(), staging, []string{"att_shared"})
	require.NoError(t, err)
	require.Len(t, restartedStaging, 1)
	assert.Equal(t, control.AttachmentLocalReady, restartedStaging[0].State)
}

func TestLocalizerGivenLegacyUnscopedReadyManifestWhenRemoteEnsuresThenItIsNotReused(t *testing.T) {
	root := t.TempDir()
	legacyDir := filepath.Join(root, "att_legacy")
	require.NoError(t, os.MkdirAll(legacyDir, 0o700))
	legacyPath := filepath.Join(legacyDir, "legacy.txt")
	require.NoError(t, os.WriteFile(legacyPath, []byte("old"), 0o600))
	legacyURI := (&url.URL{Scheme: "file", Path: legacyPath}).String()
	require.NoError(t, os.WriteFile(filepath.Join(legacyDir, "ready.json"), []byte(fmt.Sprintf(
		`{"attachment_id":"att_legacy","filename":"legacy.txt","size_bytes":3,"local_uri":%q}`,
		legacyURI,
	)), 0o600))

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("new"))
	}))
	defer server.Close()
	states := make(chan control.AttachmentLocalState, 16)
	localizer := attachmentlocalizer.New(attachmentlocalizer.Options{
		Context: context.Background(),
		RootDir: root,
		OnState: func(state control.AttachmentLocalState) { states <- state },
	})

	require.NoError(t, localizer.Ensure(
		context.Background(),
		control.Source{Kind: control.SourceRemote, RemoteID: "remote_prod"},
		integrityCommand("att_legacy", server.URL, 3, ""),
	))
	ready := waitForAttachmentTerminal(t, states)

	require.Equal(t, control.AttachmentLocalReady, ready.State)
	assert.Equal(t, int32(1), requests.Load())
	assert.NotEqual(t, legacyURI, ready.LocalURI)
	assert.Equal(t, "new", string(requireReadFile(t, fileURIPath(t, ready.LocalURI))))
}

func TestLocalizerGivenPathLikeRemoteIDWhenEnsuredThenLocalizedPathCannotEscapeRoot(t *testing.T) {
	root := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("safe"))
	}))
	defer server.Close()
	states := make(chan control.AttachmentLocalState, 16)
	localizer := attachmentlocalizer.New(attachmentlocalizer.Options{
		Context: context.Background(),
		RootDir: root,
		OnState: func(state control.AttachmentLocalState) { states <- state },
	})

	require.NoError(t, localizer.Ensure(
		context.Background(),
		control.Source{Kind: control.SourceRemote, RemoteID: "../../outside"},
		integrityCommand("att_safe", server.URL, 4, ""),
	))
	ready := waitForAttachmentTerminal(t, states)

	require.Equal(t, control.AttachmentLocalReady, ready.State)
	relative, err := filepath.Rel(root, fileURIPath(t, ready.LocalURI))
	require.NoError(t, err)
	assert.False(t, relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func TestLocalizerGivenExplicitLocalScopeWhenEnsuredThenRemoteCannotReuseIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("local"))
	}))
	defer server.Close()
	states := make(chan control.AttachmentLocalState, 16)
	localizer := attachmentlocalizer.New(attachmentlocalizer.Options{
		Context: context.Background(),
		RootDir: t.TempDir(),
		OnState: func(state control.AttachmentLocalState) { states <- state },
	})
	local := control.Source{Kind: control.SourceLocal}

	require.NoError(t, localizer.Ensure(
		context.Background(), local, integrityCommand("att_local", server.URL, 5, ""),
	))
	ready := waitForAttachmentTerminal(t, states)
	require.Equal(t, control.AttachmentLocalReady, ready.State)
	assert.Contains(t, filepath.ToSlash(fileURIPath(t, ready.LocalURI)), "/local/att_local/")
	remoteStatus, err := localizer.Status(
		context.Background(),
		control.Source{Kind: control.SourceRemote, RemoteID: "remote_prod"},
		[]string{"att_local"},
	)
	require.NoError(t, err)
	require.Len(t, remoteStatus, 1)
	assert.Equal(t, control.AttachmentLocalUnknown, remoteStatus[0].State)
}

func TestLocalizerGivenDeclaredZeroByteAttachmentWhenResponseIsNonEmptyThenItFailsSizeCheck(t *testing.T) {
	firstBody := newCountingResponseBody("unexpected response that must not be drained")
	secondBody := newCountingResponseBody("unexpected retry that must not be drained")
	var requestCount atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := firstBody
		if requestCount.Add(1) == 2 {
			body = secondBody
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       body,
			Request:    req,
		}, nil
	})}
	root := t.TempDir()
	states := make(chan control.AttachmentLocalState, 16)
	localizer := attachmentlocalizer.New(attachmentlocalizer.Options{
		Context: context.Background(),
		RootDir: root,
		Client:  client,
		OnState: func(state control.AttachmentLocalState) { states <- state },
	})

	require.NoError(t, localizer.Ensure(
		context.Background(),
		control.Source{Kind: control.SourceLocal},
		integrityCommand("att_empty", "https://objects.example.test/empty", 0, ""),
	))
	failed := waitForAttachmentTerminal(t, states)

	assert.Equal(t, control.AttachmentLocalFailed, failed.State)
	assert.Equal(t, "size_mismatch", failed.ErrorCode)
	assert.Contains(t, failed.ErrorMessage, "got 1, want 0")
	assert.Equal(t, int64(1), firstBody.bytesRead)
	assert.Equal(t, int64(1), secondBody.bytesRead)
	_, err := os.Stat(filepath.Join(root, "local", "att_empty", "content.part"))
	assert.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(root, "local", "att_empty", "ready.json"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestLocalizerGivenResumedResponseExceedsRemainingSizeThenItReadsOneExtraByteAndRemovesPartial(t *testing.T) {
	firstBody := newCountingResponseBody("definitely too many resumed bytes")
	secondBody := newCountingResponseBody("definitely too many retry bytes")
	var requestCount atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempt := requestCount.Add(1)
		if attempt == 1 {
			assert.Equal(t, "bytes=3-", req.Header.Get("Range"))
			return &http.Response{
				StatusCode: http.StatusPartialContent,
				Header:     make(http.Header),
				Body:       firstBody,
				Request:    req,
			}, nil
		}
		assert.Empty(t, req.Header.Get("Range"))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       secondBody,
			Request:    req,
		}, nil
	})}
	root := t.TempDir()
	dir := filepath.Join(root, "local", "att_bounded_resume")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "content.part"), []byte("abc"), 0o600))
	states := make(chan control.AttachmentLocalState, 32)
	localizer := attachmentlocalizer.New(attachmentlocalizer.Options{
		Context: context.Background(),
		RootDir: root,
		Client:  client,
		OnState: func(state control.AttachmentLocalState) { states <- state },
	})

	require.NoError(t, localizer.Ensure(
		context.Background(),
		control.Source{Kind: control.SourceLocal},
		integrityCommand(
			"att_bounded_resume",
			"https://objects.example.test/resumed",
			5,
			"",
		),
	))
	failed := waitForAttachmentTerminal(t, states)

	require.Equal(t, control.AttachmentLocalFailed, failed.State)
	assert.Equal(t, "size_mismatch", failed.ErrorCode)
	assert.Equal(t, int64(3), firstBody.bytesRead, "remaining two bytes plus one overflow byte")
	assert.Equal(t, int64(6), secondBody.bytesRead, "full retry size plus one overflow byte")
	_, err := os.Stat(filepath.Join(dir, "content.part"))
	assert.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(dir, "ready.json"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestLocalizerGivenDeclaredZeroByteAttachmentWhenResponseIsEmptyThenItBecomesReady(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	states := make(chan control.AttachmentLocalState, 16)
	localizer := attachmentlocalizer.New(attachmentlocalizer.Options{
		Context: context.Background(),
		RootDir: t.TempDir(),
		OnState: func(state control.AttachmentLocalState) { states <- state },
	})

	require.NoError(t, localizer.Ensure(
		context.Background(),
		control.Source{Kind: control.SourceLocal},
		integrityCommand("att_empty", server.URL, 0, ""),
	))
	ready := waitForAttachmentTerminal(t, states)

	require.Equal(t, control.AttachmentLocalReady, ready.State)
	assert.Zero(t, ready.SizeBytes)
	info, err := os.Stat(fileURIPath(t, ready.LocalURI))
	require.NoError(t, err)
	assert.Zero(t, info.Size())
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type countingResponseBody struct {
	reader    *strings.Reader
	bytesRead int64
}

func newCountingResponseBody(content string) *countingResponseBody {
	return &countingResponseBody{reader: strings.NewReader(content)}
}

func (b *countingResponseBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.bytesRead += int64(n)
	return n, err
}

func (*countingResponseBody) Close() error {
	return nil
}

var _ io.ReadCloser = (*countingResponseBody)(nil)

func TestLocalizerGivenInvalidScopeOrDescriptorWhenEnsuredThenItRejectsBeforeDownload(t *testing.T) {
	valid := integrityCommand("att_valid", "https://objects.example.test/download", 1, "")
	tests := []struct {
		name    string
		source  control.Source
		command control.EnsureAttachmentLocalCommand
	}{
		{name: "missing scope", source: control.Source{}, command: valid},
		{name: "remote without id", source: control.Source{Kind: control.SourceRemote}, command: valid},
		{name: "local with remote id", source: control.Source{Kind: control.SourceLocal, RemoteID: "remote_prod"}, command: valid},
		{name: "negative size", source: control.Source{Kind: control.SourceLocal}, command: integrityCommand("att_valid", "https://objects.example.test/download", -1, "")},
		{name: "unsafe attachment id", source: control.Source{Kind: control.SourceLocal}, command: integrityCommand("../escape", "https://objects.example.test/download", 1, "")},
		{name: "missing download url", source: control.Source{Kind: control.SourceLocal}, command: integrityCommand("att_valid", "", 1, "")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			localizer := attachmentlocalizer.New(attachmentlocalizer.Options{RootDir: t.TempDir()})
			assert.Error(t, localizer.Ensure(context.Background(), test.source, test.command))
		})
	}
}

func TestLocalizerGivenInvalidScopeOrAttachmentIDWhenStatusQueriedThenItRejects(t *testing.T) {
	localizer := attachmentlocalizer.New(attachmentlocalizer.Options{RootDir: t.TempDir()})

	_, err := localizer.Status(context.Background(), control.Source{Kind: control.SourceRemote}, []string{"att_1"})
	assert.Error(t, err)
	_, err = localizer.Status(context.Background(), control.Source{Kind: control.SourceLocal}, []string{"../escape"})
	assert.Error(t, err)
}

func TestLocalizerGivenTamperedScopedManifestWhenStatusQueriedThenItIsNotTrusted(t *testing.T) {
	source := control.Source{Kind: control.SourceRemote, RemoteID: "remote_prod"}
	tests := []struct {
		name   string
		mutate func(root string, manifest map[string]any)
	}{
		{
			name: "foreign remote binding",
			mutate: func(_ string, manifest map[string]any) {
				manifest["remote_id"] = "remote_staging"
			},
		},
		{
			name: "unsafe filename",
			mutate: func(_ string, manifest map[string]any) {
				manifest["filename"] = "../outside"
			},
		},
		{
			name: "external file uri",
			mutate: func(root string, manifest map[string]any) {
				manifest["local_uri"] = (&url.URL{Scheme: "file", Path: filepath.Join(root, "outside.txt")}).String()
			},
		},
		{
			name: "size mismatch",
			mutate: func(_ string, manifest map[string]any) {
				manifest["size_bytes"] = 999
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "remotes", remoteScopeHash(source.RemoteID), "att_manifest")
			require.NoError(t, os.MkdirAll(dir, 0o700))
			contentPath := filepath.Join(dir, "notes.txt")
			require.NoError(t, os.WriteFile(contentPath, []byte("ready"), 0o600))
			manifest := map[string]any{
				"attachment_id": "att_manifest",
				"source_kind":   "remote",
				"remote_id":     source.RemoteID,
				"filename":      "notes.txt",
				"size_bytes":    5,
				"sha256":        "",
				"local_uri":     (&url.URL{Scheme: "file", Path: contentPath}).String(),
			}
			test.mutate(root, manifest)
			encoded, err := json.Marshal(manifest)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "ready.json"), encoded, 0o600))
			localizer := attachmentlocalizer.New(attachmentlocalizer.Options{RootDir: root})

			items, err := localizer.Status(context.Background(), source, []string{"att_manifest"})

			require.NoError(t, err)
			require.Len(t, items, 1)
			assert.Equal(t, control.AttachmentLocalUnknown, items[0].State)
		})
	}
}

func fileURIPath(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	require.Equal(t, "file", parsed.Scheme)
	return parsed.Path
}

func requireReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

func remoteScopeHash(remoteID string) string {
	sum := sha256.Sum256([]byte(remoteID))
	return hex.EncodeToString(sum[:])
}

func waitForAttachmentTerminal(
	t *testing.T,
	states <-chan control.AttachmentLocalState,
) control.AttachmentLocalState {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case state := <-states:
			if state.State == control.AttachmentLocalReady || state.State == control.AttachmentLocalFailed {
				return state
			}
		case <-timer.C:
			t.Fatal("timed out waiting for terminal attachment state")
		}
	}
}

type attachmentRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f attachmentRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
