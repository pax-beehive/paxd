package e2ee

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAttachmentBrowserCompatibility(t *testing.T) {
	a := Attachment{Version: 1, AgentID: "agent_1", SessionID: "session_1", KeyEpoch: 1, AttachmentID: "file_1", SizeBytes: 5, ChunkCount: 1}
	key := bytes.Repeat([]byte{7}, 32)
	ciphertext, err := hex.DecodeString("c7b43e3b2c8d2c8c3889e4fd64e437d0710b4bdbbd")
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, DecryptAttachment(key, a, bytes.NewReader(ciphertext), &out))
	require.Equal(t, "hello", out.String())
}

func TestAttachmentRejectsCorruptionAndWrongContext(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	a := Attachment{Version: 1, AgentID: "agent_1", SessionID: "session_1", KeyEpoch: 1, AttachmentID: "file_1", SizeBytes: AttachmentChunkBytes + 3, ChunkCount: 2}
	plain := bytes.Repeat([]byte{42}, int(a.SizeBytes))
	sealed := sealAttachmentTest(t, key, a, plain)
	for _, test := range []struct {
		name   string
		modify func(*Attachment, []byte) []byte
	}{
		{"tampered", func(_ *Attachment, b []byte) []byte { b[0] ^= 1; return b }},
		{"truncated", func(_ *Attachment, b []byte) []byte { return b[:len(b)-1] }},
		{"trailing", func(_ *Attachment, b []byte) []byte { return append(b, 0) }},
		{"session", func(a *Attachment, b []byte) []byte { a.SessionID = "other"; return b }},
		{"epoch", func(a *Attachment, b []byte) []byte { a.KeyEpoch++; return b }},
		{"size", func(a *Attachment, b []byte) []byte { a.SizeBytes--; return b }},
		{"count", func(a *Attachment, b []byte) []byte { a.ChunkCount--; return b }},
		{"reordered", func(_ *Attachment, b []byte) []byte {
			return append(append([]byte{}, b[AttachmentChunkBytes+16:]...), b[:AttachmentChunkBytes+16]...)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := a
			input := test.modify(&changed, bytes.Clone(sealed))
			var out bytes.Buffer
			require.Error(t, DecryptAttachment(key, changed, bytes.NewReader(input), &out))
		})
	}
	var out bytes.Buffer
	require.NoError(t, DecryptAttachment(key, a, bytes.NewReader(sealed), &out))
	require.Equal(t, plain, out.Bytes())
}

func TestEmptyAttachmentStillAuthenticates(t *testing.T) {
	a := Attachment{Version: 1, AgentID: "agent", SessionID: "session", AttachmentID: "file", KeyEpoch: 1, ChunkCount: 1}
	key := make([]byte, 32)
	sealed := sealAttachmentTest(t, key, a, nil)
	require.Len(t, sealed, 16)
	var out bytes.Buffer
	require.NoError(t, DecryptAttachment(key, a, bytes.NewReader(sealed), &out))
	require.Error(t, DecryptAttachment(key, a, bytes.NewReader(nil), &out))
}

func sealAttachmentTest(t *testing.T, key []byte, a Attachment, plain []byte) []byte {
	t.Helper()
	cipher, err := a.aead(key)
	require.NoError(t, err)
	result := []byte{}
	for i := 0; i < a.ChunkCount; i++ {
		nonce, aad := a.chunkContext(i)
		start := min(i*AttachmentChunkBytes, len(plain))
		end := min((i+1)*AttachmentChunkBytes, len(plain))
		result = append(result, cipher.Seal(nil, nonce, plain[start:end], aad)...)
	}
	return result
}
