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
	"github.com/pax-beehive/paxd/internal/safehttp"
)

const (
	partialFilename  = "content.part"
	manifestFilename = "ready.json"
)

type Options struct {
	Context context.Context
	RootDir string
	Client  *http.Client
	// OnState is retained for local, unscoped observers. Remote routing must use OnScopedState.
	OnState func(control.AttachmentLocalState)
	// OnScopedState includes the authenticated source that owns each state transition.
	OnScopedState func(control.Source, control.AttachmentLocalState)
}

type Localizer struct {
	ctx           context.Context
	rootDir       string
	client        *http.Client
	onState       func(control.AttachmentLocalState)
	onScopedState func(control.Source, control.AttachmentLocalState)

	mu     sync.Mutex
	states map[attachmentKey]control.AttachmentLocalState
	active map[attachmentKey]bool
}

type readyManifest struct {
	AttachmentID string `json:"attachment_id"`
	SourceKind   string `json:"source_kind"`
	RemoteID     string `json:"remote_id,omitempty"`
	Filename     string `json:"filename"`
	SizeBytes    int64  `json:"size_bytes"`
	SHA256       string `json:"sha256"`
	LocalURI     string `json:"local_uri"`
}

type attachmentKey struct {
	SourceKind   control.SourceKind
	RemoteID     string
	AttachmentID string
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
		ctx:           ctx,
		rootDir:       opts.RootDir,
		client:        client,
		onState:       opts.OnState,
		onScopedState: opts.OnScopedState,
		states:        make(map[attachmentKey]control.AttachmentLocalState),
		active:        make(map[attachmentKey]bool),
	}
}

func (l *Localizer) Ensure(_ context.Context, source control.Source, command control.EnsureAttachmentLocalCommand) error {
	source, err := normalizeSource(source)
	if err != nil {
		return err
	}
	if err := validateCommand(command); err != nil {
		return err
	}
	if ready, ok := l.readReady(source, command.Attachment.AttachmentID); ok {
		l.publish(source, ready)
		return nil
	}

	attachmentID := command.Attachment.AttachmentID
	key := newAttachmentKey(source, attachmentID)
	l.mu.Lock()
	if l.active[key] {
		l.mu.Unlock()
		return nil
	}
	l.active[key] = true
	l.mu.Unlock()

	l.publish(source, control.AttachmentLocalState{
		AttachmentID: attachmentID,
		State:        control.AttachmentLocalQueued,
		TotalBytes:   command.Attachment.SizeBytes,
		SizeBytes:    command.Attachment.SizeBytes,
	})
	go l.download(source, command)
	return nil
}

func (l *Localizer) Status(_ context.Context, source control.Source, attachmentIDs []string) ([]control.AttachmentLocalState, error) {
	source, err := normalizeSource(source)
	if err != nil {
		return nil, err
	}
	result := make([]control.AttachmentLocalState, 0, len(attachmentIDs))
	for _, attachmentID := range attachmentIDs {
		if err := validateAttachmentID(attachmentID); err != nil {
			return nil, err
		}

		l.mu.Lock()
		state, ok := l.states[newAttachmentKey(source, attachmentID)]
		l.mu.Unlock()
		if ok && state.State != control.AttachmentLocalUnknown {
			result = append(result, state)
			continue
		}
		if ready, ok := l.readReady(source, attachmentID); ok {
			result = append(result, ready)
			continue
		}
		partPath := filepath.Join(l.attachmentDir(source, attachmentID), partialFilename)
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

func (l *Localizer) download(source control.Source, command control.EnsureAttachmentLocalCommand) {
	attachment := command.Attachment
	key := newAttachmentKey(source, attachment.AttachmentID)
	defer func() {
		l.mu.Lock()
		delete(l.active, key)
		l.mu.Unlock()
	}()

	if err := l.downloadFile(source, command); err != nil {
		errorCode := "download_failed"
		var integrity *integrityError
		if errors.As(err, &integrity) {
			errorCode = integrity.code
		}
		l.publish(source, control.AttachmentLocalState{
			AttachmentID: attachment.AttachmentID,
			State:        control.AttachmentLocalFailed,
			TotalBytes:   attachment.SizeBytes,
			SizeBytes:    attachment.SizeBytes,
			ErrorCode:    errorCode,
			ErrorMessage: err.Error(),
		})
	}
}

func (l *Localizer) downloadFileAttempt(source control.Source, command control.EnsureAttachmentLocalCommand) error {
	attachment := command.Attachment
	dir := l.attachmentDir(source, attachment.AttachmentID)
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
	if offset > attachment.SizeBytes {
		if err := part.Truncate(0); err != nil {
			return fmt.Errorf("reset oversized partial attachment: %w", err)
		}
		offset = 0
	}
	if offset == attachment.SizeBytes {
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
		return safehttp.RedactError("create attachment download request", err)
	}
	for key, value := range command.Download.Headers {
		req.Header.Set(key, value)
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := safehttp.DoNoRedirect(l.client, req)
	if err != nil {
		return safehttp.RedactError("download attachment", err)
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

	l.publish(source, control.AttachmentLocalState{
		AttachmentID:    attachment.AttachmentID,
		State:           control.AttachmentLocalDownloading,
		BytesDownloaded: offset,
		TotalBytes:      attachment.SizeBytes,
		SizeBytes:       attachment.SizeBytes,
	})
	// Read at most the declared remainder plus one byte. The extra byte is
	// sufficient to prove a size mismatch without allowing a corrupt or
	// malicious object response to grow the daemon's partial file without
	// bound. Attachment sizes are validated as non-negative before this point,
	// and offset is reset above whenever it exceeds the declared size.
	remaining := attachment.SizeBytes - offset
	boundedBody := newRemainingPlusOneReader(resp.Body, remaining)
	written, err := l.copyWithProgress(source, part, boundedBody, attachment, offset)
	if err != nil {
		return err
	}
	total := offset + written
	l.publish(source, control.AttachmentLocalState{
		AttachmentID:    attachment.AttachmentID,
		State:           control.AttachmentLocalVerifying,
		BytesDownloaded: total,
		TotalBytes:      attachment.SizeBytes,
		SizeBytes:       attachment.SizeBytes,
	})
	if err := part.Sync(); err != nil {
		return fmt.Errorf("sync partial attachment: %w", err)
	}
	if total != attachment.SizeBytes {
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
		SourceKind:   string(source.Kind),
		RemoteID:     source.RemoteID,
		Filename:     filename,
		SizeBytes:    total,
		SHA256:       actualSHA256,
		LocalURI:     localURI,
	}
	if err := writeReadyManifest(dir, manifest); err != nil {
		return err
	}
	l.publish(source, control.AttachmentLocalState{
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

func (l *Localizer) copyWithProgress(source control.Source, dst io.Writer, src io.Reader, attachment control.AttachmentDescriptor, offset int64) (int64, error) {
	buf := make([]byte, 256*1024)
	var written int64
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			nw, writeErr := dst.Write(buf[:n])
			written += int64(nw)
			l.publish(source, control.AttachmentLocalState{
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

// remainingPlusOneReader permits the expected remainder and then probes at
// most one additional byte. Keeping the probe as separate state avoids an
// int64 overflow when a caller supplies the largest representable size.
type remainingPlusOneReader struct {
	source       io.Reader
	remaining    int64
	probePending bool
}

func newRemainingPlusOneReader(source io.Reader, remaining int64) io.Reader {
	return &remainingPlusOneReader{
		source:       source,
		remaining:    remaining,
		probePending: true,
	}
}

func (r *remainingPlusOneReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining > 0 {
		if int64(len(p)) > r.remaining {
			p = p[:int(r.remaining)]
		}
		n, err := r.source.Read(p)
		r.remaining -= int64(n)
		return n, err
	}
	if !r.probePending {
		return 0, io.EOF
	}
	n, err := r.source.Read(p[:1])
	if n > 0 || err != nil {
		r.probePending = false
	}
	return n, err
}

func (l *Localizer) publish(source control.Source, state control.AttachmentLocalState) {
	l.mu.Lock()
	l.states[newAttachmentKey(source, state.AttachmentID)] = state
	l.mu.Unlock()
	if l.onState != nil {
		l.onState(state)
	}
	if l.onScopedState != nil {
		l.onScopedState(source, state)
	}
}

func (l *Localizer) readReady(source control.Source, attachmentID string) (control.AttachmentLocalState, bool) {
	dir := l.attachmentDir(source, attachmentID)
	data, err := os.ReadFile(filepath.Join(dir, manifestFilename))
	if err != nil {
		return control.AttachmentLocalState{}, false
	}
	var manifest readyManifest
	if err := json.Unmarshal(data, &manifest); err != nil ||
		manifest.AttachmentID != attachmentID ||
		manifest.SourceKind != string(source.Kind) ||
		manifest.RemoteID != source.RemoteID ||
		manifest.Filename != safeFilename(manifest.Filename) {
		return control.AttachmentLocalState{}, false
	}
	parsed, err := url.Parse(manifest.LocalURI)
	if err != nil || parsed.Scheme != "file" || parsed.Host != "" {
		return control.AttachmentLocalState{}, false
	}
	expectedPath, err := filepath.Abs(filepath.Join(dir, manifest.Filename))
	if err != nil || filepath.Clean(parsed.Path) != filepath.Clean(expectedPath) {
		return control.AttachmentLocalState{}, false
	}
	info, err := os.Lstat(expectedPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() != manifest.SizeBytes {
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
	if command.Attachment.SizeBytes < 0 {
		return errors.New("attachment size cannot be negative")
	}
	return nil
}

func normalizeSource(source control.Source) (control.Source, error) {
	source.RemoteID = strings.TrimSpace(source.RemoteID)
	switch source.Kind {
	case control.SourceLocal:
		if source.RemoteID != "" {
			return control.Source{}, errors.New("local attachment scope cannot include a remote ID")
		}
	case control.SourceRemote:
		if source.RemoteID == "" {
			return control.Source{}, errors.New("remote attachment scope requires a remote ID")
		}
	default:
		return control.Source{}, errors.New("attachment scope must be local or remote")
	}
	return source, nil
}

func newAttachmentKey(source control.Source, attachmentID string) attachmentKey {
	return attachmentKey{
		SourceKind:   source.Kind,
		RemoteID:     source.RemoteID,
		AttachmentID: attachmentID,
	}
}

func (l *Localizer) attachmentDir(source control.Source, attachmentID string) string {
	if source.Kind == control.SourceLocal {
		return filepath.Join(l.rootDir, "local", attachmentID)
	}
	remoteHash := sha256.Sum256([]byte(source.RemoteID))
	return filepath.Join(l.rootDir, "remotes", hex.EncodeToString(remoteHash[:]), attachmentID)
}

func validateAttachmentID(attachmentID string) error {
	if attachmentID == "" || attachmentID == "." || attachmentID == ".." ||
		filepath.Base(attachmentID) != attachmentID || strings.ContainsAny(attachmentID, `/\`) {
		return errors.New("invalid attachment ID")
	}
	return nil
}

func safeFilename(filename string) string {
	filename = strings.TrimSpace(filename)
	if filename == "" || filename == "." || filename == ".." ||
		filepath.Base(filename) != filename || strings.ContainsAny(filename, `/\`) ||
		filename == partialFilename || filename == manifestFilename || filename == manifestFilename+".tmp" {
		return "content"
	}
	return filename
}
