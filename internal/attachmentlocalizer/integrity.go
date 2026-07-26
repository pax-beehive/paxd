package attachmentlocalizer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pax-beehive/paxd/internal/control"
)

type integrityError struct {
	code    string
	message string
}

func (e *integrityError) Error() string {
	return e.message
}

func (l *Localizer) downloadFile(command control.EnsureAttachmentLocalCommand) error {
	var lastIntegrity *integrityError
	for attempt := 0; attempt < 2; attempt++ {
		err := l.downloadFileAttempt(command)
		if err == nil {
			return nil
		}
		if !errors.As(err, &lastIntegrity) {
			return err
		}
		partPath := filepath.Join(
			l.rootDir,
			command.Attachment.AttachmentID,
			partialFilename,
		)
		if removeErr := os.Remove(partPath); removeErr != nil &&
			!errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("discard corrupt partial attachment: %w", removeErr)
		}
		if attempt == 0 {
			l.publish(control.AttachmentLocalState{
				AttachmentID: command.Attachment.AttachmentID,
				State:        control.AttachmentLocalQueued,
				TotalBytes:   command.Attachment.SizeBytes,
				SizeBytes:    command.Attachment.SizeBytes,
			})
		}
	}
	return lastIntegrity
}

func openFileSHA256(file *os.File) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("seek attachment for verification: %w", err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash attachment: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
