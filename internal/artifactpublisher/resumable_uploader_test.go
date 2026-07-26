package artifactpublisher_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pax-beehive/paxd/internal/artifactpublisher"
	"github.com/pax-beehive/paxd/internal/daemonstore"
)

func TestResumableUploaderGivenNewTicketWhenUploadingThenSendsAlignedChunks(
	t *testing.T,
) {
	var ranges []string
	var bodies []string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "start", r.Header.Get("x-goog-resumable"))
			assert.Equal(t, "sha_test", r.Header.Get("x-goog-meta-sha256"))
			w.Header().Set("Location", server.URL+"/session")
			w.WriteHeader(http.StatusCreated)
		case "/session":
			ranges = append(ranges, r.Header.Get("Content-Range"))
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			bodies = append(bodies, string(body))
			switch len(ranges) {
			case 1:
				w.Header().Set("Range", "bytes=0-3")
				w.WriteHeader(308)
			case 2:
				w.Header().Set("Range", "bytes=0-7")
				w.WriteHeader(308)
			default:
				w.WriteHeader(http.StatusOK)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	job := resumableUploadJob(t, []byte("0123456789"))
	uploader := artifactpublisher.NewResumableUploader(server.Client())
	uploader.ChunkSize = 4
	var offsets []int64

	err := uploader.Upload(
		context.Background(),
		job,
		artifactpublisher.UploadTicket{
			Method: http.MethodPost,
			URL:    server.URL + "/start",
			Headers: map[string]string{
				"x-goog-resumable":   "start",
				"x-goog-meta-sha256": "sha_test",
			},
			ChunkAlignment: 4,
		},
		func(_ string, offset int64) error {
			offsets = append(offsets, offset)
			return nil
		},
	)

	require.NoError(t, err)
	assert.Equal(t, []string{
		"bytes 0-3/10",
		"bytes 4-7/10",
		"bytes 8-9/10",
	}, ranges)
	assert.Equal(t, []string{"0123", "4567", "89"}, bodies)
	assert.Equal(t, []int64{0, 4, 8, 10}, offsets)
}

func TestResumableUploaderGivenRestartWhenUploadingThenQueriesCommittedOffset(
	t *testing.T,
) {
	var ranges []string
	var queryCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentRange := r.Header.Get("Content-Range")
		if contentRange == "bytes */10" {
			queryCount++
			w.Header().Set("Range", "bytes=0-3")
			w.WriteHeader(308)
			return
		}
		ranges = append(ranges, contentRange)
		if len(ranges) == 1 {
			w.Header().Set("Range", "bytes=0-7")
			w.WriteHeader(308)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	job := resumableUploadJob(t, []byte("0123456789"))
	job.ResumableURL = server.URL + "/session"
	job.UploadedBytes = 1
	uploader := artifactpublisher.NewResumableUploader(server.Client())
	uploader.ChunkSize = 4

	err := uploader.Upload(
		context.Background(),
		job,
		artifactpublisher.UploadTicket{ChunkAlignment: 4},
		func(string, int64) error { return nil },
	)

	require.NoError(t, err)
	assert.Equal(t, 1, queryCount)
	assert.Equal(t, []string{"bytes 4-7/10", "bytes 8-9/10"}, ranges)
}

func TestResumableUploaderGivenExpiredSessionWhenResumingThenRequestsFreshTicket(
	t *testing.T,
) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer server.Close()
	job := resumableUploadJob(t, []byte("content"))
	job.ResumableURL = server.URL + "/expired"
	uploader := artifactpublisher.NewResumableUploader(server.Client())

	err := uploader.Upload(
		context.Background(),
		job,
		artifactpublisher.UploadTicket{},
		func(string, int64) error { return nil },
	)

	require.ErrorIs(t, err, artifactpublisher.ErrResumableSessionExpired)
}

func resumableUploadJob(
	t *testing.T,
	content []byte,
) daemonstore.ArtifactPublishJob {
	t.Helper()
	path := filepath.Join(t.TempDir(), "content")
	require.NoError(t, os.WriteFile(path, content, 0o600))
	return daemonstore.ArtifactPublishJob{
		PublicationID: "apub_upload",
		SpoolPath:     path,
		SizeBytes:     int64(len(content)),
		ContentType:   "application/octet-stream",
	}
}
