package attachmentlocalizer_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	dir := filepath.Join(root, "att_1")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "content.part"), []byte(content[:6]), 0o600))

	states := make(chan control.AttachmentLocalState, 16)
	localizer := attachmentlocalizer.New(attachmentlocalizer.Options{
		Context: context.Background(),
		RootDir: root,
		OnState: func(state control.AttachmentLocalState) { states <- state },
	})
	require.NoError(t, localizer.Ensure(context.Background(), control.EnsureAttachmentLocalCommand{
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

	items, err := localizer.Status(context.Background(), []string{"att_1"})
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
	require.NoError(t, first.Ensure(context.Background(), control.EnsureAttachmentLocalCommand{
		Attachment: control.AttachmentDescriptor{
			AttachmentID: "att_restart",
			Filename:     "persisted.txt",
			SizeBytes:    int64(len(content)),
		},
		Download: control.AttachmentDownloadTicket{URL: server.URL},
	}))
	require.Eventually(t, func() bool {
		items, err := first.Status(context.Background(), []string{"att_restart"})
		return err == nil && len(items) == 1 && items[0].State == control.AttachmentLocalReady
	}, 5*time.Second, 10*time.Millisecond)

	restarted := attachmentlocalizer.New(attachmentlocalizer.Options{Context: context.Background(), RootDir: root})
	items, err := restarted.Status(context.Background(), []string{"att_restart", "missing"})
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, control.AttachmentLocalReady, items[0].State)
	assert.Equal(t, control.AttachmentLocalUnknown, items[1].State)
}
