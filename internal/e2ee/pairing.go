package e2ee

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	pairingSecretMinBytes = 16
	pairingNonceBytes     = 12
)

type PairingContext struct {
	PairingID string `json:"pairing_id"`
	AgentID   string `json:"agent_id"`
	DeviceID  string `json:"device_id"`
	KeyEpoch  int64  `json:"key_epoch"`
}

type WrappedAgentRootKey struct {
	RecipientPublicKey       []byte `json:"recipient_public_key"`
	SenderEphemeralPublicKey []byte `json:"sender_ephemeral_public_key"`
	Nonce                    []byte `json:"nonce"`
	Ciphertext               []byte `json:"ciphertext"`
}

func DeriveAgentRootKey(nodeSeed []byte, agentID string, keyEpoch int64) ([]byte, error) {
	if len(nodeSeed) != rootKeyBytes {
		return nil, fmt.Errorf("E2EE node seed must be %d bytes", rootKeyBytes)
	}
	if err := validatePairingIdentifier("agent_id", agentID); err != nil {
		return nil, err
	}
	if keyEpoch < 1 || keyEpoch > 1<<53-1 {
		return nil, errors.New("E2EE key epoch must be a positive safe integer")
	}
	info, _ := json.Marshal([]any{"pax/e2ee/agent-root/v1", agentID, keyEpoch})
	return hkdfSHA256(nodeSeed, nil, info, rootKeyBytes), nil
}

func PairingSecretCommitment(
	secret []byte,
	context PairingContext,
	recipientPublicKey []byte,
) ([]byte, error) {
	publicContext, err := pairingPublicContext(context, recipientPublicKey)
	if err != nil {
		return nil, err
	}
	if len(secret) < pairingSecretMinBytes {
		return nil, fmt.Errorf("pairing secret must be at least %d bytes", pairingSecretMinBytes)
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte("pax/e2ee/pairing-commitment/v1\x00"))
	_, _ = digest.Write(secret)
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(publicContext)
	return digest.Sum(nil), nil
}

func WrapAgentRootKey(
	rootKey []byte,
	recipientPublicKey []byte,
	secret []byte,
	context PairingContext,
	random io.Reader,
) (WrappedAgentRootKey, error) {
	if err := validateRootKey(rootKey); err != nil {
		return WrappedAgentRootKey{}, err
	}
	if _, err := PairingSecretCommitment(secret, context, recipientPublicKey); err != nil {
		return WrappedAgentRootKey{}, err
	}
	curve := ecdh.P256()
	recipient, err := curve.NewPublicKey(recipientPublicKey)
	if err != nil {
		return WrappedAgentRootKey{}, errors.New("invalid recipient ECDH public key")
	}
	if random == nil {
		random = rand.Reader
	}
	ephemeral, err := curve.GenerateKey(random)
	if err != nil {
		return WrappedAgentRootKey{}, fmt.Errorf("generate wrapping key: %w", err)
	}
	shared, err := ephemeral.ECDH(recipient)
	if err != nil {
		return WrappedAgentRootKey{}, fmt.Errorf("derive wrapping secret: %w", err)
	}
	publicKey := ephemeral.PublicKey().Bytes()
	aad, err := pairingPackageContext(context, recipientPublicKey, publicKey)
	if err != nil {
		return WrappedAgentRootKey{}, err
	}
	aead, err := pairingAEAD(shared, secret, aad)
	if err != nil {
		return WrappedAgentRootKey{}, err
	}
	nonce := make([]byte, pairingNonceBytes)
	if _, err := io.ReadFull(random, nonce); err != nil {
		return WrappedAgentRootKey{}, fmt.Errorf("generate wrapping nonce: %w", err)
	}
	return WrappedAgentRootKey{
		RecipientPublicKey:       append([]byte(nil), recipientPublicKey...),
		SenderEphemeralPublicKey: publicKey,
		Nonce:                    nonce,
		Ciphertext:               aead.Seal(nil, nonce, rootKey, aad),
	}, nil
}

func UnwrapAgentRootKey(
	recipientPrivateKey []byte,
	secret []byte,
	context PairingContext,
	wrapped WrappedAgentRootKey,
) ([]byte, error) {
	curve := ecdh.P256()
	recipient, err := curve.NewPrivateKey(recipientPrivateKey)
	if err != nil {
		return nil, errors.New("invalid recipient ECDH private key")
	}
	if _, err := PairingSecretCommitment(secret, context, recipient.PublicKey().Bytes()); err != nil {
		return nil, err
	}
	if !bytes.Equal(wrapped.RecipientPublicKey, recipient.PublicKey().Bytes()) {
		return nil, errors.New("unwrap E2EE agent root key: recipient mismatch")
	}
	sender, err := curve.NewPublicKey(wrapped.SenderEphemeralPublicKey)
	if err != nil {
		return nil, errors.New("invalid sender ECDH public key")
	}
	if len(wrapped.Nonce) != pairingNonceBytes {
		return nil, errors.New("invalid wrapping nonce")
	}
	shared, err := recipient.ECDH(sender)
	if err != nil {
		return nil, fmt.Errorf("derive wrapping secret: %w", err)
	}
	aad, err := pairingPackageContext(
		context,
		recipient.PublicKey().Bytes(),
		wrapped.SenderEphemeralPublicKey,
	)
	if err != nil {
		return nil, err
	}
	aead, err := pairingAEAD(shared, secret, aad)
	if err != nil {
		return nil, err
	}
	rootKey, err := aead.Open(nil, wrapped.Nonce, wrapped.Ciphertext, aad)
	if err != nil {
		return nil, errors.New("unwrap E2EE agent root key")
	}
	if err := validateRootKey(rootKey); err != nil {
		return nil, errors.New("unwrap E2EE agent root key")
	}
	return rootKey, nil
}

func pairingAEAD(shared []byte, secret []byte, info []byte) (cipher.AEAD, error) {
	salt := sha256.Sum256(secret)
	key := hkdfSHA256(shared, salt[:], info, rootKeyBytes)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func pairingPublicContext(context PairingContext, recipientPublicKey []byte) ([]byte, error) {
	if err := validatePairingContext(context); err != nil {
		return nil, err
	}
	if _, err := ecdh.P256().NewPublicKey(recipientPublicKey); err != nil {
		return nil, errors.New("invalid recipient ECDH public key")
	}
	return json.Marshal([]any{
		"pax/e2ee/pairing-public/v1", context.PairingID, context.AgentID,
		context.DeviceID, context.KeyEpoch, recipientPublicKey,
	})
}

func pairingPackageContext(
	context PairingContext,
	recipientPublicKey []byte,
	senderEphemeralPublicKey []byte,
) ([]byte, error) {
	if _, err := pairingPublicContext(context, recipientPublicKey); err != nil {
		return nil, err
	}
	if _, err := ecdh.P256().NewPublicKey(senderEphemeralPublicKey); err != nil {
		return nil, errors.New("invalid sender ECDH public key")
	}
	return json.Marshal([]any{
		"pax/e2ee/root-package/v1", context.PairingID, context.AgentID,
		context.DeviceID, context.KeyEpoch, recipientPublicKey, senderEphemeralPublicKey,
	})
}

func validatePairingContext(context PairingContext) error {
	if err := validatePairingIdentifier("pairing_id", context.PairingID); err != nil {
		return err
	}
	if err := validatePairingIdentifier("agent_id", context.AgentID); err != nil {
		return err
	}
	if err := validatePairingIdentifier("device_id", context.DeviceID); err != nil {
		return err
	}
	if context.KeyEpoch < 1 || context.KeyEpoch > 1<<53-1 {
		return errors.New("E2EE key epoch must be a positive safe integer")
	}
	return nil
}

func validatePairingIdentifier(name string, value string) error {
	if strings.TrimSpace(value) == "" || strings.ContainsRune(value, '\x00') || len(value) > 512 {
		return fmt.Errorf("E2EE %s is invalid", name)
	}
	return nil
}
