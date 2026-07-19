package acpforwarder

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	_ "github.com/mattn/go-sqlite3"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/pax-beehive/paxd/internal/store"
	"github.com/pax-beehive/paxkit/reliablemq"
	"github.com/pax-beehive/paxkit/reliablemq/sqlstore"
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

func TestValidateRejectsMissingACPCommandExecutable(t *testing.T) {
	s := New(Config{
		CloudURL:     "https://fleet.example.com",
		APIKey:       "pax_key",
		ConnectionID: "conn_1",
		Command:      []string{"definitely-not-a-real-paxd-acp-command"},
		Journal:      openTestJournal(t),
		History:      openTestHistory(t, filepath.Join(t.TempDir(), "history.db")),
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

func TestReliableMQEnvelopeUnwrapsACPPayload(t *testing.T) {
	payload := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	frame, err := reliablemq.MarshalEnvelope(reliablemq.Envelope{
		Type:    reliablemq.EnvelopeTypeData,
		QueueID: "conn_1",
		Stream:  reliablemq.StreamACP,
		Seq:     7,
		Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}

	env, err := reliablemq.UnmarshalEnvelope(frame)
	if err != nil {
		t.Fatalf("UnmarshalEnvelope() error = %v", err)
	}
	if env.Type != reliablemq.EnvelopeTypeData || env.QueueID != "conn_1" ||
		env.Stream != reliablemq.StreamACP || env.Seq != 7 ||
		string(env.Payload) != string(payload) {
		t.Fatalf("envelope = %+v", env)
	}
}

func TestReliableMQEnvelopeWrapsPaxdToManagerPayload(t *testing.T) {
	payload := []byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`)
	frame, err := reliablemq.MarshalEnvelope(reliablemq.Envelope{
		Type:     reliablemq.EnvelopeTypeData,
		QueueID:  "conn_1",
		Stream:   reliablemq.StreamACP,
		Seq:      3,
		Metadata: reliablemq.Metadata{"agent_id": "agent_1"},
		Payload:  payload,
	})
	if err != nil {
		t.Fatalf("MarshalEnvelope() error = %v", err)
	}

	env, err := reliablemq.UnmarshalEnvelope(frame)
	if err != nil {
		t.Fatalf("decode wrapped envelope: %v", err)
	}
	if env.Type != reliablemq.EnvelopeTypeData || env.QueueID != "conn_1" ||
		env.Metadata["agent_id"] != "agent_1" ||
		env.Stream != reliablemq.StreamACP || env.Seq != 3 {
		t.Fatalf("wrapped envelope = %+v", env)
	}
	if string(env.Payload) != string(payload) {
		t.Fatalf("payload = %s, want %s", env.Payload, payload)
	}
}

func TestCopyWSToStdinDuplicateInboundAcksWithoutDuplicateDispatch(t *testing.T) {
	journal := openTestJournal(t)
	defer journal.Close()
	history := openTestHistory(t, filepath.Join(t.TempDir(), "history.db"))
	svc := New(Config{
		CloudURL:     "https://fleet.example.com",
		APIKey:       "pax_key",
		Command:      []string{"test-acp"},
		ConnectionID: "conn_1",
		AgentID:      "agent_1",
		Journal:      journal,
		History:      history,
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
	if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}

	payload := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	frame, err := testDataEnvelope(1, payload)
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
		ack := decodeTestEnvelope(t, ackBytes)
		if ack.Type != reliablemq.EnvelopeTypeAck || ack.QueueID != "conn_1" ||
			ack.Stream != reliablemq.StreamACP || ack.Seq != 1 {
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
	history := openTestHistory(t, filepath.Join(t.TempDir(), "history.db"))
	svc := New(Config{
		CloudURL:     "https://fleet.example.com",
		APIKey:       "pax_key",
		Command:      []string{"test-acp"},
		ConnectionID: "conn_1",
		AgentID:      "agent_1",
		Journal:      journal,
		History:      history,
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
	if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}

	payload := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	frame, err := testDataEnvelope(1, payload)
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
	ack := decodeTestEnvelope(t, ackBytes)
	if ack.Type != reliablemq.EnvelopeTypeAck || ack.QueueID != "conn_1" ||
		ack.Stream != reliablemq.StreamACP || ack.Seq != 1 {
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

	stored := waitReliableFrame(t, journal, reliablemq.FrameKey{
		QueueID:   "conn_1",
		Stream:    reliablemq.StreamACP,
		Seq:       1,
		Direction: reliablemq.DirectionInbound,
	}, func(frame reliablemq.Frame) bool {
		return frame.Status == reliablemq.StatusApplied && string(frame.Payload) == string(payload)
	})
	if stored.Status != reliablemq.StatusApplied || string(stored.Payload) != string(payload) {
		t.Fatalf("stored frame = %+v", stored)
	}
}

func TestCopyWSToStdinWriteFailureLeavesInboundReceived(t *testing.T) {
	journal := openTestJournal(t)
	defer journal.Close()
	history := openTestHistory(t, filepath.Join(t.TempDir(), "history.db"))
	svc := New(Config{
		CloudURL:     "https://fleet.example.com",
		APIKey:       "pax_key",
		Command:      []string{"test-acp"},
		ConnectionID: "conn_1",
		AgentID:      "agent_1",
		Journal:      journal,
		History:      history,
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
	if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}

	payload := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	frame, err := testDataEnvelope(1, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WriteMessage(websocket.TextMessage, frame); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	if _, _, err := client.ReadMessage(); err != nil {
		t.Fatalf("read ack: %v", err)
	}
	_ = client.Close()
	select {
	case <-errCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for copyWSToStdin to exit")
	}

	stored := waitReliableFrame(t, journal, reliablemq.FrameKey{
		QueueID:   "conn_1",
		Stream:    reliablemq.StreamACP,
		Seq:       1,
		Direction: reliablemq.DirectionInbound,
	}, func(frame reliablemq.Frame) bool {
		return frame.Status == reliablemq.StatusReceived &&
			strings.Contains(frame.ErrorMessage, "write acp stdin")
	})
	if stored.Status != reliablemq.StatusReceived {
		t.Fatalf("stored frame = %+v, want received and not applied", stored)
	}
	if !strings.Contains(stored.ErrorMessage, "write acp stdin") {
		t.Fatalf("stored error = %q, want stdin failure", stored.ErrorMessage)
	}
}

func TestCopyWSToStdinAckUpdatesOnlyPaxdToManagerOutbound(t *testing.T) {
	journal := openTestJournal(t)
	defer journal.Close()
	history := openTestHistory(t, filepath.Join(t.TempDir(), "history.db"))
	svc := New(Config{
		CloudURL:     "https://fleet.example.com",
		APIKey:       "pax_key",
		Command:      []string{"test-acp"},
		ConnectionID: "conn_1",
		AgentID:      "agent_1",
		Journal:      journal,
		History:      history,
	})
	first := appendSentReliableFrame(t, journal, reliablemq.StreamACP, []byte(`{"jsonrpc":"2.0"}`))
	second := appendSentReliableFrame(t, journal, reliablemq.StreamACP, []byte(`{"jsonrpc":"2.0"}`))
	wrongStreamSeed := appendSentReliableFrame(t, journal, reliablemq.StreamControl, []byte(`{"jsonrpc":"2.0"}`))

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
	ack, err := reliablemq.MarshalEnvelope(reliablemq.AckEnvelope("conn_1", reliablemq.StreamACP, 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WriteMessage(websocket.TextMessage, ack); err != nil {
		t.Fatalf("write ack: %v", err)
	}
	_ = client.Close()
	<-errCh

	got1 := waitReliableFrame(t, journal, first.Key, func(frame reliablemq.Frame) bool {
		return frame.Status == reliablemq.StatusAcked
	})
	got2 := mustGetReliableFrame(t, journal, second.Key)
	wrongStream := mustGetReliableFrame(t, journal, wrongStreamSeed.Key)
	if got1.Status != reliablemq.StatusAcked {
		t.Fatalf("seq 1 status = %q, want acked", got1.Status)
	}
	if got2.Status != reliablemq.StatusSent {
		t.Fatalf("seq 2 status = %q, want sent", got2.Status)
	}
	if wrongStream.Status != reliablemq.StatusSent {
		t.Fatalf("wrong stream status = %q, want sent", wrongStream.Status)
	}
}

func TestCopyStdoutToWSPersistsOutboundFrame(t *testing.T) {
	journal := openTestJournal(t)
	defer journal.Close()
	history := openTestHistory(t, filepath.Join(t.TempDir(), "history.db"))
	svc := New(Config{
		CloudURL:     "https://fleet.example.com",
		APIKey:       "pax_key",
		Command:      []string{"test-acp"},
		ConnectionID: "conn_1",
		AgentID:      "agent_1",
		Journal:      journal,
		History:      history,
	})
	var writeMu sync.Mutex
	payload := []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	stdout, stdoutWriter := io.Pipe()
	defer stdout.Close()
	defer stdoutWriter.Close()
	copyDone := make(chan error, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		copyDone <- svc.copyStdoutToWS(stdout, conn, &writeMu)
	}))
	defer server.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := stdoutWriter.Write(append(payload, '\n'))
		writeDone <- writeErr
	}()
	if err := <-writeDone; err != nil {
		t.Fatalf("write stdout: %v", err)
	}

	_, frameBytes, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	env := decodeTestEnvelope(t, frameBytes)
	if env.Type != reliablemq.EnvelopeTypeData || env.QueueID != "conn_1" ||
		env.Stream != reliablemq.StreamACP ||
		env.Seq != 1 || string(env.Payload) != string(payload) {
		t.Fatalf("frame = %+v", env)
	}
	if err := stdoutWriter.Close(); err != nil {
		t.Fatalf("close stdout: %v", err)
	}
	if err := <-copyDone; err != nil {
		t.Fatalf("copyStdoutToWS() error = %v", err)
	}

	stored := waitReliableFrame(t, journal, reliablemq.FrameKey{
		QueueID:   "conn_1",
		Stream:    reliablemq.StreamACP,
		Seq:       1,
		Direction: reliablemq.DirectionOutbound,
	}, func(frame reliablemq.Frame) bool {
		return frame.Status == reliablemq.StatusPending && string(frame.Payload) == string(payload)
	})
	if stored.Status != reliablemq.StatusPending || string(stored.Payload) != string(payload) {
		t.Fatalf("stored frame = %+v", stored)
	}
}

var testUpgrader = websocket.Upgrader{}

func testDataEnvelope(seq int64, payload []byte) ([]byte, error) {
	return reliablemq.MarshalEnvelope(reliablemq.Envelope{
		Type:     reliablemq.EnvelopeTypeData,
		QueueID:  "conn_1",
		Stream:   reliablemq.StreamACP,
		Seq:      seq,
		Metadata: reliablemq.Metadata{"agent_id": "agent_1"},
		Payload:  append([]byte(nil), payload...),
	})
}

func decodeTestEnvelope(t *testing.T, payload []byte) reliablemq.Envelope {
	t.Helper()
	env, err := reliablemq.UnmarshalEnvelope(payload)
	if err != nil {
		t.Fatalf("decode reliablemq envelope: %v", err)
	}
	return env
}

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

func openTestHistory(t *testing.T, path string) *daemonstore.Store {
	t.Helper()
	history, err := daemonstore.OpenSQLite(path)
	if err != nil {
		t.Fatalf("open test history: %v", err)
	}
	if err := history.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate test history: %v", err)
	}
	return history
}

func openReliableStore(t *testing.T, journal *store.Store) *sqlstore.Store {
	t.Helper()
	mqStore, err := sqlstore.NewSQLite(journal.DB(), sqlstore.WithTableName("transport_journal"))
	if err != nil {
		t.Fatalf("open reliablemq store: %v", err)
	}
	return mqStore
}

func mustGetReliableFrame(t *testing.T, journal *store.Store, key reliablemq.FrameKey) reliablemq.Frame {
	t.Helper()
	frame, ok, err := openReliableStore(t, journal).Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get reliablemq frame: %v", err)
	}
	if !ok {
		t.Fatalf("missing reliablemq frame: %+v", key)
	}
	return frame
}

func waitReliableFrame(
	t *testing.T,
	journal *store.Store,
	key reliablemq.FrameKey,
	accept func(reliablemq.Frame) bool,
) reliablemq.Frame {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	var last reliablemq.Frame
	for time.Now().Before(deadline) {
		frame, ok, err := openReliableStore(t, journal).Get(context.Background(), key)
		if err != nil {
			t.Fatalf("get reliablemq frame: %v", err)
		}
		if ok {
			last = frame
			if accept == nil || accept(frame) {
				return frame
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("reliablemq frame %+v did not reach expected state; last = %+v", key, last)
	return reliablemq.Frame{}
}

func appendSentReliableFrame(
	t *testing.T,
	journal *store.Store,
	stream reliablemq.Stream,
	payload []byte,
) reliablemq.Frame {
	t.Helper()
	mqStore := openReliableStore(t, journal)
	frame, err := mqStore.AppendOutboundData(
		context.Background(),
		"conn_1",
		stream,
		payload,
		reliablemq.Metadata{"agent_id": "agent_1"},
	)
	if err != nil {
		t.Fatalf("append reliablemq outbound frame: %v", err)
	}
	if err := mqStore.MarkSent(context.Background(), frame.Key); err != nil {
		t.Fatalf("mark reliablemq frame sent: %v", err)
	}
	return frame
}
