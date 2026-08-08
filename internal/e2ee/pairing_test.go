package e2ee

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPairingGivenNodeSeedWhenAgentOrEpochChangesThenRootKeysAreDomainSeparated(t *testing.T) {
	t.Parallel()
	seed := make([]byte, 32)
	for index := range seed {
		seed[index] = byte(index)
	}

	first, err := DeriveAgentRootKey(seed, "agent_1", 1)
	require.NoError(t, err)
	repeated, err := DeriveAgentRootKey(seed, "agent_1", 1)
	require.NoError(t, err)
	otherAgent, err := DeriveAgentRootKey(seed, "agent_2", 1)
	require.NoError(t, err)
	otherEpoch, err := DeriveAgentRootKey(seed, "agent_1", 2)
	require.NoError(t, err)

	assert.Equal(t, first, repeated)
	assert.Len(t, first, 32)
	assert.NotEqual(t, first, otherAgent)
	assert.NotEqual(t, first, otherEpoch)
}

func TestPairingGivenBrowserPublicKeyAndOutOfBandSecretWhenPaxdWrapsThenOnlyBrowserCanOpen(t *testing.T) {
	t.Parallel()
	rootKey := make([]byte, 32)
	for index := range rootKey {
		rootKey[index] = byte(31 - index)
	}
	recipient, err := ecdh.P256().GenerateKey(rand.Reader)
	require.NoError(t, err)
	otherRecipient, err := ecdh.P256().GenerateKey(rand.Reader)
	require.NoError(t, err)
	secret := []byte("0123456789abcdef")
	context := PairingContext{
		PairingID: "pair_1", AgentID: "agent_1", DeviceID: "device_1", KeyEpoch: 1,
	}

	commitment, err := PairingSecretCommitment(secret, context, recipient.PublicKey().Bytes())
	require.NoError(t, err)
	assert.Len(t, commitment, 32)
	wrapped, err := WrapAgentRootKey(
		rootKey, recipient.PublicKey().Bytes(), secret, context, rand.Reader,
	)
	require.NoError(t, err)
	assert.NotContains(t, string(wrapped.Ciphertext), string(rootKey))
	opened, err := UnwrapAgentRootKey(recipient.Bytes(), secret, context, wrapped)
	require.NoError(t, err)
	assert.Equal(t, rootKey, opened)

	_, err = UnwrapAgentRootKey(otherRecipient.Bytes(), secret, context, wrapped)
	require.ErrorContains(t, err, "unwrap")
	tampered := wrapped
	tampered.Ciphertext = append([]byte(nil), wrapped.Ciphertext...)
	tampered.Ciphertext[0] ^= 0xff
	_, err = UnwrapAgentRootKey(recipient.Bytes(), secret, context, tampered)
	require.ErrorContains(t, err, "unwrap")
}

func TestPairingGivenInvalidInputsWhenDerivingOrWrappingThenRejectsThem(t *testing.T) {
	t.Parallel()
	_, err := DeriveAgentRootKey(make([]byte, 31), "agent_1", 1)
	require.Error(t, err)
	_, err = DeriveAgentRootKey(make([]byte, 32), "", 1)
	require.Error(t, err)
	_, err = PairingSecretCommitment([]byte("short"), PairingContext{}, nil)
	require.Error(t, err)
	recipient, keyErr := ecdh.P256().GenerateKey(rand.Reader)
	require.NoError(t, keyErr)
	context := PairingContext{PairingID: "pair_1", AgentID: "agent_1", DeviceID: "device_1", KeyEpoch: 1}
	_, err = WrapAgentRootKey(make([]byte, 31), recipient.PublicKey().Bytes(), []byte("0123456789abcdef"), context, rand.Reader)
	require.Error(t, err)
	_, err = WrapAgentRootKey(make([]byte, 32), recipient.PublicKey().Bytes(), []byte("0123456789abcdef"), context, bytes.NewReader(nil))
	require.ErrorContains(t, err, "wrapping nonce")
	_, err = UnwrapAgentRootKey([]byte("bad"), []byte("0123456789abcdef"), context, WrappedAgentRootKey{})
	require.ErrorContains(t, err, "private key")
	_, err = UnwrapAgentRootKey(recipient.Bytes(), []byte("0123456789abcdef"), context, WrappedAgentRootKey{
		RecipientPublicKey: recipient.PublicKey().Bytes(), SenderEphemeralPublicKey: []byte("bad"),
	})
	require.ErrorContains(t, err, "sender")
	validPublic := recipient.PublicKey().Bytes()
	_, err = pairingPublicContext(PairingContext{PairingID: "pair_1", DeviceID: "device_1", KeyEpoch: 1}, validPublic)
	require.ErrorContains(t, err, "agent_id")
	_, err = pairingPublicContext(PairingContext{PairingID: "pair_1", AgentID: "agent_1", KeyEpoch: 1}, validPublic)
	require.ErrorContains(t, err, "device_id")
	_, err = pairingPublicContext(PairingContext{PairingID: "pair_1", AgentID: "agent_1", DeviceID: "device_1"}, validPublic)
	require.ErrorContains(t, err, "epoch")
	_, err = pairingPackageContext(context, validPublic, []byte("bad"))
	require.ErrorContains(t, err, "sender")
}
