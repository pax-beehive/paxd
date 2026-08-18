package artifactpublisher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/pax-beehive/paxd/internal/safehttp"
)

const defaultPresignedPutResponseHeaderTimeout = 30 * time.Second

// PresignedPutUploader uploads one complete artifact to a final presigned PUT
// URL. The URL is a bearer credential and is deliberately never persisted.
type PresignedPutUploader struct {
	Client HTTPDoer
}

func NewPresignedPutUploader(client HTTPDoer) *PresignedPutUploader {
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = defaultPresignedPutResponseHeaderTimeout
		client = &http.Client{Transport: transport}
	}
	return &PresignedPutUploader{Client: client}
}

func (u *PresignedPutUploader) Upload(
	ctx context.Context,
	job daemonstore.ArtifactPublishJob,
	ticket UploadTicket,
	progress func(string, int64) error,
) error {
	if u == nil || u.Client == nil {
		return errors.New("artifact presigned PUT uploader is not configured")
	}
	if strings.TrimSpace(ticket.URL) == "" {
		return errors.New("artifact presigned PUT URL is empty")
	}
	file, err := os.Open(job.SpoolPath) // #nosec G304 -- path is daemon-owned durable state.
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != job.SizeBytes {
		return errors.New("artifact snapshot size changed after hashing")
	}
	var body io.Reader = file
	if job.SizeBytes == 0 {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, ticket.URL, body) // #nosec G107 -- URL is a short-lived manager-issued upload ticket.
	if err != nil {
		return errors.New("create artifact presigned PUT request failed")
	}
	for key, value := range ticket.Headers {
		req.Header.Set(key, value)
	}
	req.ContentLength = job.SizeBytes
	resp, err := safehttp.DoNoRedirect(u.Client, req)
	if err != nil {
		return safehttp.RedactError("upload artifact with presigned PUT", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if (resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices) &&
		resp.StatusCode != http.StatusPreconditionFailed {
		return fmt.Errorf("artifact presigned PUT returned HTTP %d", resp.StatusCode)
	}
	if progress == nil {
		return nil
	}
	return progress("", job.SizeBytes)
}
