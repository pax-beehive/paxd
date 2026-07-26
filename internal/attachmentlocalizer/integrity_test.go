package attachmentlocalizer_test

import (
	"context"
	"crypto/sha256"
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

func TestLocalizerGivenCompleteCorruptPartialWhenEnsuredThenItRedownloadsFromZero(t *testing.T) {
	content := "hello world"
	sum := sha256.Sum256([]byte(content))
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Empty(t, r.Header.Get("Range"))
		_, _ = w.Write([]byte(content))
	}))
	defer server.Close()

	root := t.TempDir()
	dir := filepath.Join(root, "att_corrupt")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "content.part"),
		[]byte("HELLO WORLD"),
		0o600,
	))

	states := make(chan control.AttachmentLocalState, 16)
	localizer := attachmentlocalizer.New(attachmentlocalizer.Options{
		Context: context.Background(),
		RootDir: root,
		OnState: func(state control.AttachmentLocalState) { states <- state },
	})
	require.NoError(t, localizer.Ensure(context.Background(), control.EnsureAttachmentLocalCommand{
		Attachment: control.AttachmentDescriptor{
			AttachmentID: "att_corrupt",
			Filename:     "notes.txt",
			SizeBytes:    int64(len(content)),
			SHA256:       fmt.Sprintf("%x", sum),
		},
		Download: control.AttachmentDownloadTicket{URL: server.URL},
	}))

	var ready control.AttachmentLocalState
	require.Eventually(t, func() bool {
		select {
		case state := <-states:
			ready = state
		default:
		}
		return ready.State == control.AttachmentLocalReady
	}, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, 1, requests)
	path := strings.TrimPrefix(ready.LocalURI, "file://")
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, content, string(got))
}
