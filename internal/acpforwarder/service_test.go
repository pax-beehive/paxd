package acpforwarder

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	_ "github.com/mattn/go-sqlite3"
	"github.com/pax-beehive/paxd/internal/store"
)

func TestTunnelURLFromHTTP(t *testing.T) {
	tests := []struct {
		name       string
		base       string
		tunnelPath string
		want       string
	}{
		{
			name:       "https",
			base:       "https://fleet.example.com",
			tunnelPath: "/api/v1/agent/tunnel",
			want:       "wss://fleet.example.com/api/v1/agent/tunnel",
		},
		{
			name:       "http with path",
			base:       "http://localhost:8080/base/",
			tunnelPath: "tunnel",
			want:       "ws://localhost:8080/base/tunnel",
		},
		{
			name:       "already websocket",
			base:       "wss://fleet.example.com",
			tunnelPath: "",
			want:       "wss://fleet.example.com/api/v1/agent/tunnel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tunnelURLFromHTTP(tt.base, tt.tunnelPath)
			if err != nil {
				t.Fatalf("tunnelURLFromHTTP() error = %v", err)
			}
			if got.String() != tt.want {
				t.Fatalf("tunnelURLFromHTTP() = %q, want %q", got.String(), tt.want)
			}
		})
	}
}

func TestTunnelURLFromHTTPRejectsUnsupportedScheme(t *testing.T) {
	if _, err := tunnelURLFromHTTP("ftp://fleet.example.com", "/tunnel"); err == nil {
		t.Fatal("expected unsupported scheme error")
	}
}

func TestNextReconnectBackoffDoublesFailedConnections(t *testing.T) {
	initial := 2 * time.Second
	got := nextReconnectBackoff(initial, initial, false)
	if got != 4*time.Second {
		t.Fatalf("next backoff = %s, want 4s", got)
	}

	got = nextReconnectBackoff(20*time.Second, initial, false)
	if got != maxReconnectBackoff {
		t.Fatalf("capped backoff = %s, want %s", got, maxReconnectBackoff)
	}
}

func TestNextReconnectBackoffResetsAfterConnectedTunnel(t *testing.T) {
	initial := 2 * time.Second
	got := nextReconnectBackoff(maxReconnectBackoff, initial, true)
	if got != initial {
		t.Fatalf("reset backoff = %s, want %s", got, initial)
	}
}

func TestProjectTransportMessageAggregatesDeltasIntoOnePart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "paxd.db")
	journal, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer journal.Close()

	first := json.RawMessage(`{
		"jsonrpc":"2.0",
		"method":"session/update",
		"params":{
			"sessionId":"sess-1",
			"role":"assistant",
			"delta":"h"
		}
	}`)
	second := json.RawMessage(`{
		"jsonrpc":"2.0",
		"method":"session/update",
		"params":{
			"sessionId":"sess-1",
			"role":"assistant",
			"delta":"i"
		}
	}`)
	rpcResponse := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`)
	if err := projectTransportMessage(journal, "agent-1", store.TransportStreamPaxdToManager, 1, first); err != nil {
		t.Fatalf("project first delta: %v", err)
	}
	if err := projectTransportMessage(journal, "agent-1", store.TransportStreamPaxdToManager, 2, second); err != nil {
		t.Fatalf("project second delta: %v", err)
	}
	if err := projectTransportMessage(journal, "agent-1", store.TransportStreamPaxdToManager, 3, rpcResponse); err != nil {
		t.Fatalf("project rpc response: %v", err)
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open sqlite for assertion: %v", err)
	}
	defer db.Close()
	var text string
	var count int
	if err := db.QueryRow(`
		SELECT text FROM message_parts ORDER BY id LIMIT 1
	`).Scan(&text); err != nil {
		t.Fatalf("read projected part: %v", err)
	}
	if text != "hi" {
		t.Fatalf("projected text = %q, want hi", text)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM message_parts`).Scan(&count); err != nil {
		t.Fatalf("count projected parts: %v", err)
	}
	if count != 1 {
		t.Fatalf("projected parts = %d, want 1", count)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE message_id LIKE '%rpc:%'`).Scan(&count); err != nil {
		t.Fatalf("count rpc-derived messages: %v", err)
	}
	if count != 0 {
		t.Fatalf("rpc-derived messages = %d, want 0", count)
	}
}

func TestValidateRejectsMissingACPCommandExecutable(t *testing.T) {
	s := New(Config{
		CloudURL: "https://fleet.example.com",
		APIKey:   "pax_key",
		Command:  []string{"definitely-not-a-real-paxd-acp-command"},
		Journal:  openTestJournal(t),
	})
	defer s.cfg.Journal.Close()

	err := s.validate()
	if err == nil {
		t.Fatal("validate() error = nil, want missing executable error")
	}
	if !strings.Contains(err.Error(), "not found in PATH") {
		t.Fatalf("validate() error = %v", err)
	}
}

func TestTunnelEnvelopeUnwrapsManagerToPaxdPayload(t *testing.T) {
	payload := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	frame, err := json.Marshal(tunnelEnvelope{
		Type:    tunnelTypeData,
		Stream:  tunnelStreamManagerToPaxd,
		Seq:     7,
		Payload: json.RawMessage(payload),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, ok, err := unwrapManagerToPaxd(frame)
	if err != nil {
		t.Fatalf("unwrapManagerToPaxd() error = %v", err)
	}
	if !ok {
		t.Fatal("unwrapManagerToPaxd() ok = false")
	}
	if string(got) != string(payload) {
		t.Fatalf("payload = %s, want %s", got, payload)
	}
}

func TestTunnelEnvelopeUnwrapDecisionTable(t *testing.T) {
	payload := json.RawMessage(`{"jsonrpc":"2.0","id":1}`)
	cases := []struct {
		name    string
		env     tunnelEnvelope
		wantOK  bool
		wantErr string
	}{
		{
			name: "valid data",
			env: tunnelEnvelope{
				Type:    tunnelTypeData,
				Stream:  tunnelStreamManagerToPaxd,
				Seq:     1,
				Payload: payload,
			},
			wantOK: true,
		},
		{
			name: "ack is ignored by data unwrap",
			env: tunnelEnvelope{
				Type:   tunnelTypeAck,
				Stream: tunnelStreamManagerToPaxd,
				Seq:    1,
			},
		},
		{
			name: "wrong stream errors",
			env: tunnelEnvelope{
				Type:    tunnelTypeData,
				Stream:  tunnelStreamPaxdToManager,
				Seq:     1,
				Payload: payload,
			},
			wantErr: "unexpected tunnel stream",
		},
		{
			name: "non-positive seq errors",
			env: tunnelEnvelope{
				Type:    tunnelTypeData,
				Stream:  tunnelStreamManagerToPaxd,
				Seq:     0,
				Payload: payload,
			},
			wantErr: "invalid tunnel seq",
		},
		{
			name: "missing payload errors",
			env: tunnelEnvelope{
				Type:   tunnelTypeData,
				Stream: tunnelStreamManagerToPaxd,
				Seq:    1,
			},
			wantErr: "missing tunnel payload",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := unwrapManagerToPaxdEnvelope(tc.env)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error = %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if tc.wantOK && string(got) != string(payload) {
				t.Fatalf("payload = %s, want %s", got, payload)
			}
		})
	}
}

func TestTunnelEnvelopeWrapsPaxdToManagerPayload(t *testing.T) {
	payload := []byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`)

	frame, err := wrapPaxdToManager(3, payload)
	if err != nil {
		t.Fatalf("wrapPaxdToManager() error = %v", err)
	}

	var env tunnelEnvelope
	if err := json.Unmarshal(frame, &env); err != nil {
		t.Fatalf("decode wrapped frame: %v", err)
	}
	if env.Type != tunnelTypeData || env.Stream != tunnelStreamPaxdToManager || env.Seq != 3 {
		t.Fatalf("wrapped envelope = %+v", env)
	}
	if string(env.Payload) != string(payload) {
		t.Fatalf("payload = %s, want %s", env.Payload, payload)
	}
}

func TestCopyWSToStdinDuplicateInboundAcksWithoutDuplicateDispatch(t *testing.T) {
	journal := openTestJournal(t)
	defer journal.Close()
	svc := New(Config{
		CloudURL: "https://fleet.example.com",
		APIKey:   "pax_key",
		Command:  []string{"test-acp"},
		AgentID:  "agent_1",
		Journal:  journal,
	})
	stdin := &recordingWriteCloser{written: make(chan []byte, 4)}
	var writeMu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		err = svc.copyWSToStdin(conn, stdin, &writeMu)
		if err != nil && !strings.Contains(err.Error(), "close") {
			t.Errorf("copyWSToStdin() error = %v", err)
		}
	}))
	defer server.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	payload := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	frame, err := json.Marshal(tunnelEnvelope{
		Type:    tunnelTypeData,
		Stream:  tunnelStreamManagerToPaxd,
		Seq:     7,
		Payload: json.RawMessage(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := client.WriteMessage(websocket.TextMessage, frame); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
		_, ackBytes, err := client.ReadMessage()
		if err != nil {
			t.Fatalf("read ack %d: %v", i, err)
		}
		var ack tunnelEnvelope
		if err := json.Unmarshal(ackBytes, &ack); err != nil {
			t.Fatalf("decode ack %d: %v", i, err)
		}
		if ack.Type != tunnelTypeAck || ack.Stream != tunnelStreamManagerToPaxd || ack.Seq != 7 {
			t.Fatalf("ack %d = %+v", i, ack)
		}
	}

	select {
	case got := <-stdin.written:
		if string(bytes.TrimSpace(got)) != string(payload) {
			t.Fatalf("stdin payload = %s, want %s", got, payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first stdin write")
	}
	select {
	case got := <-stdin.written:
		t.Fatalf("duplicate dispatch wrote stdin payload %s", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCopyWSToStdinPersistsInboundFrameAndAcks(t *testing.T) {
	journal := openTestJournal(t)
	defer journal.Close()
	svc := New(Config{
		CloudURL: "https://fleet.example.com",
		APIKey:   "pax_key",
		Command:  []string{"test-acp"},
		AgentID:  "agent_1",
		Journal:  journal,
	})
	stdin := &recordingWriteCloser{written: make(chan []byte, 2)}
	var writeMu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		err = svc.copyWSToStdin(conn, stdin, &writeMu)
		if err != nil && !strings.Contains(err.Error(), "close") {
			t.Errorf("copyWSToStdin() error = %v", err)
		}
	}))
	defer server.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	payload := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	frame, err := json.Marshal(tunnelEnvelope{
		Type:    tunnelTypeData,
		Stream:  tunnelStreamManagerToPaxd,
		Seq:     9,
		Payload: json.RawMessage(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WriteMessage(websocket.TextMessage, frame); err != nil {
		t.Fatalf("write frame: %v", err)
	}

	_, ackBytes, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	var ack tunnelEnvelope
	if err := json.Unmarshal(ackBytes, &ack); err != nil {
		t.Fatalf("decode ack: %v", err)
	}
	if ack.Type != tunnelTypeAck || ack.Stream != tunnelStreamManagerToPaxd || ack.Seq != 9 {
		t.Fatalf("ack = %+v", ack)
	}

	select {
	case got := <-stdin.written:
		if string(bytes.TrimSpace(got)) != string(payload) {
			t.Fatalf("stdin payload = %s, want %s", got, payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stdin write")
	}

	stored, err := journal.GetTransportFrame(
		"agent_1",
		store.TransportStreamManagerToPaxd,
		9,
		store.TransportDirectionInbound,
	)
	if err != nil {
		t.Fatalf("GetTransportFrame() error = %v", err)
	}
	if stored == nil || stored.Status != store.TransportStatusApplied || stored.PayloadJSON != string(payload) {
		t.Fatalf("stored frame = %+v", stored)
	}
}

func TestCopyWSToStdinWriteFailureLeavesInboundReceived(t *testing.T) {
	journal := openTestJournal(t)
	defer journal.Close()
	svc := New(Config{
		CloudURL: "https://fleet.example.com",
		APIKey:   "pax_key",
		Command:  []string{"test-acp"},
		AgentID:  "agent_1",
		Journal:  journal,
	})
	stdin := &failingWriteCloser{}
	var writeMu sync.Mutex
	errCh := make(chan error, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		errCh <- svc.copyWSToStdin(conn, stdin, &writeMu)
	}))
	defer server.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	payload := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	frame, err := json.Marshal(tunnelEnvelope{
		Type:    tunnelTypeData,
		Stream:  tunnelStreamManagerToPaxd,
		Seq:     11,
		Payload: json.RawMessage(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WriteMessage(websocket.TextMessage, frame); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	if _, _, err := client.ReadMessage(); err != nil {
		t.Fatalf("read ack: %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "write acp stdin") {
			t.Fatalf("copyWSToStdin() error = %v, want stdin write failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for copyWSToStdin error")
	}

	stored, err := journal.GetTransportFrame(
		"agent_1",
		store.TransportStreamManagerToPaxd,
		11,
		store.TransportDirectionInbound,
	)
	if err != nil {
		t.Fatalf("GetTransportFrame() error = %v", err)
	}
	if stored == nil || stored.Status != store.TransportStatusReceived || stored.AppliedAt != "" {
		t.Fatalf("stored frame = %+v, want received and not applied", stored)
	}
}

func TestCopyWSToStdinAckUpdatesOnlyPaxdToManagerOutbound(t *testing.T) {
	journal := openTestJournal(t)
	defer journal.Close()
	svc := New(Config{
		CloudURL: "https://fleet.example.com",
		APIKey:   "pax_key",
		Command:  []string{"test-acp"},
		AgentID:  "agent_1",
		Journal:  journal,
	})
	for seq := int64(1); seq <= 2; seq++ {
		if err := journal.SaveTransportFrame(&store.TransportFrame{
			AgentID:        "agent_1",
			Stream:         store.TransportStreamPaxdToManager,
			Seq:            seq,
			LocalDirection: store.TransportDirectionOutbound,
			PayloadJSON:    `{"jsonrpc":"2.0"}`,
			Status:         store.TransportStatusSent,
		}); err != nil {
			t.Fatalf("SaveTransportFrame outbound %d: %v", seq, err)
		}
	}
	if err := journal.SaveTransportFrame(&store.TransportFrame{
		AgentID:        "agent_1",
		Stream:         store.TransportStreamManagerToPaxd,
		Seq:            1,
		LocalDirection: store.TransportDirectionOutbound,
		PayloadJSON:    `{"jsonrpc":"2.0"}`,
		Status:         store.TransportStatusSent,
	}); err != nil {
		t.Fatalf("SaveTransportFrame wrong stream: %v", err)
	}

	var writeMu sync.Mutex
	errCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		errCh <- svc.copyWSToStdin(conn, &recordingWriteCloser{written: make(chan []byte, 1)}, &writeMu)
	}))
	defer server.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ack, err := json.Marshal(tunnelEnvelope{
		Type:   tunnelTypeAck,
		Stream: tunnelStreamPaxdToManager,
		Seq:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WriteMessage(websocket.TextMessage, ack); err != nil {
		t.Fatalf("write ack: %v", err)
	}
	_ = client.Close()
	<-errCh

	got1, err := journal.GetTransportFrame("agent_1", store.TransportStreamPaxdToManager, 1, store.TransportDirectionOutbound)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := journal.GetTransportFrame("agent_1", store.TransportStreamPaxdToManager, 2, store.TransportDirectionOutbound)
	if err != nil {
		t.Fatal(err)
	}
	wrongStream, err := journal.GetTransportFrame("agent_1", store.TransportStreamManagerToPaxd, 1, store.TransportDirectionOutbound)
	if err != nil {
		t.Fatal(err)
	}
	if got1.Status != store.TransportStatusAcked {
		t.Fatalf("seq 1 status = %q, want acked", got1.Status)
	}
	if got2.Status != store.TransportStatusSent {
		t.Fatalf("seq 2 status = %q, want sent", got2.Status)
	}
	if wrongStream.Status != store.TransportStatusSent {
		t.Fatalf("wrong stream status = %q, want sent", wrongStream.Status)
	}
}

func TestCopyStdoutToWSPersistsOutboundFrame(t *testing.T) {
	journal := openTestJournal(t)
	defer journal.Close()
	svc := New(Config{
		CloudURL: "https://fleet.example.com",
		APIKey:   "pax_key",
		Command:  []string{"test-acp"},
		AgentID:  "agent_1",
		Journal:  journal,
	})
	var writeMu sync.Mutex
	payload := []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		if err := svc.copyStdoutToWS(bytes.NewReader(append(payload, '\n')), conn, &writeMu); err != nil {
			t.Errorf("copyStdoutToWS() error = %v", err)
		}
	}))
	defer server.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	_, frameBytes, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var env tunnelEnvelope
	if err := json.Unmarshal(frameBytes, &env); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if env.Type != tunnelTypeData || env.Stream != tunnelStreamPaxdToManager ||
		env.Seq != 1 || string(env.Payload) != string(payload) {
		t.Fatalf("frame = %+v", env)
	}

	stored, err := journal.GetTransportFrame(
		"agent_1",
		store.TransportStreamPaxdToManager,
		1,
		store.TransportDirectionOutbound,
	)
	if err != nil {
		t.Fatalf("GetTransportFrame() error = %v", err)
	}
	if stored == nil || stored.Status != store.TransportStatusSent || stored.PayloadJSON != string(payload) {
		t.Fatalf("stored frame = %+v", stored)
	}
}

var testUpgrader = websocket.Upgrader{}

type recordingWriteCloser struct {
	written chan []byte
}

func (w *recordingWriteCloser) Write(p []byte) (int, error) {
	if len(bytes.TrimSpace(p)) == 0 {
		return len(p), nil
	}
	cp := append([]byte(nil), p...)
	select {
	case w.written <- cp:
	default:
	}
	return len(p), nil
}

func (w *recordingWriteCloser) Close() error {
	return nil
}

type failingWriteCloser struct{}

func (w *failingWriteCloser) Write([]byte) (int, error) {
	return 0, errTestWriteFailure
}

func (w *failingWriteCloser) Close() error {
	return nil
}

var errTestWriteFailure = &testError{message: "test write failure"}

type testError struct {
	message string
}

func (e *testError) Error() string {
	return e.message
}

func openTestJournal(t *testing.T) *store.Store {
	t.Helper()
	journal, err := store.Open(filepath.Join(t.TempDir(), "paxd.db"))
	if err != nil {
		t.Fatalf("open test journal: %v", err)
	}
	return journal
}
