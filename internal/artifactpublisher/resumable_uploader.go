package artifactpublisher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/daemonstore"
)

const defaultArtifactChunkSize int64 = 8 * 1024 * 1024

type ResumableUploader struct {
	Client    HTTPDoer
	ChunkSize int64
}

func NewResumableUploader(client HTTPDoer) *ResumableUploader {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	return &ResumableUploader{Client: client, ChunkSize: defaultArtifactChunkSize}
}

func (u *ResumableUploader) Upload(
	ctx context.Context,
	job daemonstore.ArtifactPublishJob,
	ticket UploadTicket,
	progress func(string, int64) error,
) error {
	if u == nil || u.Client == nil {
		return errors.New("artifact resumable uploader is not configured")
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
	sessionURL := strings.TrimSpace(job.ResumableURL)
	offset := job.UploadedBytes
	if sessionURL == "" {
		sessionURL, err = u.initiate(ctx, ticket)
		if err != nil {
			return err
		}
		offset = 0
		if err := progress(sessionURL, offset); err != nil {
			return err
		}
	} else {
		offset, err = u.queryOffset(ctx, sessionURL, job.SizeBytes)
		if err != nil {
			return err
		}
		if err := progress(sessionURL, offset); err != nil {
			return err
		}
	}
	if offset < 0 || offset > job.SizeBytes {
		return fmt.Errorf("invalid committed artifact offset %d", offset)
	}
	chunkSize := alignedArtifactChunkSize(u.ChunkSize, ticket.ChunkAlignment)
	if job.SizeBytes == 0 {
		return u.uploadZeroByteObject(ctx, sessionURL, progress)
	}
	for offset < job.SizeBytes {
		length := min(chunkSize, job.SizeBytes-offset)
		next, complete, err := u.uploadChunk(
			ctx,
			sessionURL,
			file,
			offset,
			length,
			job.SizeBytes,
			job.ContentType,
		)
		if err != nil {
			return err
		}
		if next <= offset {
			return fmt.Errorf("artifact upload made no progress from offset %d", offset)
		}
		offset = next
		if err := progress(sessionURL, offset); err != nil {
			return err
		}
		if complete && offset != job.SizeBytes {
			return fmt.Errorf("artifact upload completed early at offset %d", offset)
		}
	}
	return nil
}

func (u *ResumableUploader) initiate(
	ctx context.Context,
	ticket UploadTicket,
) (string, error) {
	if strings.TrimSpace(ticket.URL) == "" {
		return "", errors.New("artifact upload ticket URL is empty")
	}
	method := strings.TrimSpace(ticket.Method)
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, ticket.URL, nil)
	if err != nil {
		return "", err
	}
	for key, value := range ticket.Headers {
		req.Header.Set(key, value)
	}
	req.ContentLength = 0
	resp, err := u.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", resumableStatusError(resp.StatusCode, "initiate")
	}
	location := strings.TrimSpace(resp.Header.Get("Location"))
	if location == "" {
		return "", errors.New("artifact resumable initiation omitted Location")
	}
	base, err := url.Parse(ticket.URL)
	if err != nil {
		return "", err
	}
	resolved, err := base.Parse(location)
	if err != nil {
		return "", err
	}
	return resolved.String(), nil
}

func (u *ResumableUploader) queryOffset(
	ctx context.Context,
	sessionURL string,
	total int64,
) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, sessionURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Range", fmt.Sprintf("bytes */%d", total))
	req.ContentLength = 0
	resp, err := u.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		return total, nil
	case 308:
		return committedArtifactOffset(resp.Header.Get("Range"))
	default:
		return 0, resumableStatusError(resp.StatusCode, "query")
	}
}

func (u *ResumableUploader) uploadChunk(
	ctx context.Context,
	sessionURL string,
	file *os.File,
	offset int64,
	length int64,
	total int64,
	contentType string,
) (int64, bool, error) {
	body := io.NewSectionReader(file, offset, length)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, sessionURL, body)
	if err != nil {
		return 0, false, err
	}
	req.ContentLength = length
	req.Header.Set(
		"Content-Range",
		fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, total),
	)
	if strings.TrimSpace(contentType) != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := u.Client.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		return offset + length, true, nil
	case 308:
		next, err := committedArtifactOffset(resp.Header.Get("Range"))
		return next, false, err
	default:
		return 0, false, resumableStatusError(resp.StatusCode, "upload")
	}
}

func (u *ResumableUploader) uploadZeroByteObject(
	ctx context.Context,
	sessionURL string,
	progress func(string, int64) error,
) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, sessionURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Range", "bytes */0")
	req.ContentLength = 0
	resp, err := u.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return resumableStatusError(resp.StatusCode, "upload empty object")
	}
	return progress(sessionURL, 0)
}

func alignedArtifactChunkSize(configured int64, alignment int64) int64 {
	if configured <= 0 {
		configured = defaultArtifactChunkSize
	}
	if alignment <= 0 {
		return configured
	}
	if configured < alignment {
		return alignment
	}
	return configured - configured%alignment
}

func committedArtifactOffset(header string) (int64, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, nil
	}
	value := strings.TrimPrefix(header, "bytes=")
	parts := strings.Split(value, "-")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid artifact upload Range %q", header)
	}
	last, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || last < 0 {
		return 0, fmt.Errorf("invalid artifact upload Range %q", header)
	}
	return last + 1, nil
}

func resumableStatusError(status int, action string) error {
	if status == http.StatusNotFound || status == http.StatusGone {
		return fmt.Errorf("%w during %s", ErrResumableSessionExpired, action)
	}
	return fmt.Errorf("artifact resumable %s returned HTTP %d", action, status)
}
