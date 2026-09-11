package browsercontrol

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

func TestVNCHandshakeAndReplayFence(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		_, err := server.Write([]byte("RFB 003.008\n"))
		if err != nil {
			done <- err
			return
		}
		version := make([]byte, 12)
		if _, err = io.ReadFull(server, version); err != nil {
			done <- err
			return
		}
		if _, err = server.Write([]byte{1, 2}); err != nil {
			done <- err
			return
		}
		choice := make([]byte, 1)
		if _, err = io.ReadFull(server, choice); err != nil {
			done <- err
			return
		}
		challenge := make([]byte, 16)
		rand.Read(challenge)
		if _, err = server.Write(challenge); err != nil {
			done <- err
			return
		}
		response := make([]byte, 16)
		if _, err = io.ReadFull(server, response); err != nil {
			done <- err
			return
		}
		_, err = server.Write([]byte{0, 0, 0, 0})
		done <- err
	}()
	if err := authenticateVNC(client, []byte("canary")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s := &vncSession{source: "fixture", conn: client, created: time.Now(), timer: time.NewTimer(time.Minute)}
	c := &Client{vncSessions: map[string]*vncSession{"test": s}}
	defer s.timer.Stop()
	p, _ := json.Marshal(vncRequest{ID: "test", Seq: 0, Data: base64.StdEncoding.EncodeToString([]byte("RFB 003.008\n"))})
	if _, err := c.Request(context.Background(), "other-remote", "vnc_exchange", p); err == nil {
		t.Fatal("VNC session crossed remote identity")
	}
	if _, err := c.Request(context.Background(), "fixture", "vnc_exchange", p); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Request(context.Background(), "fixture", "vnc_exchange", p); err == nil {
		t.Fatal("replayed VNC input accepted")
	}
	if !s.closed {
		t.Fatal("ambiguous stream must close")
	}
}

func TestDockerVNCLiveHandshake(t *testing.T) {
	if os.Getenv("PAX_BROWSER_VNC_TEST") != "1" {
		t.Skip("requires local Docker VNC")
	}
	c := &Client{}
	raw, err := c.Request(context.Background(), "fixture", "vnc_open", nil)
	if err != nil {
		t.Fatal(err)
	}
	var opened vncResult
	if err = json.Unmarshal(raw, &opened); err != nil {
		t.Fatal(err)
	}
	defer func() {
		p, _ := json.Marshal(vncRequest{ID: opened.ID})
		c.Request(context.Background(), "fixture", "vnc_close", p)
	}()
	seq := uint64(0)
	exchange := func(data []byte) []byte {
		t.Helper()
		p, _ := json.Marshal(vncRequest{ID: opened.ID, Seq: seq, Data: base64.StdEncoding.EncodeToString(data)})
		seq++
		raw, err := c.Request(context.Background(), "fixture", "vnc_exchange", p)
		if err != nil {
			t.Fatal(err)
		}
		var r vncResult
		if err = json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		out, err := base64.StdEncoding.DecodeString(r.Data)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := exchange([]byte("RFB 003.008\n")); string(got) != string([]byte{1, 1}) {
		t.Fatal("invalid private RFB negotiation")
	}
	if got := exchange([]byte{1}); string(got) != string([]byte{0, 0, 0, 0}) {
		t.Fatal("invalid private RFB security result")
	}
	init := exchange([]byte{1})
	until := time.Now().Add(3 * time.Second)
	for len(init) < 24 && time.Now().Before(until) {
		time.Sleep(20 * time.Millisecond)
		init = append(init, exchange(nil)...)
	}
	if len(init) < 24 {
		t.Fatal("no desktop initialization")
	}
	width, height := binary.BigEndian.Uint16(init[:2]), binary.BigEndian.Uint16(init[2:4])
	if width < 640 || height < 480 {
		t.Fatal("unexpected desktop size")
	}
	t.Logf("Docker desktop initialized: %dx%d; credentials stayed on node", width, height)
}
