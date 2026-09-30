package runtime

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/pax-beehive/paxd/internal/e2ee"
	"github.com/pax-beehive/paxkit/reliablemq"
	"github.com/stretchr/testify/require"
)

func TestE2EEAttachmentLocalizesOnlyAuthenticatedFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sealed, err := hex.DecodeString("c7b43e3b2c8d2c8c3889e4fd64e437d0710b4bdbbd")
	require.NoError(t, err)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(sealed) }))
	defer server.Close()
	previous := http.DefaultClient
	http.DefaultClient = server.Client()
	defer func() { http.DefaultClient = previous }()
	a := e2ee.Attachment{Version: 1, AgentID: "agent_1", SessionID: "session_1", KeyEpoch: 1, AttachmentID: "file_1", SizeBytes: 5, ChunkCount: 1, Filename: "hello.txt", ContentType: "text/plain", DownloadURL: server.URL}
	frame := func(a e2ee.Attachment) []byte {
		b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "prompt_1", "method": "session/prompt", "params": map[string]any{"sessionId": "session_1", "prompt": []any{}, "paxEncryptedAttachments": []e2ee.Attachment{a}}})
		require.NoError(t, err)
		return b
	}
	envelope := e2ee.Envelope{AgentID: "agent_1", SessionID: "session_1", KeyEpoch: 1}
	result, err := localizeE2EEAttachments(context.Background(), frame(a), bytes.Repeat([]byte{7}, 32), envelope)
	require.NoError(t, err)
	require.NotContains(t, string(result), "download_url")
	require.NotContains(t, string(result), "paxEncryptedAttachments")
	var parsed struct {
		Params struct {
			Prompt []struct {
				URI string `json:"uri"`
			} `json:"prompt"`
		} `json:"params"`
	}
	require.NoError(t, json.Unmarshal(result, &parsed))
	uri, err := url.Parse(parsed.Params.Prompt[0].URI)
	require.NoError(t, err)
	data, err := os.ReadFile(uri.Path)
	require.NoError(t, err)
	require.Equal(t, "hello", string(data))
	info, err := os.Stat(uri.Path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	require.NoError(t, os.RemoveAll(filepath.Join(home, ".paxd")))
	sealed[0] ^= 1
	_, err = localizeE2EEAttachments(context.Background(), frame(a), bytes.Repeat([]byte{7}, 32), envelope)
	require.Error(t, err)
	var files []string
	require.NoError(t, filepath.WalkDir(filepath.Join(home, ".paxd"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	}))
	require.Empty(t, files)
	a.SessionID = "other"
	_, err = localizeE2EEAttachments(context.Background(), frame(a), bytes.Repeat([]byte{7}, 32), envelope)
	require.ErrorContains(t, err, "route mismatch")
}

func TestE2EEAttachmentFailureReturnsEncryptedErrorWithoutDispatch(t *testing.T) {
	rootKey := make([]byte, 32)
	receipts := &fakeE2EECommandStore{seen: make(map[string]bool)}
	var event []byte
	bridge := newE2EETransportBridge(rootKey, "agent_1", "queue_1", receipts, func(_ context.Context, payload []byte, meta reliablemq.Metadata) error {
		if meta["e2ee_kind"] == "event" {
			event = bytes.Clone(payload)
		}
		return nil
	})
	command := []byte(`{"jsonrpc":"2.0","id":"prompt_1","method":"session/prompt","params":{"sessionId":"session_1","prompt":[],"paxEncryptedAttachments":[{"version":1,"agent_id":"other","session_id":"session_1","key_epoch":1}]}}`)
	envelope, err := e2ee.Encrypt(rootKey, e2ee.DirectionCommand, e2ee.Metadata{RecordID: "cmd_1", AgentID: "agent_1", SessionID: "session_1", Kind: "acp_command", KeyEpoch: 1}, command)
	require.NoError(t, err)
	payload, err := json.Marshal(envelope)
	require.NoError(t, err)
	handled, err := bridge.handleCommand(context.Background(), reliablemq.Frame{Payload: payload, Metadata: reliablemq.Metadata{"command_id": "cmd_1", "connection_epoch": "1"}}, func(context.Context, string, []byte) (string, error) {
		t.Fatal("invalid attachment was dispatched")
		return "", nil
	})
	require.NoError(t, err)
	require.True(t, handled)
	require.NotContains(t, string(event), "route mismatch")
	var encrypted e2ee.Envelope
	require.NoError(t, json.Unmarshal(event, &encrypted))
	plain, err := e2ee.Decrypt(rootKey, e2ee.DirectionEvent, encrypted)
	require.NoError(t, err)
	require.Contains(t, string(plain), "route mismatch")
	require.Contains(t, string(plain), "prompt_1")
}

func TestE2EEAttachmentOnlyHistoryRetainsResourceLinks(t *testing.T) {
	projector := newE2EEHistoryProjector("agent_1", nil)
	payload := []byte(`{"jsonrpc":"2.0","id":"prompt_1","method":"session/prompt","params":{"sessionId":"session_1","prompt":[{"type":"resource_link","uri":"file:///private/hello.txt","name":"hello.txt","mimeType":"text/plain"}]}}`)
	records, err := projector.projectCommand("session_1", payload)
	require.NoError(t, err)
	require.NotEmpty(t, records)
	found := false
	for _, record := range records {
		if bytes.Contains(record.plaintext, []byte("resource_link")) {
			found = true
		}
	}
	require.True(t, found)
}
