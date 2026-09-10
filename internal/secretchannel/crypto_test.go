package secretchannel

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testContext() PushContext {
	return PushContext{
		NodeID:        "node_1",
		ChannelID:     "chan_1",
		CommandID:     "cmd_1",
		ExpiresAtUnix: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix(),
	}
}

func generateRecipientKey(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	require.NoError(t, err)
	return priv
}

func TestSealOpenRoundTrip(t *testing.T) {
	recipient := generateRecipientKey(t)
	plaintext := []byte("sk-super-secret-value")

	sealed, err := SealSecret(recipient.PublicKey().Bytes(), plaintext, testContext(), rand.Reader)
	require.NoError(t, err)

	opened, err := OpenSecret(recipient, sealed, testContext())
	require.NoError(t, err)
	require.Equal(t, plaintext, opened)
}

func TestOpenRejectsWrongRecipient(t *testing.T) {
	recipient := generateRecipientKey(t)
	otherRecipient := generateRecipientKey(t)

	sealed, err := SealSecret(recipient.PublicKey().Bytes(), []byte("value"), testContext(), rand.Reader)
	require.NoError(t, err)

	_, err = OpenSecret(otherRecipient, sealed, testContext())
	require.Error(t, err)
}

func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	recipient := generateRecipientKey(t)
	sealed, err := SealSecret(recipient.PublicKey().Bytes(), []byte("value"), testContext(), rand.Reader)
	require.NoError(t, err)

	tampered := sealed
	tampered.Ciphertext = append([]byte(nil), sealed.Ciphertext...)
	tampered.Ciphertext[0] ^= 0xFF

	_, err = OpenSecret(recipient, tampered, testContext())
	require.Error(t, err)
}

func TestOpenRejectsMismatchedContext(t *testing.T) {
	recipient := generateRecipientKey(t)
	ctx := testContext()
	sealed, err := SealSecret(recipient.PublicKey().Bytes(), []byte("value"), ctx, rand.Reader)
	require.NoError(t, err)

	wrongChannel := ctx
	wrongChannel.ChannelID = "chan_other"
	_, err = OpenSecret(recipient, sealed, wrongChannel)
	require.Error(t, err, "AAD must bind ciphertext to its channel so it cannot be replayed against another one")

	wrongCommand := ctx
	wrongCommand.CommandID = "cmd_other"
	_, err = OpenSecret(recipient, sealed, wrongCommand)
	require.Error(t, err, "AAD must bind ciphertext to its command_id")

	wrongExpiry := ctx
	wrongExpiry.ExpiresAtUnix++
	_, err = OpenSecret(recipient, sealed, wrongExpiry)
	require.Error(t, err, "AAD must bind ciphertext to the channel's expiry")
}

func TestOpenRejectsWrongNonceLength(t *testing.T) {
	recipient := generateRecipientKey(t)
	sealed, err := SealSecret(recipient.PublicKey().Bytes(), []byte("value"), testContext(), rand.Reader)
	require.NoError(t, err)

	sealed.Nonce = sealed.Nonce[:len(sealed.Nonce)-1]
	_, err = OpenSecret(recipient, sealed, testContext())
	require.Error(t, err)
}

func TestSealRejectsOversizedPlaintext(t *testing.T) {
	recipient := generateRecipientKey(t)
	oversized := bytes.Repeat([]byte("a"), MaxPlaintextBytes+1)

	_, err := SealSecret(recipient.PublicKey().Bytes(), oversized, testContext(), rand.Reader)
	require.ErrorIs(t, err, ErrPlaintextTooLarge)
}

func TestSealProducesDistinctNoncesAndEphemeralKeys(t *testing.T) {
	recipient := generateRecipientKey(t)
	a, err := SealSecret(recipient.PublicKey().Bytes(), []byte("value"), testContext(), rand.Reader)
	require.NoError(t, err)
	b, err := SealSecret(recipient.PublicKey().Bytes(), []byte("value"), testContext(), rand.Reader)
	require.NoError(t, err)

	require.False(t, bytes.Equal(a.Nonce, b.Nonce), "nonces must not repeat across seals")
	require.False(t, bytes.Equal(a.SenderPublicKey, b.SenderPublicKey), "each seal must use a fresh ephemeral keypair")
	require.False(t, bytes.Equal(a.Ciphertext, b.Ciphertext))
}

func TestSealRejectsInvalidRecipientKey(t *testing.T) {
	_, err := SealSecret([]byte("not-a-key"), []byte("value"), testContext(), rand.Reader)
	require.Error(t, err)
}
