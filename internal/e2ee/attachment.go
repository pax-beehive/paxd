package e2ee

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const AttachmentChunkBytes = 4 * 1024 * 1024
const MaxAttachmentBytes = 512 * 1024 * 1024

// Attachment is carried only inside an authenticated E2EE command.
type Attachment struct {
	Version      int    `json:"version"`
	AgentID      string `json:"agent_id"`
	SessionID    string `json:"session_id"`
	KeyEpoch     int64  `json:"key_epoch"`
	AttachmentID string `json:"attachment_id"`
	Filename     string `json:"filename"`
	ContentType  string `json:"content_type"`
	SizeBytes    int64  `json:"size_bytes"`
	ChunkCount   int    `json:"chunk_count"`
	ObjectID     string `json:"object_id"`
	DownloadURL  string `json:"download_url"`
}

func (a Attachment) Validate() error {
	if a.Version != 1 || a.SizeBytes < 0 || a.SizeBytes > MaxAttachmentBytes || a.KeyEpoch < 1 {
		return errors.New("invalid encrypted attachment parameters")
	}
	for _, value := range []string{a.AgentID, a.SessionID, a.AttachmentID} {
		if value == "" || strings.ContainsRune(value, 0) {
			return errors.New("invalid encrypted attachment identity")
		}
	}
	count := max(1, int((a.SizeBytes+AttachmentChunkBytes-1)/AttachmentChunkBytes))
	if a.ChunkCount != count {
		return errors.New("invalid encrypted attachment chunk count")
	}
	return nil
}

func (a Attachment) CiphertextSize() int64 { return a.SizeBytes + int64(a.ChunkCount)*16 }

func (a Attachment) aead(rootKey []byte) (cipher.AEAD, error) {
	if err := validateRootKey(rootKey); err != nil {
		return nil, err
	}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	info := "pax/e2ee/v1/attachment\x00" + a.AgentID + "\x00" + a.SessionID + "\x00" + strconv.FormatInt(a.KeyEpoch, 10) + "\x00" + a.AttachmentID
	block, err := aes.NewCipher(hkdfSHA256(rootKey, nil, []byte(info), 32))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (a Attachment) chunkContext(index int) ([]byte, []byte) {
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint32(nonce[8:], uint32(index))
	aad, _ := json.Marshal([]any{1, "pax/e2ee/attachment", a.AgentID, a.SessionID, strconv.FormatInt(a.KeyEpoch, 10), a.AttachmentID, a.SizeBytes, a.ChunkCount, index})
	return nonce, aad
}

// DecryptAttachment authenticates every chunk and rejects trailing data. The
// caller must keep the output private until this function has succeeded.
func DecryptAttachment(rootKey []byte, a Attachment, src io.Reader, dst io.Writer) error {
	aead, err := a.aead(rootKey)
	if err != nil {
		return err
	}
	remaining := a.SizeBytes
	for index := 0; index < a.ChunkCount; index++ {
		size := min(remaining, int64(AttachmentChunkBytes))
		ciphertext := make([]byte, int(size)+aead.Overhead())
		if _, err := io.ReadFull(src, ciphertext); err != nil {
			return errors.New("truncated encrypted attachment")
		}
		nonce, aad := a.chunkContext(index)
		plaintext, err := aead.Open(ciphertext[:0], nonce, ciphertext, aad)
		if err != nil {
			return errors.New("encrypted attachment authentication failed")
		}
		if _, err := dst.Write(plaintext); err != nil {
			return fmt.Errorf("write decrypted attachment: %w", err)
		}
		remaining -= size
	}
	var extra [1]byte
	n, err := io.ReadFull(src, extra[:])
	if n != 0 || err != io.EOF {
		return errors.New("unexpected encrypted attachment trailing data")
	}
	return nil
}
