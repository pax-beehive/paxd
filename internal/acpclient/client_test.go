package acpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestSessionListerList(t *testing.T) {
	lister := SessionLister{
		Command: []string{
			os.Args[0],
			"-test.run=TestHelperProcess",
			"--",
			"acp-fixture",
		},
		Timeout: time.Second,
	}
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")

	sessions, err := lister.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("List() returned %d sessions, want 2", len(sessions))
	}
	if sessions[0].SessionID != "sess-1" || sessions[0].Name != "first" {
		t.Fatalf("first session = %+v", sessions[0])
	}
	if sessions[1].SessionID != "fallback-id" {
		t.Fatalf("second session id = %q, want fallback-id", sessions[1].SessionID)
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	defer os.Exit(0)

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(scanner.Bytes(), &req)
		switch req.Method {
		case "initialize":
			writeFixtureResponse(req.ID, map[string]any{
				"authMethods": []map[string]any{
					{"id": "setup", "type": "terminal"},
					{"id": "oauth-personal", "name": "Log in with Google"},
					{"id": "runtime"},
				},
			})
		case "authenticate":
			writeFixtureResponse(req.ID, map[string]any{})
		case "session/list":
			writeFixtureResponse(req.ID, map[string]any{
				"sessions": []map[string]any{
					{
						"sessionId":      "sess-1",
						"agentType":      "codex",
						"nativeId":       "native-1",
						"name":           "first",
						"lastActive":     "2026-06-16T00:00:00Z",
						"workspaceRoots": []string{"/tmp"},
						"status":         "idle",
					},
					{"id": "fallback-id", "title": "fallback title", "cwd": "/workspace"},
				},
			})
		default:
			fmt.Printf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"not found"}}`+"\n", req.ID)
		}
	}
}

func writeFixtureResponse(id int64, result any) {
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
	fmt.Println(string(payload))
}

func TestFirstNonTerminalAuthMethodSkipsInteractiveMethods(t *testing.T) {
	got := firstNonTerminalAuthMethod([]authMethod{
		{ID: "oauth-personal", Name: "Log in with Google"},
		{ID: "gemini-api-key", Name: "Gemini API key"},
		{ID: "vertex-ai", Name: "Vertex AI"},
		{ID: "hermes-setup", Type: "terminal"},
		{ID: "runtime"},
	})
	if got != "runtime" {
		t.Fatalf("auth method = %q, want runtime", got)
	}
}

func TestDecodeSessionAcceptsTitleAndCWD(t *testing.T) {
	got := decodeSession([]byte(`{"sessionId":"sess","title":"Title","cwd":"/tmp/project","updatedAt":"2026-06-17T00:00:00Z"}`))
	if got.Name != "Title" {
		t.Fatalf("Name = %q, want Title", got.Name)
	}
	if got.ProjectID != "/tmp/project" {
		t.Fatalf("ProjectID = %q, want cwd", got.ProjectID)
	}
	if len(got.WorkspaceRoots) != 1 || got.WorkspaceRoots[0] != "/tmp/project" {
		t.Fatalf("WorkspaceRoots = %+v, want cwd", got.WorkspaceRoots)
	}
}
