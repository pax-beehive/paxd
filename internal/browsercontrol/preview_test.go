package browsercontrol

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"image/jpeg"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

func previewFixture(t *testing.T, w, h uint16, update []byte) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	_ = client.SetDeadline(time.Now().Add(time.Second))
	go func() {
		defer server.Close()
		shared := make([]byte, 1)
		if _, err := io.ReadFull(server, shared); err != nil {
			return
		}
		if shared[0] != 1 {
			t.Error("preview must share desktop")
		}
		init := make([]byte, 24)
		binary.BigEndian.PutUint16(init, w)
		binary.BigEndian.PutUint16(init[2:], h)
		if _, err := server.Write(init); err != nil {
			return
		}
		setup := make([]byte, 38)
		if _, err := io.ReadFull(server, setup); err != nil {
			return
		}
		if setup[0] != 0 || setup[20] != 2 || setup[28] != 3 || setup[29] != 0 {
			t.Error("expected pixel format, raw encoding and full update only")
		}
		_, _ = server.Write(update)
	}()
	return client
}

func TestPreviewIndependentFullFrame(t *testing.T) {
	// Two rectangles in reverse spatial order must produce one complete image.
	update := []byte{0, 0, 0, 2,
		0, 1, 0, 0, 0, 1, 0, 1, 0, 0, 0, 0, 0, 255, 0, 0,
		0, 0, 0, 0, 0, 1, 0, 1, 0, 0, 0, 0, 255, 0, 0, 0}
	frame, err := captureDesktop(previewFixture(t, 2, 1, update))
	if err != nil {
		t.Fatal(err)
	}
	if frame.RGBAAt(0, 0).R != 255 || frame.RGBAAt(1, 0).G != 255 || frame.RGBAAt(1, 0).A != 255 {
		t.Fatal("wrong preview pixels")
	}
}

func TestPreviewRejectsIncompleteAndInvalidData(t *testing.T) {
	cases := map[string][]byte{
		"incomplete":           {0, 0, 0, 1, 0, 0, 0, 0, 0, 1, 0, 1, 0, 0, 0, 0, 255, 0, 0, 0},
		"out of bounds":        {0, 0, 0, 1, 0, 2, 0, 0, 0, 1, 0, 1, 0, 0, 0, 0},
		"unsupported encoding": {0, 0, 0, 1, 0, 0, 0, 0, 0, 2, 0, 1, 0, 0, 0, 7},
		"oversized clipboard":  {3, 0, 0, 0, 0xff, 0xff, 0xff, 0xff},
	}
	for name, update := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := captureDesktop(previewFixture(t, 2, 1, update)); err == nil {
				t.Fatal("invalid update accepted")
			}
		})
	}
}

func TestDockerPreviewRejectsInputActions(t *testing.T) {
	for _, kind := range []string{"click", "key", "takeover"} {
		_, err := (&Client{}).Request(context.Background(), "fixture", "view", json.RawMessage(`{"source":"docker","action":{"type":"`+kind+`"}}`))
		if err == nil {
			t.Fatal("preview accepted input")
		}
	}
}

func TestDockerPreviewLive(t *testing.T) {
	if os.Getenv("PAX_BROWSER_VNC_TEST") != "1" {
		t.Skip("requires local Docker VNC")
	}
	c := &Client{}
	for i := 0; i < 2; i++ {
		raw, err := c.Request(context.Background(), "fixture", "view", json.RawMessage(`{"source":"docker","action":{"type":"screenshot"}}`))
		if err != nil {
			t.Fatal(err)
		}
		var frame struct {
			Image         string
			Width, Height int
		}
		if err = json.Unmarshal(raw, &frame); err != nil {
			t.Fatal(err)
		}
		data, err := base64.StdEncoding.DecodeString(frame.Image)
		if err != nil {
			t.Fatal(err)
		}
		config, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if config.Width != frame.Width || config.Height != frame.Height || frame.Width > 1280 {
			t.Fatal("invalid JPEG size")
		}
		if len(c.vncSessions) != 0 {
			t.Fatal("preview retained interactive connection")
		}
		t.Logf("independent preview %dx%d, %d JPEG bytes", frame.Width, frame.Height, len(data))
	}
}
