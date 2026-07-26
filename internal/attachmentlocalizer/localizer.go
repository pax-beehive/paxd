package attachmentlocalizer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pax-beehive/paxd/internal/control"
)

const (
	partialFilename  = "content.part"
	manifestFilename = "ready.json"
)

type Options struct {
	Context context.Context
	RootDir string
	Client  *http.Client
	OnState func(control.AttachmentLocalState)
}

type Localizer struct {
	ctx     context.Context
	rootDir string
	client  *http.Client
	onState func(control.AttachmentLocalState)

	mu     sync.Mutex
	states map[string]control.AttachmentLocalState
	active map[string]bool
}

type readyManifest struct {
	AttachmentID string `json:"attachment_id"`
	Filename     string `json:"filename"`
	SizeBytes    int64  `json:"size_bytes"`
	SHA256       string `json:"sha256"`
	LocalURI     string `json:"local_uri"`
}

func New(opts Options) *Localizer {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}
	return &Localizer{
		ctx:     ctx,
		rootDir: opts.RootDir,
		client:  client,
		onState: opts.OnState,
		states:  make(map[string]control.AttachmentLocalState),
		active:  make(map[string]bool),
	}
}

func (l *Localizer) Ensure(_ context.Context, command control.EnsureAttachmentLocalCommand) error {
	if err := validateCommand(command); err != nil {
		return err
	}
	if ready, ok := l.readReady(command.Attachment.AttachmentID); ok {
		l.publish(ready)
		return nil
	}

	attachmentID := command.Attachment.AttachmentID
	l.mu.Lock()
	if l.active[attachmentID] {
		l.mu.Unlock()
		return nil
	}
	l.active[attachmentID] = true
	l.mu.Unlock()

	l.publish(control.AttachmentLocalState{
		AttachmentID: attachmentID,
		State:        control.AttachmentLocalQueued,
		TotalBytes:   command.Attachment.SizeBytes,
		SizeBytes:    command.Attachment.SizeBytes,
	})
	go l.download(command)
	return nil
}

func (l *Localizer) Status(_ context.Context, attachmentIDs []string) ([]control.AttachmentLocalState, error) {
	result := make([]control.AttachmentLocalState, 0, len(attachmentIDs))
	for _, attachmentID := range attachmentIDs {
		if err := validateAttachmentID(attachmentID); err != nil {
			return nil, err
		}

		l.mu.Lock()
		state, ok := l.states[attachmentID]
		l.mu.Unlock()
		if ok && state.State != control.AttachmentLocalUnknown {
			result = append(result, state)
			continue
		}
		if ready, ok := l.readReady(attachmentID); ok {
			result = append(result, ready)
			continue
		}
		partPath := filepath.Join(l.rootDir, attachmentID, partialFilename)
		if info, err := os.Stat(partPath); err == nil {
			result = append(result, control.AttachmentLocalState{
				AttachmentID:    attachmentID,
				State:           control.AttachmentLocalPaused,
				BytesDownloaded: info.Size(),
			})
			continue
		}
		result = append(result, control.AttachmentLocalState{
			AttachmentID: attachmentID,
			State:        control.AttachmentLocalUnknown,
		})
	}
	return result, nil
}

func (l *Localizer) download(command control.EnsureAttachmentLocalCommand) {
	attachment := command.Attachment
	defer func() {
		l.mu.Lock()
		delete(l.active, attachment.AttachmentID)
		l.mu.Unlock()
	}()

	if err := l.downloadFile(command); err != nil {
		errorCode := "download_failed"
		var integrity *integrityError
		if errors.As(err, &integrity) {
			errorCode = integrity.code
		}
		l.publish(control.AttachmentLocalState{
			AttachmentID: attachment.AttachmentID,
			State:        control.AttachmentLocalFailed,
			TotalBytes:   attachment.SizeBytes,
			SizeBytes:    attachment.SizeBytes,
			ErrorCode:    errorCode,
			ErrorMessage: err.Error(),
		})
	}
}

func (l *Localizer) downloadFileAttempt(command control.EnsureAttachmentLocalCommand) error {
	attachment := command.Attachment
	dir := filepath.Join(l.rootDir, attachment.AttachmentID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create attachment directory: %w", err)
	}
	partPath := filepath.Join(dir, partialFilename)
	part, err := os.OpenFile(partPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open partial attachment: %w", err)
	}
	defer part.Close()

	info, err := part.Stat()
	if err != nil {
		return fmt.Errorf("stat partial attachment: %w", err)
	}
	offset := info.Size()
	if attachment.SizeBytes > 0 && offset > attachment.SizeBytes {
		if err := part.Truncate(0); err != nil {
			return fmt.Errorf("reset oversized partial attachment: %w", err)
		}
		offset = 0
	}
	if attachment.SizeBytes > 0 && offset == attachment.SizeBytes {
		if attachment.SHA256 != "" {
			actualSHA256, hashErr := openFileSHA256(part)
			if hashErr != nil {
				return hashErr
			}
			if !strings.EqualFold(actualSHA256, attachment.SHA256) {
				return &integrityError{
					code: "hash_mismatch",
					message: fmt.Sprintf(
						"sha256 mismatch: got %s, want %s",
						actualSHA256,
						attachment.SHA256,
					),
				}
			}
		}
		if err := part.Truncate(0); err != nil {
			return fmt.Errorf("reset complete partial attachment: %w", err)
		}
		if _, err := part.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("seek reset complete attachment: %w", err)
		}
		offset = 0
	}

	req, err := http.NewRequestWithContext(l.ctx, http.MethodGet, command.Download.URL, nil)
	if err != nil {
		return fmt.Errorf("create download request: %w", err)
	}
	for key, value := range command.Download.Headers {
		req.Header.Set(key, value)
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return fmt.Errorf("download attachment: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case offset > 0 && resp.StatusCode == http.StatusPartialContent:
		if _, err := part.Seek(offset, io.SeekStart); err != nil {
			return fmt.Errorf("seek partial attachment: %w", err)
		}
	case resp.StatusCode == http.StatusOK:
		if err := part.Truncate(0); err != nil {
			return fmt.Errorf("reset partial attachment: %w", err)
		}
		if _, err := part.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("seek reset attachment: %w", err)
		}
		offset = 0
	default:
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	l.publish(control.AttachmentLocalState{
		AttachmentID:    attachment.AttachmentID,
		State:           control.AttachmentLocalDownloading,
		BytesDownloaded: offset,
		TotalBytes:      attachment.SizeBytes,
		SizeBytes:       attachment.SizeBytes,
	})
	written, err := l.copyWithProgress(part, resp.Body, attachment, offset)
	if err != nil {
		return err
	}
	total := offset + written
	l.publish(control.AttachmentLocalState{
		AttachmentID:    attachment.AttachmentID,
		State:           control.AttachmentLocalVerifying,
		BytesDownloaded: total,
		TotalBytes:      attachment.SizeBytes,
		SizeBytes:       attachment.SizeBytes,
	})
	if err := part.Sync(); err != nil {
		return fmt.Errorf("sync partial attachment: %w", err)
	}
	if attachment.SizeBytes > 0 && total != attachment.SizeBytes {
		return &integrityError{
			code:    "size_mismatch",
			message: fmt.Sprintf("size mismatch: got %d, want %d", total, attachment.SizeBytes),
		}
	}
	if _, err := part.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek attachment for verification: %w", err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, part); err != nil {
		return fmt.Errorf("hash attachment: %w", err)
	}
	actualSHA256 := hex.EncodeToString(hash.Sum(nil))
	if attachment.SHA256 != "" && !strings.EqualFold(actualSHA256, attachment.SHA256) {
		return &integrityError{
			code: "hash_mismatch",
			message: fmt.Sprintf(
				"sha256 mismatch: got %s, want %s",
				actualSHA256,
				attachment.SHA256,
			),
		}
	}
	if err := part.Close(); err != nil {
		return fmt.Errorf("close partial attachment: %w", err)
	}

	filename := safeFilename(attachment.Filename)
	finalPath := filepath.Join(dir, filename)
	if err := os.Rename(partPath, finalPath); err != nil {
		return fmt.Errorf("publish attachment: %w", err)
	}
	absolutePath, err := filepath.Abs(finalPath)
	if err != nil {
		return fmt.Errorf("resolve attachment path: %w", err)
	}
	localURI := (&url.URL{Scheme: "file", Path: absolutePath}).String()
	manifest := readyManifest{
		AttachmentID: attachment.AttachmentID,
		Filename:     filename,
		SizeBytes:    total,
		SHA256:       actualSHA256,
		LocalURI:     localURI,
	}
	if err := writeReadyManifest(dir, manifest); err != nil {
		return err
	}
	l.publish(control.AttachmentLocalState{
		AttachmentID:    attachment.AttachmentID,
		State:           control.AttachmentLocalReady,
		BytesDownloaded: total,
		TotalBytes:      total,
		SizeBytes:       total,
		LocalURI:        localURI,
		SHA256:          actualSHA256,
	})
	return nil
}

func (l *Localizer) copyWithProgress(dst io.Writer, src io.Reader, attachment control.AttachmentDescriptor, offset int64) (int64, error) {
	buf := make([]byte, 256*1024)
	var written int64
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			nw, writeErr := dst.Write(buf[:n])
			written += int64(nw)
			l.publish(control.AttachmentLocalState{
				AttachmentID:    attachment.AttachmentID,
				State:           control.AttachmentLocalDownloading,
				BytesDownloaded: offset + written,
				TotalBytes:      attachment.SizeBytes,
				SizeBytes:       attachment.SizeBytes,
			})
			if writeErr != nil {
				return written, fmt.Errorf("write partial attachment: %w", writeErr)
			}
			if nw != n {
				return written, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return written, nil
			}
			return written, fmt.Errorf("read attachment response: %w", readErr)
		}
	}
}

func (l *Localizer) publish(state control.AttachmentLocalState) {
	l.mu.Lock()
	l.states[state.AttachmentID] = state
	l.mu.Unlock()
	if l.onState != nil {
		l.onState(state)
	}
}

func (l *Localizer) readReady(attachmentID string) (control.AttachmentLocalState, bool) {
	data, err := os.ReadFile(filepath.Join(l.rootDir, attachmentID, manifestFilename))
	if err != nil {
		return control.AttachmentLocalState{}, false
	}
	var manifest readyManifest
	if err := json.Unmarshal(data, &manifest); err != nil || manifest.AttachmentID != attachmentID {
		return control.AttachmentLocalState{}, false
	}
	parsed, err := url.Parse(manifest.LocalURI)
	if err != nil || parsed.Scheme != "file" {
		return control.AttachmentLocalState{}, false
	}
	if _, err := os.Stat(parsed.Path); err != nil {
		return control.AttachmentLocalState{}, false
	}
	return control.AttachmentLocalState{
		AttachmentID:    manifest.AttachmentID,
		State:           control.AttachmentLocalReady,
		BytesDownloaded: manifest.SizeBytes,
		TotalBytes:      manifest.SizeBytes,
		SizeBytes:       manifest.SizeBytes,
		LocalURI:        manifest.LocalURI,
		SHA256:          manifest.SHA256,
	}, true
}

func writeReadyManifest(dir string, manifest readyManifest) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode ready manifest: %w", err)
	}
	tempPath := filepath.Join(dir, manifestFilename+".tmp")
	if err := os.WriteFile(tempPath, data, 0o600); err != nil {
		return fmt.Errorf("write ready manifest: %w", err)
	}
	if err := os.Rename(tempPath, filepath.Join(dir, manifestFilename)); err != nil {
		return fmt.Errorf("publish ready manifest: %w", err)
	}
	return nil
}

func validateCommand(command control.EnsureAttachmentLocalCommand) error {
	if err := validateAttachmentID(command.Attachment.AttachmentID); err != nil {
		return err
	}
	if strings.TrimSpace(command.Download.URL) == "" {
		return errors.New("attachment download URL is required")
	}
	return nil
}

func validateAttachmentID(attachmentID string) error {
	if attachmentID == "" || attachmentID == "." || attachmentID == ".." ||
		filepath.Base(attachmentID) != attachmentID || strings.ContainsAny(attachmentID, `/\`) {
		return errors.New("invalid attachment ID")
	}
	return nil
}

func safeFilename(filename string) string {
	filename = filepath.Base(strings.TrimSpace(filename))
	if filename == "" || filename == "." || filename == ".." || filename == string(filepath.Separator) {
		return "content"
	}
	return filename
}
