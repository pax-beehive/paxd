package e2ee

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	ProtocolVersion = 1
	CipherVersion   = 1
	rootKeyBytes    = 32
	nonceBytes      = 12
)

type Direction string

const (
	DirectionCommand Direction = "command"
	DirectionEvent   Direction = "event"
)

type Envelope struct {
	ProtocolVersion int    `json:"protocol_version"`
	CipherVersion   int    `json:"cipher_version"`
	KeyEpoch        int64  `json:"key_epoch"`
	RecordID        string `json:"record_id"`
	AgentID         string `json:"agent_id"`
	SessionID       string `json:"session_id"`
	Kind            string `json:"kind"`
	Nonce           string `json:"nonce"`
	Payload         string `json:"payload"`
}

type Metadata struct {
	KeyEpoch  int64
	RecordID  string
	AgentID   string
	SessionID string
	Kind      string
}

func Encrypt(rootKey []byte, direction Direction, metadata Metadata, plaintext []byte) (Envelope, error) {
	nonce := make([]byte, nonceBytes)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return Envelope{}, fmt.Errorf("generate E2EE nonce: %w", err)
	}
	return seal(rootKey, direction, metadata, plaintext, nonce)
}

func Decrypt(rootKey []byte, direction Direction, envelope Envelope) ([]byte, error) {
	if err := validateRootKey(rootKey); err != nil {
		return nil, err
	}
	if err := validateEnvelope(envelope); err != nil {
		return nil, err
	}
	nonce, err := base64.StdEncoding.DecodeString(envelope.Nonce)
	if err != nil {
		return nil, fmt.Errorf("decode E2EE nonce: %w", err)
	}
	if len(nonce) != nonceBytes {
		return nil, fmt.Errorf("E2EE nonce must be %d bytes", nonceBytes)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		return nil, fmt.Errorf("decode E2EE payload: %w", err)
	}
	aead, err := newAEAD(rootKey, direction, metadataFromEnvelope(envelope))
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, additionalData(metadataFromEnvelope(envelope)))
	if err != nil {
		return nil, errors.New("authenticate E2EE envelope")
	}
	return plaintext, nil
}

func ParseRootKey(encoded string) ([]byte, error) {
	rootKey, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode E2EE root key: %w", err)
	}
	if err := validateRootKey(rootKey); err != nil {
		return nil, err
	}
	return rootKey, nil
}

func seal(rootKey []byte, direction Direction, metadata Metadata, plaintext []byte, nonce []byte) (Envelope, error) {
	if err := validateRootKey(rootKey); err != nil {
		return Envelope{}, err
	}
	if err := validateMetadata(metadata); err != nil {
		return Envelope{}, err
	}
	if len(nonce) != nonceBytes {
		return Envelope{}, fmt.Errorf("E2EE nonce must be %d bytes", nonceBytes)
	}
	aead, err := newAEAD(rootKey, direction, metadata)
	if err != nil {
		return Envelope{}, err
	}
	envelope := Envelope{
		ProtocolVersion: ProtocolVersion,
		CipherVersion:   CipherVersion,
		KeyEpoch:        metadata.KeyEpoch,
		RecordID:        metadata.RecordID,
		AgentID:         metadata.AgentID,
		SessionID:       metadata.SessionID,
		Kind:            metadata.Kind,
		Nonce:           base64.StdEncoding.EncodeToString(nonce),
	}
	envelope.Payload = base64.StdEncoding.EncodeToString(
		aead.Seal(nil, nonce, plaintext, additionalData(metadata)),
	)
	return envelope, nil
}

func newAEAD(rootKey []byte, direction Direction, metadata Metadata) (cipher.AEAD, error) {
	key, err := deriveKey(rootKey, direction, metadata)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create E2EE cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

func deriveKey(rootKey []byte, direction Direction, metadata Metadata) ([]byte, error) {
	if direction != DirectionCommand && direction != DirectionEvent {
		return nil, fmt.Errorf("unsupported E2EE direction %q", direction)
	}
	if err := validateMetadata(metadata); err != nil {
		return nil, err
	}
	info := []byte("pax/e2ee/v1/" + string(direction) + "\x00" + metadata.AgentID + "\x00" + metadata.SessionID + "\x00" + strconv.FormatInt(metadata.KeyEpoch, 10))
	return hkdfSHA256(rootKey, nil, info, 32), nil
}

func hkdfSHA256(secret []byte, salt []byte, info []byte, length int) []byte {
	if salt == nil {
		salt = make([]byte, sha256.Size)
	}
	extract := hmac.New(sha256.New, salt)
	_, _ = extract.Write(secret)
	prk := extract.Sum(nil)

	result := make([]byte, 0, length)
	var previous []byte
	for counter := byte(1); len(result) < length; counter++ {
		expand := hmac.New(sha256.New, prk)
		_, _ = expand.Write(previous)
		_, _ = expand.Write(info)
		_, _ = expand.Write([]byte{counter})
		previous = expand.Sum(nil)
		result = append(result, previous...)
	}
	return result[:length]
}

func additionalData(metadata Metadata) []byte {
	value, _ := json.Marshal([]any{
		ProtocolVersion,
		CipherVersion,
		strconv.FormatInt(metadata.KeyEpoch, 10),
		metadata.RecordID,
		metadata.AgentID,
		metadata.SessionID,
		metadata.Kind,
	})
	return value
}

func metadataFromEnvelope(envelope Envelope) Metadata {
	return Metadata{
		KeyEpoch:  envelope.KeyEpoch,
		RecordID:  envelope.RecordID,
		AgentID:   envelope.AgentID,
		SessionID: envelope.SessionID,
		Kind:      envelope.Kind,
	}
}

func validateEnvelope(envelope Envelope) error {
	if envelope.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("unsupported E2EE protocol version %d", envelope.ProtocolVersion)
	}
	if envelope.CipherVersion != CipherVersion {
		return fmt.Errorf("unsupported E2EE cipher version %d", envelope.CipherVersion)
	}
	return validateMetadata(metadataFromEnvelope(envelope))
}

func validateMetadata(metadata Metadata) error {
	if metadata.KeyEpoch < 1 || metadata.KeyEpoch > 1<<53-1 {
		return errors.New("E2EE key epoch must be a positive safe integer")
	}
	fields := map[string]string{
		"record_id":  metadata.RecordID,
		"agent_id":   metadata.AgentID,
		"session_id": metadata.SessionID,
		"kind":       metadata.Kind,
	}
	for name, value := range fields {
		if value == "" || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("E2EE %s must be non-empty and cannot contain NUL", name)
		}
	}
	return nil
}

func validateRootKey(rootKey []byte) error {
	if len(rootKey) != rootKeyBytes {
		return fmt.Errorf("E2EE root key must be %d bytes", rootKeyBytes)
	}
	return nil
}
