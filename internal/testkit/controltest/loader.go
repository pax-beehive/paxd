package controltest

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pax-beehive/paxd/internal/control"
)

type LocalAPIRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Command *control.Command  `json:"command,omitempty"`
	Query   *control.Query    `json:"query,omitempty"`
}

type LocalAPIResponse struct {
	Status      int                   `json:"status"`
	Headers     map[string]string     `json:"headers,omitempty"`
	CommandAck  *control.CommandAck   `json:"command_ack,omitempty"`
	QueryResult *control.QueryResult  `json:"query_result,omitempty"`
	Error       *control.ControlError `json:"error,omitempty"`
}

type ControlWSFrame struct {
	Kind        string                `json:"kind"`
	RequestID   string                `json:"request_id,omitempty"`
	CommandID   string                `json:"command_id,omitempty"`
	Command     *control.Command      `json:"command,omitempty"`
	Query       *control.Query        `json:"query,omitempty"`
	CommandAck  *control.CommandAck   `json:"command_ack,omitempty"`
	QueryResult *control.QueryResult  `json:"query_result,omitempty"`
	Error       *control.ControlError `json:"error,omitempty"`
}

func LoadCommandRequest(t testing.TB, name string) control.Command {
	t.Helper()
	path := goldenPath(t, "control", "command_"+name+"_request.json")
	cmd := loadJSON[control.Command](t, path)
	if err := cmd.Validate(); err != nil {
		t.Fatalf("%s: command fixture does not validate: %v", path, err)
	}
	return cmd
}

func LoadCommandResponse(t testing.TB, name string) control.CommandAck {
	t.Helper()
	path := goldenPath(t, "control", "command_"+name+"_response.json")
	ack := loadJSON[control.CommandAck](t, path)
	if strings.TrimSpace(ack.CommandID) == "" {
		t.Fatalf("%s: command response fixture has empty command_id", path)
	}
	if err := ack.Validate(); err != nil {
		t.Fatalf("%s: command response fixture does not validate: %v", path, err)
	}

	requestPath := goldenPath(t, "control", "command_"+name+"_request.json")
	if fileExists(requestPath) {
		cmd := loadJSON[control.Command](t, requestPath)
		if ack.CommandID != cmd.CommandID {
			t.Fatalf("%s: command_id %q does not match request fixture %s command_id %q", path, ack.CommandID, requestPath, cmd.CommandID)
		}
	}
	return ack
}

func LoadQueryRequest(t testing.TB, name string) control.Query {
	t.Helper()
	path := goldenPath(t, "control", "query_"+name+"_request.json")
	query := loadJSON[control.Query](t, path)
	if err := query.Validate(); err != nil {
		t.Fatalf("%s: query fixture does not validate: %v", path, err)
	}
	return query
}

func LoadQueryResponse(t testing.TB, name string) control.QueryResult {
	t.Helper()
	path := goldenPath(t, "control", "query_"+name+"_response.json")
	result := loadJSON[control.QueryResult](t, path)
	if result.Type == "" {
		t.Fatalf("%s: query response fixture has empty type", path)
	}

	requestPath := goldenPath(t, "control", "query_"+name+"_request.json")
	if fileExists(requestPath) {
		query := loadJSON[control.Query](t, requestPath)
		if result.Type != query.Type {
			t.Fatalf("%s: query type %q does not match request fixture %s type %q", path, result.Type, requestPath, query.Type)
		}
	}
	return result
}

func LoadLocalAPIRequest(t testing.TB, name string) LocalAPIRequest {
	t.Helper()
	req := loadJSON[LocalAPIRequest](t, goldenPath(t, "localapi", name+"_request.json"))
	validateLocalAPIRequest(t, req)
	return req
}

func LoadLocalAPIResponse(t testing.TB, name string) LocalAPIResponse {
	t.Helper()
	resp := loadJSON[LocalAPIResponse](t, goldenPath(t, "localapi", name+"_response.json"))
	if resp.Status == 0 {
		t.Fatalf("localapi response fixture %q has empty status", name)
	}
	return resp
}

func LoadControlWSFrameRequest(t testing.TB, name string) ControlWSFrame {
	t.Helper()
	frame := loadJSON[ControlWSFrame](t, goldenPath(t, "controlws", name+"_request.json"))
	validateControlWSFrame(t, name, frame)
	return frame
}

func LoadControlWSFrameResponse(t testing.TB, name string) ControlWSFrame {
	t.Helper()
	frame := loadJSON[ControlWSFrame](t, goldenPath(t, "controlws", name+"_response.json"))
	validateControlWSFrame(t, name, frame)
	return frame
}

func GoldenPath(t testing.TB, layer, file string) string {
	t.Helper()
	return goldenPath(t, layer, file)
}

func loadJSON[T any](t testing.TB, path string) T {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("%s: open fixture: %v", path, err)
	}
	defer file.Close()

	var value T
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("%s: decode fixture: %v", path, err)
	}
	var extra struct{}
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("%s: fixture contains trailing JSON values", path)
	}
	return value
}

func validateLocalAPIRequest(t testing.TB, req LocalAPIRequest) {
	t.Helper()
	if req.Method == "" {
		t.Fatal("localapi request fixture has empty method")
	}
	if req.Path == "" {
		t.Fatal("localapi request fixture has empty path")
	}
	if req.Command != nil && req.Query != nil {
		t.Fatal("localapi request fixture cannot include both command and query")
	}
	if req.Command != nil {
		if err := req.Command.Validate(); err != nil {
			t.Fatalf("localapi command fixture does not validate: %v", err)
		}
	}
	if req.Query != nil {
		if err := req.Query.Validate(); err != nil {
			t.Fatalf("localapi query fixture does not validate: %v", err)
		}
	}
}

func validateControlWSFrame(t testing.TB, name string, frame ControlWSFrame) {
	t.Helper()
	if frame.Kind == "" {
		t.Fatalf("controlws frame fixture %q has empty kind", name)
	}
	if frame.Command != nil && frame.Query != nil {
		t.Fatalf("controlws frame fixture %q cannot include both command and query", name)
	}
	if frame.Command != nil {
		if err := frame.Command.Validate(); err != nil {
			t.Fatalf("controlws command frame fixture %q does not validate: %v", name, err)
		}
	}
	if frame.Query != nil {
		if err := frame.Query.Validate(); err != nil {
			t.Fatalf("controlws query frame fixture %q does not validate: %v", name, err)
		}
	}
}

func goldenPath(t testing.TB, layer, file string) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve controltest source path")
	}
	return filepath.Join(filepath.Dir(source), "testdata", "golden", layer, file)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
