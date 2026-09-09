// Package secretchannel implements a short-lived, single-use envelope for
// pushing one secret value from a browser to paxd without pax-manager ever
// holding the plaintext or a decryption key.
//
// This is deliberately not the agent E2EE pairing protocol
// (internal/e2ee): there is no session, no device registry, no key epoch.
// paxd generates a fresh ECDH(P-256) keypair per channel, hands out the
// public key, and destroys the private key the moment it is used or expires.
package secretchannel

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	// MaxPlaintextBytes bounds the size of a single pushed secret. This
	// channel is for credentials (passwords, API keys, tokens), not bulk
	// data.
	MaxPlaintextBytes = 64 * 1024

	nonceBytes = 12
	keyBytes   = 32

	protocolVersion = 1
	// purposeLabel is bound into the AAD so a ciphertext produced for this
	// protocol can never be reinterpreted by another one that happens to
	// reuse the same wire shapes.
	purposeLabel = "pax/secret-channel/temporary-file/v1"
	// hkdfInfoLabel is intentionally distinct from the e2ee package's
	// "pax/e2ee/..." labels: this channel must not derive keys under the
	// same label space as agent root key wrapping.
	hkdfInfoLabel = "pax/secret-channel/hkdf/v1"
)

var (
	ErrPlaintextTooLarge = errors.New("secretchannel: plaintext exceeds maximum size")
	ErrInvalidPublicKey  = errors.New("secretchannel: invalid ECDH public key")
	ErrInvalidNonce      = errors.New("secretchannel: invalid nonce length")
	ErrDecryptFailed     = errors.New("secretchannel: decryption failed")
)

// PushContext binds a sealed message to the exact channel, command, and
// expiry it was produced for. It is authenticated (not encrypted) via the
// AES-GCM additional data, so a captured ciphertext cannot be replayed
// against a different channel, a different command_id, or after the
// channel's advertised expiry changes.
type PushContext struct {
	NodeID        string
	ChannelID     string
	CommandID     string
	ExpiresAtUnix int64
}

// SealedMessage is what the browser sends back to paxd (via pax-manager, as
// opaque bytes) after encrypting a secret with the channel's public key.
type SealedMessage struct {
	SenderPublicKey []byte
	Nonce           []byte
	Ciphertext      []byte
}

// SealSecret encrypts plaintext for recipientPublicKey using a fresh
// ephemeral ECDH(P-256) keypair, HKDF-SHA256, and AES-256-GCM. It exists
// primarily so Go tests (and any Go-side tooling) can exercise the exact
// wire format the browser's WebCrypto implementation must produce; paxd
// itself only ever calls OpenSecret.
func SealSecret(recipientPublicKey []byte, plaintext []byte, ctx PushContext, random io.Reader) (SealedMessage, error) {
	if len(plaintext) > MaxPlaintextBytes {
		return SealedMessage{}, ErrPlaintextTooLarge
	}
	if random == nil {
		random = rand.Reader
	}
	curve := ecdh.P256()
	recipient, err := curve.NewPublicKey(recipientPublicKey)
	if err != nil {
		return SealedMessage{}, fmt.Errorf("%w: %v", ErrInvalidPublicKey, err)
	}
	ephemeral, err := curve.GenerateKey(random)
	if err != nil {
		return SealedMessage{}, fmt.Errorf("secretchannel: generate ephemeral key: %w", err)
	}
	shared, err := ephemeral.ECDH(recipient)
	if err != nil {
		return SealedMessage{}, fmt.Errorf("secretchannel: derive shared secret: %w", err)
	}
	nonce := make([]byte, nonceBytes)
	if _, err := io.ReadFull(random, nonce); err != nil {
		return SealedMessage{}, fmt.Errorf("secretchannel: generate nonce: %w", err)
	}
	aead, err := newAEAD(shared)
	if err != nil {
		return SealedMessage{}, err
	}
	aad := additionalData(ctx)
	ciphertext := aead.Seal(nil, nonce, plaintext, aad)
	return SealedMessage{
		SenderPublicKey: ephemeral.PublicKey().Bytes(),
		Nonce:           nonce,
		Ciphertext:      ciphertext,
	}, nil
}

// OpenSecret decrypts a SealedMessage using the channel's recipient private
// key. It fails closed: any mismatch between the message and ctx (wrong
// channel, wrong command_id, wrong expiry) is an authentication failure,
// not just a semantic one, because those fields are bound into the AAD.
func OpenSecret(recipientPrivateKey *ecdh.PrivateKey, msg SealedMessage, ctx PushContext) ([]byte, error) {
	if len(msg.Nonce) != nonceBytes {
		return nil, ErrInvalidNonce
	}
	curve := ecdh.P256()
	sender, err := curve.NewPublicKey(msg.SenderPublicKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPublicKey, err)
	}
	shared, err := recipientPrivateKey.ECDH(sender)
	if err != nil {
		return nil, fmt.Errorf("secretchannel: derive shared secret: %w", err)
	}
	aead, err := newAEAD(shared)
	if err != nil {
		return nil, err
	}
	aad := additionalData(ctx)
	plaintext, err := aead.Open(nil, msg.Nonce, msg.Ciphertext, aad)
	if err != nil {
		return nil, ErrDecryptFailed
	}
	if len(plaintext) > MaxPlaintextBytes {
		return nil, ErrPlaintextTooLarge
	}
	return plaintext, nil
}

func newAEAD(shared []byte) (cipher.AEAD, error) {
	key := hkdfSHA256(shared, nil, []byte(hkdfInfoLabel), keyBytes)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretchannel: create cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// additionalData encodes the binding context as a fixed-order JSON array so
// the wire format does not depend on struct field order or map iteration
// order, and is trivially reproducible from a browser WebCrypto
// implementation encoding the same tuple.
func additionalData(ctx PushContext) []byte {
	value, _ := json.Marshal([]any{
		protocolVersion,
		purposeLabel,
		ctx.NodeID,
		ctx.ChannelID,
		ctx.CommandID,
		ctx.ExpiresAtUnix,
	})
	return value
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
