package browsercontrol

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// Opt-in loopback harness for the real noVNC frontend smoke test. No arbitrary
// proxy or native operator routes are exposed. It stops after ninety seconds.
func TestVNCBrowserHarness(t *testing.T) {
	if os.Getenv("PAX_BROWSER_VNC_HARNESS") != "1" {
		t.Skip("opt-in frontend harness")
	}
	c := &Client{}
	listener, err := net.Listen("tcp", "127.0.0.1:17432")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{ReadHeaderTimeout: 3 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Operation string          `json:"operation"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&req) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		if req.Operation != "vnc_open" && req.Operation != "vnc_exchange" && req.Operation != "vnc_close" {
			http.Error(w, "denied", 403)
			return
		}
		result, err := c.Request(r.Context(), "fixture", req.Operation, req.Payload)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"error": map[string]string{"message": err.Error()}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"browser_control": result}})
	})}
	go server.Serve(listener)
	defer server.Close()
	time.Sleep(90 * time.Second)
}
