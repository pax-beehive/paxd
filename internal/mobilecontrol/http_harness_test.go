package mobilecontrol

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestMobileHTTPHarness is opt-in and never compiled into the daemon. The
// private Unix socket lets a Node-based browser fixture call the real adapter
// without publishing an unauthenticated TCP operator service.
func TestMobileHTTPHarness(t *testing.T) {
	socket := os.Getenv("PAX_MOBILE_HARNESS_SOCKET")
	if socket == "" {
		t.Skip("set PAX_MOBILE_HARNESS_SOCKET for the private frontend harness")
	}
	if !filepath.IsAbs(socket) {
		t.Fatal("harness socket must be absolute")
	}
	parent, err := os.Lstat(filepath.Dir(socket))
	if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 || parent.Mode().Perm()&0077 != 0 {
		t.Fatal("harness directory must be private and not a symlink")
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refusing to replace an existing harness socket")
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	client := &Client{}
	closed := make(chan struct{})
	var closeOnce sync.Once
	server := &http.Server{ReadHeaderTimeout: 3 * time.Second, MaxHeaderBytes: 8192, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.RawQuery != "" {
			http.Error(w, "denied", http.StatusNotFound)
			return
		}
		if r.URL.Path == "/shutdown" {
			closeOnce.Do(func() { close(closed) })
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/mobile" {
			http.Error(w, "denied", http.StatusNotFound)
			return
		}
		var request struct {
			Operation string          `json:"operation"`
			Payload   json.RawMessage `json:"payload"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPayload+128))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		result, err := client.Request(r.Context(), "local:pax-console-smoke", request.Operation, request.Payload)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"error": map[string]string{"message": err.Error()}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"mobile_control": result}})
	})}
	defer server.Close()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			closeOnce.Do(func() { close(closed) })
		}
	}()
	t.Log("Private mobile frontend harness is ready")
	select {
	case <-closed:
	case <-time.After(10 * time.Minute):
	}
}
