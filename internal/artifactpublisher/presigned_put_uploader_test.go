package artifactpublisher_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pax-beehive/paxd/internal/artifactpublisher"
)

func TestPresignedPutUploaderGivenTicketWhenUploadingThenPutsEntireFileWithTicketHeaders(
	t *testing.T,
) {
	content := []byte("complete artifact bytes")
	var gotBody []byte
	var gotMethod string
	var gotContentLength int64
	var gotChecksum string
	var gotContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentLength = r.ContentLength
		gotChecksum = r.Header.Get("x-amz-checksum-sha256")
		gotContentType = r.Header.Get("Content-Type")
		var err error
		gotBody, err = io.ReadAll(r.Body)
		require.NoError(t, err)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	job := resumableUploadJob(t, content)
	uploader := artifactpublisher.NewPresignedPutUploader(server.Client())
	var progressURL string
	var uploadedBytes int64

	err := uploader.Upload(
		context.Background(),
		job,
		artifactpublisher.UploadTicket{
			Protocol: artifactpublisher.UploadProtocolS3PresignedPut,
			Method:   http.MethodPost,
			URL:      server.URL + "/object?signature=opaque",
			Headers: map[string]string{
				"x-amz-checksum-sha256": "checksum-value",
				"Content-Type":          "application/custom",
			},
		},
		func(sessionURL string, offset int64) error {
			progressURL = sessionURL
			uploadedBytes = offset
			return nil
		},
	)

	require.NoError(t, err)
	assert.Equal(t, http.MethodPut, gotMethod, "presigned PUT ignores ticket method overrides")
	assert.Equal(t, int64(len(content)), gotContentLength)
	assert.Equal(t, content, gotBody)
	assert.Equal(t, "checksum-value", gotChecksum)
	assert.Equal(t, "application/custom", gotContentType)
	assert.Empty(t, progressURL, "a presigned URL must not be persisted as resumable state")
	assert.Equal(t, int64(len(content)), uploadedBytes)
}

func TestPresignedPutUploaderAcceptsAnySuccessfulHTTPStatus(t *testing.T) {
	for _, status := range []int{
		http.StatusOK,
		http.StatusCreated,
		http.StatusAccepted,
		http.StatusNoContent,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer server.Close()
			uploader := artifactpublisher.NewPresignedPutUploader(server.Client())

			err := uploader.Upload(
				context.Background(),
				resumableUploadJob(t, []byte("bytes")),
				artifactpublisher.UploadTicket{URL: server.URL},
				func(string, int64) error { return nil },
			)

			require.NoError(t, err)
		})
	}
}

func TestPresignedPutUploaderGivenWriteOncePreconditionFailureThenContinuesCompletion(
	t *testing.T,
) {
	job := resumableUploadJob(t, []byte("already stored bytes"))
	uploader := &artifactpublisher.PresignedPutUploader{Client: artifactHTTPDoerFunc(
		func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusPreconditionFailed,
				Body:       http.NoBody,
			}, nil
		},
	)}
	var progressURL string
	var uploadedBytes int64

	err := uploader.Upload(
		context.Background(),
		job,
		artifactpublisher.UploadTicket{URL: "https://objects.example.test/already-stored"},
		func(sessionURL string, offset int64) error {
			progressURL = sessionURL
			uploadedBytes = offset
			return nil
		},
	)

	require.NoError(t, err)
	assert.Empty(t, progressURL, "a presigned URL must never enter durable progress state")
	assert.Equal(t, job.SizeBytes, uploadedBytes)
}

func TestPresignedPutUploaderGivenOtherClientConflictThenRejectsUpload(t *testing.T) {
	uploader := &artifactpublisher.PresignedPutUploader{Client: artifactHTTPDoerFunc(
		func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusConflict, Body: http.NoBody}, nil
		},
	)}
	progressCalled := false

	err := uploader.Upload(
		context.Background(),
		resumableUploadJob(t, []byte("bytes")),
		artifactpublisher.UploadTicket{URL: "https://objects.example.test/conflict"},
		func(string, int64) error {
			progressCalled = true
			return nil
		},
	)

	require.ErrorContains(t, err, "HTTP 409")
	assert.False(t, progressCalled)
}

func TestPresignedPutUploaderGivenZeroByteSnapshotThenSendsAnEmptyFixedLengthBody(
	t *testing.T,
) {
	var gotContentLength int64
	var gotTransferEncoding []string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentLength = r.ContentLength
		gotTransferEncoding = append([]string(nil), r.TransferEncoding...)
		var err error
		gotBody, err = io.ReadAll(r.Body)
		require.NoError(t, err)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	uploader := artifactpublisher.NewPresignedPutUploader(server.Client())

	err := uploader.Upload(
		context.Background(),
		resumableUploadJob(t, nil),
		artifactpublisher.UploadTicket{URL: server.URL},
		nil,
	)

	require.NoError(t, err)
	assert.Zero(t, gotContentLength)
	assert.Empty(t, gotTransferEncoding, "zero-byte S3 PUT must not use chunked encoding")
	assert.Empty(t, gotBody)
}

func TestNewPresignedPutUploaderUsesPhaseTimeoutsWithoutAnOverallUploadDeadline(
	t *testing.T,
) {
	uploader := artifactpublisher.NewPresignedPutUploader(nil)
	client, ok := uploader.Client.(*http.Client)
	require.True(t, ok)
	assert.Zero(t, client.Timeout, "large uploads must not be canceled by a fixed overall deadline")
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	assert.NotNil(t, transport.DialContext)
	assert.Greater(t, transport.TLSHandshakeTimeout, time.Duration(0))
	assert.Greater(t, transport.ResponseHeaderTimeout, time.Duration(0))
}

func TestPresignedPutUploaderGivenRejectedRequestThenDoesNotReportProgress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("signature rejected"))
	}))
	defer server.Close()
	uploader := artifactpublisher.NewPresignedPutUploader(server.Client())
	progressCalled := false

	err := uploader.Upload(
		context.Background(),
		resumableUploadJob(t, []byte("bytes")),
		artifactpublisher.UploadTicket{URL: server.URL},
		func(string, int64) error {
			progressCalled = true
			return nil
		},
	)

	require.ErrorContains(t, err, "HTTP 403")
	assert.False(t, progressCalled)
}

func TestPresignedPutUploaderValidatesConfigurationAndSnapshot(t *testing.T) {
	job := resumableUploadJob(t, []byte("bytes"))

	err := (*artifactpublisher.PresignedPutUploader)(nil).Upload(
		context.Background(), job, artifactpublisher.UploadTicket{URL: "https://upload.example"}, nil,
	)
	require.ErrorContains(t, err, "not configured")

	uploader := artifactpublisher.NewPresignedPutUploader(nil)
	require.NotNil(t, uploader.Client)
	err = uploader.Upload(context.Background(), job, artifactpublisher.UploadTicket{}, nil)
	require.ErrorContains(t, err, "URL is empty")

	missing := job
	missing.SpoolPath += ".missing"
	err = uploader.Upload(
		context.Background(), missing,
		artifactpublisher.UploadTicket{URL: "https://upload.example"}, nil,
	)
	require.Error(t, err)

	changed := job
	changed.SizeBytes++
	err = uploader.Upload(
		context.Background(), changed,
		artifactpublisher.UploadTicket{URL: "https://upload.example"}, nil,
	)
	require.ErrorContains(t, err, "snapshot size changed")

	directory := job
	directory.SpoolPath = t.TempDir()
	directory.SizeBytes = 0
	err = uploader.Upload(
		context.Background(), directory,
		artifactpublisher.UploadTicket{URL: "https://upload.example"}, nil,
	)
	require.ErrorContains(t, err, "snapshot size changed")

	require.NoError(t, os.Remove(job.SpoolPath))
}

func TestPresignedPutUploaderPropagatesRequestTransportAndProgressErrors(t *testing.T) {
	job := resumableUploadJob(t, []byte("bytes"))
	uploader := &artifactpublisher.PresignedPutUploader{Client: failingArtifactHTTPDoer{}}

	err := uploader.Upload(
		context.Background(), job,
		artifactpublisher.UploadTicket{URL: ":"}, nil,
	)
	require.ErrorContains(t, err, "create artifact presigned PUT request")

	err = uploader.Upload(
		context.Background(), job,
		artifactpublisher.UploadTicket{URL: "https://upload.example"}, nil,
	)
	require.ErrorContains(t, err, "upload artifact with presigned PUT")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	uploader = artifactpublisher.NewPresignedPutUploader(server.Client())
	err = uploader.Upload(
		context.Background(), job,
		artifactpublisher.UploadTicket{URL: server.URL},
		func(string, int64) error { return errors.New("persist progress") },
	)
	require.ErrorContains(t, err, "persist progress")

	require.NoError(t, uploader.Upload(
		context.Background(), job,
		artifactpublisher.UploadTicket{URL: server.URL}, nil,
	))
}

func TestPresignedPutUploaderRedactsPresignedURLFromTransportErrors(t *testing.T) {
	job := resumableUploadJob(t, []byte("bytes"))
	secretURL := "https://bucket.example/object?X-Amz-Credential=credential&X-Amz-Signature=top-secret"
	uploader := &artifactpublisher.PresignedPutUploader{Client: artifactHTTPDoerFunc(
		func(*http.Request) (*http.Response, error) {
			return nil, &url.Error{
				Op:  http.MethodPut,
				URL: secretURL,
				Err: context.DeadlineExceeded,
			}
		},
	)}

	err := uploader.Upload(
		context.Background(), job,
		artifactpublisher.UploadTicket{URL: secretURL}, nil,
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotContains(t, err.Error(), secretURL)
	assert.NotContains(t, strings.ToLower(err.Error()), "x-amz-signature")
	assert.NotContains(t, err.Error(), "top-secret")
}

func TestPresignedPutUploaderGivenRedirectThenDoesNotFollowBearerURL(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	secret := "top-secret-presigned-query"
	uploader := artifactpublisher.NewPresignedPutUploader(redirect.Client())
	progressCalled := false

	err := uploader.Upload(
		context.Background(),
		resumableUploadJob(t, []byte("bytes")),
		artifactpublisher.UploadTicket{URL: redirect.URL + "/object?X-Amz-Signature=" + secret},
		func(string, int64) error {
			progressCalled = true
			return nil
		},
	)

	require.ErrorContains(t, err, "HTTP 302")
	assert.Zero(t, targetCalls.Load(), "a presigned URL redirect must never be contacted")
	assert.False(t, progressCalled)
	assert.NotContains(t, err.Error(), secret)
}

type failingArtifactHTTPDoer struct{}

func (failingArtifactHTTPDoer) Do(*http.Request) (*http.Response, error) {
	return nil, errors.New("transport unavailable")
}

type artifactHTTPDoerFunc func(*http.Request) (*http.Response, error)

func (f artifactHTTPDoerFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}
