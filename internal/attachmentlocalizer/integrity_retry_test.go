package attachmentlocalizer_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/attachmentlocalizer"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalizerGivenFirstDownloadHashMismatchWhenRetriedThenSecondDownloadBecomesReady(t *testing.T) {
	content := "hello"
	sum := sha256.Sum256([]byte(content))
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Range"))
		if requests.Add(1) == 1 {
			_, _ = w.Write([]byte("HELLO"))
			return
		}
		_, _ = w.Write([]byte(content))
	}))
	defer server.Close()

	states := make(chan control.AttachmentLocalState, 32)
	localizer := attachmentlocalizer.New(attachmentlocalizer.Options{
		Context: context.Background(),
		RootDir: t.TempDir(),
		OnState: func(state control.AttachmentLocalState) { states <- state },
	})
	require.NoError(t, localizer.Ensure(context.Background(), integrityCommand(
		"att_retry",
		server.URL,
		int64(len(content)),
		fmt.Sprintf("%x", sum),
	)))

	state := waitForAttachmentState(t, states, control.AttachmentLocalReady)
	require.Equal(t, control.AttachmentLocalReady, state.State)
	require.Equal(t, int32(2), requests.Load())
}

func TestLocalizerGivenRepeatedHashMismatchWhenRetriesExhaustedThenBadPartialIsRemoved(t *testing.T) {
	content := "hello"
	sum := sha256.Sum256([]byte(content))
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Range"))
		requests.Add(1)
		_, _ = w.Write([]byte("HELLO"))
	}))
	defer server.Close()

	root := t.TempDir()
	states := make(chan control.AttachmentLocalState, 32)
	localizer := attachmentlocalizer.New(attachmentlocalizer.Options{
		Context: context.Background(),
		RootDir: root,
		OnState: func(state control.AttachmentLocalState) { states <- state },
	})
	require.NoError(t, localizer.Ensure(context.Background(), integrityCommand(
		"att_failed",
		server.URL,
		int64(len(content)),
		fmt.Sprintf("%x", sum),
	)))

	state := waitForAttachmentState(t, states, control.AttachmentLocalFailed)
	require.Equal(t, "hash_mismatch", state.ErrorCode)
	require.Equal(t, int32(2), requests.Load())
	_, err := os.Stat(filepath.Join(root, "att_failed", "content.part"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func integrityCommand(
	attachmentID string,
	downloadURL string,
	size int64,
	sha256 string,
) control.EnsureAttachmentLocalCommand {
	return control.EnsureAttachmentLocalCommand{
		Attachment: control.AttachmentDescriptor{
			AttachmentID: attachmentID,
			Filename:     "notes.txt",
			SizeBytes:    size,
			SHA256:       sha256,
		},
		Download: control.AttachmentDownloadTicket{URL: downloadURL},
	}
}

func waitForAttachmentState(
	t *testing.T,
	states <-chan control.AttachmentLocalState,
	target control.AttachmentLocalStateValue,
) control.AttachmentLocalState {
	t.Helper()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case state := <-states:
			if state.State == target {
				return state
			}
		case <-timeout.C:
			t.Fatalf("timed out waiting for attachment state %q", target)
		}
	}
}
