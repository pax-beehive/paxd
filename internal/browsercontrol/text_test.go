package browsercontrol

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func textFixture(t *testing.T) (*Client, *vncSession) {
	t.Helper()
	local, remote := net.Pipe()
	t.Cleanup(func() { local.Close(); remote.Close() })
	session := &vncSession{source: "owner", conn: local, created: time.Now(), stage: 2, timer: time.NewTimer(time.Minute)}
	t.Cleanup(func() { session.timer.Stop() })
	return &Client{vncSessions: map[string]*vncSession{"control": session}}, session
}
func textPayload(id string, seq uint64, text string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"source": "docker", "action": map[string]any{"type": "text", "vncID": id, "seq": seq, "text": text}})
	return raw
}
func TestDockerTextRequiresCurrentControlOwnerAndValidText(t *testing.T) {
	c, s := textFixture(t)
	called := 0
	c.dockerTextInput = func(context.Context, string) error { called++; return nil }
	for _, tc := range []struct{ source, id, text string }{
		{"other", "control", "hello"}, {"owner", "missing", "hello"}, {"owner", "control", ""},
		{"owner", "control", strings.Repeat("中", 2001)}, {"owner", "control", "nul\x00text"},
	} {
		if _, err := c.Request(context.Background(), tc.source, "view", textPayload(tc.id, 0, tc.text)); err == nil {
			t.Fatal("invalid text/control request accepted")
		}
	}
	s.stage = 0
	if _, err := c.Request(context.Background(), "owner", "view", textPayload("control", 0, "hello")); err == nil {
		t.Fatal("uninitialized control accepted")
	}
	s.stage = 2
	s.created = time.Now().Add(-31 * time.Minute)
	if _, err := c.Request(context.Background(), "owner", "view", textPayload("control", 0, "hello")); err == nil {
		t.Fatal("expired control accepted")
	}
	if called != 0 {
		t.Fatal("unauthorized input was executed")
	}
}
func TestDockerTextUsesSequenceFenceAndDoesNotReplay(t *testing.T) {
	c, s := textFixture(t)
	var received []string
	c.dockerTextInput = func(ctx context.Context, text string) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("text command needs a deadline")
		}
		received = append(received, text)
		return nil
	}
	text := "aA!中文\n\t$()"
	raw := textPayload("control", 0, text)
	result, err := c.Request(context.Background(), "owner", "view", raw)
	if err != nil || string(result) != `{"ok":true}` {
		t.Fatalf("text failed: %v", err)
	}
	if len(received) != 1 || received[0] != text || s.seq != 1 {
		t.Fatal("text changed or sequence not advanced")
	}
	if _, err = c.Request(context.Background(), "owner", "view", raw); err == nil {
		t.Fatal("replayed text accepted")
	}
	if len(received) != 1 || !s.closed {
		t.Fatal("replay must retire stream without executing text")
	}
}
func TestDockerTextFailureRetiresStreamWithoutLeakingText(t *testing.T) {
	c, s := textFixture(t)
	called := 0
	c.dockerTextInput = func(context.Context, string) error { called++; return errors.New("SECRET_TEXT_SENTINEL") }
	_, err := c.Request(context.Background(), "owner", "view", textPayload("control", 0, "private text"))
	if err == nil || strings.Contains(err.Error(), "SECRET_TEXT_SENTINEL") || called != 1 || !s.closed {
		t.Fatal("failure must be sanitized and retire the stream")
	}
	if _, err = c.Request(context.Background(), "owner", "view", textPayload("control", 1, "private text")); err == nil || called != 1 {
		t.Fatal("failed stream reused")
	}
}

func TestDockerTakeoverRetiresPreviousControllerForKeysAndText(t *testing.T) {
	c, previous := textFixture(t)
	_, next := textFixture(t)
	next.source = "next-owner"
	c.mu.Lock()
	c.replaceVNCControllerLocked("next", next)
	c.mu.Unlock()
	if !previous.closed || len(c.vncSessions) != 1 {
		t.Fatal("takeover must retire the previous controller")
	}
	c.dockerTextInput = func(context.Context, string) error { return nil }
	if _, err := c.Request(context.Background(), "owner", "view", textPayload("control", 0, "old")); err == nil {
		t.Fatal("previous viewer pasted after takeover")
	}
	if _, err := c.Request(context.Background(), "owner", "vnc_exchange", json.RawMessage(`{"id":"control","seq":0,"data":""}`)); err == nil {
		t.Fatal("previous viewer typed after takeover")
	}
	if _, err := c.Request(context.Background(), "next-owner", "view", textPayload("next", 0, "new")); err != nil {
		t.Fatal(err)
	}
}
