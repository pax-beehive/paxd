package e2ee

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvelopeCompatibilityVector(t *testing.T) {
	t.Parallel()
	rootKey := make([]byte, 32)
	for i := range rootKey {
		rootKey[i] = byte(i)
	}
	metadata := Metadata{
		RecordID:  "cmd_vector_1",
		AgentID:   "agent_vector",
		SessionID: "session_vector",
		Kind:      "acp_command",
		KeyEpoch:  7,
	}
	plaintext := []byte(`{"jsonrpc":"2.0","id":"request_1","method":"session/prompt"}`)

	envelope, err := seal(
		rootKey,
		DirectionCommand,
		metadata,
		plaintext,
		[]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11},
	)
	require.NoError(t, err)
	assert.Equal(t, "AAECAwQFBgcICQoL", envelope.Nonce)
	assert.Equal(t, "GcCQKbHNJjmk1yKKWUQmcLKrS6pV6FGkHGpQi4Td1BeDFtE/OtzFYmyzcBjSxOzaG0ws42u2NTlwXuCv4F42mB0xnD57M53gMNcC9g==", envelope.Payload)

	decrypted, err := Decrypt(rootKey, DirectionCommand, envelope)
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted)
}

func TestEnvelopeRejectsAuthenticatedMetadataChanges(t *testing.T) {
	t.Parallel()
	rootKey := make([]byte, 32)
	envelope, err := Encrypt(rootKey, DirectionEvent, Metadata{
		RecordID:  "evt_1",
		AgentID:   "agent_1",
		SessionID: "session_1",
		Kind:      "acp_event",
		KeyEpoch:  1,
	}, json.RawMessage(`{"secret":true}`))
	require.NoError(t, err)

	envelope.SessionID = "session_2"
	_, err = Decrypt(rootKey, DirectionEvent, envelope)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "authenticate E2EE envelope")
}

func TestEnvelopeUsesDirectionSpecificKeys(t *testing.T) {
	t.Parallel()
	rootKey := make([]byte, 32)
	envelope, err := Encrypt(rootKey, DirectionCommand, Metadata{
		RecordID:  "cmd_1",
		AgentID:   "agent_1",
		SessionID: "session_1",
		Kind:      "acp_command",
		KeyEpoch:  1,
	}, []byte("secret"))
	require.NoError(t, err)

	_, err = Decrypt(rootKey, DirectionEvent, envelope)
	require.Error(t, err)
}

func TestRootKeyParsingAndEnvelopeValidation(t *testing.T) {
	t.Parallel()
	encoded := base64.StdEncoding.EncodeToString(make([]byte, 32))
	rootKey, err := ParseRootKey("  " + encoded + "\n")
	require.NoError(t, err)
	require.Len(t, rootKey, 32)

	_, err = ParseRootKey("not-base64")
	require.ErrorContains(t, err, "decode E2EE root key")
	_, err = ParseRootKey(base64.StdEncoding.EncodeToString([]byte("short")))
	require.ErrorContains(t, err, "32 bytes")

	_, err = Encrypt([]byte("short"), DirectionCommand, Metadata{}, nil)
	require.ErrorContains(t, err, "32 bytes")
	_, err = Encrypt(rootKey, Direction("sideways"), Metadata{
		RecordID: "cmd_1", AgentID: "agent_1", SessionID: "session_1", Kind: "acp_command", KeyEpoch: 1,
	}, nil)
	require.ErrorContains(t, err, "unsupported E2EE direction")
}

func TestDecryptRejectsMalformedEnvelopeFields(t *testing.T) {
	t.Parallel()
	rootKey := make([]byte, 32)
	envelope, err := Encrypt(rootKey, DirectionCommand, Metadata{
		RecordID: "cmd_1", AgentID: "agent_1", SessionID: "session_1", Kind: "acp_command", KeyEpoch: 1,
	}, []byte("secret"))
	require.NoError(t, err)

	invalidVersion := envelope
	invalidVersion.ProtocolVersion = 2
	_, err = Decrypt(rootKey, DirectionCommand, invalidVersion)
	require.ErrorContains(t, err, "protocol version")
	invalidCipher := envelope
	invalidCipher.CipherVersion = 2
	_, err = Decrypt(rootKey, DirectionCommand, invalidCipher)
	require.ErrorContains(t, err, "cipher version")
	invalidNonce := envelope
	invalidNonce.Nonce = "AA=="
	_, err = Decrypt(rootKey, DirectionCommand, invalidNonce)
	require.ErrorContains(t, err, "12 bytes")
	invalidPayload := envelope
	invalidPayload.Payload = "%%%"
	_, err = Decrypt(rootKey, DirectionCommand, invalidPayload)
	require.ErrorContains(t, err, "decode E2EE payload")
	invalidMetadata := envelope
	invalidMetadata.RecordID = ""
	_, err = Decrypt(rootKey, DirectionCommand, invalidMetadata)
	require.ErrorContains(t, err, "record_id")
}
