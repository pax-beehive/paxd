package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecretaryToolGivenNodeIdentityWhenCalledThenPreservesBridgeEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/node/agents/agent_1/secretary/tools", r.URL.Path)
		assert.Equal(t, "node-test-key", r.Header.Get("X-Pax-Key"))
		assert.Empty(t, r.Header.Get("Authorization"))
		var request SecretaryToolRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		assert.Equal(t, "execution-session", request.SessionID)
		assert.Equal(t, "execution-capability", request.Capability)
		assert.Equal(t, "read", request.Tool)
		assert.JSONEq(t, `{"ref":{"kind":"summary","id":"summary_1","revision":9007199254740993}}`, request.ArgumentsJSON)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":200,"data":{"revision":9007199254740993,"content_hash":"exact"}}`))
	}))
	defer server.Close()
	data, err := NewClient(server.URL, "node-test-key").SecretaryTool(context.Background(), "agent_1", SecretaryToolRequest{
		SessionID: "execution-session", Capability: "execution-capability", Tool: "read", ArgumentsJSON: `{"ref":{"kind":"summary","id":"summary_1","revision":9007199254740993}}`,
	})
	require.NoError(t, err)
	assert.JSONEq(t, `{"revision":9007199254740993,"content_hash":"exact"}`, string(data))
}

func TestSecretaryToolGivenFailureWhenManagerRepliesThenNeverReturnsCredentials(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body, code string
	}{
		{"invalid", 400, `{"code":400,"message":"node-secret cap-secret"}`, "invalid_request"},
		{"unauthenticated", 401, `{"code":401,"message":"node-secret cap-secret"}`, "unauthenticated"},
		{"revoked", 403, `{"code":403,"message":"node-secret cap-secret"}`, "forbidden"},
		{"old Manager", 404, `{"code":404,"message":"not found"}`, "unavailable"},
		{"version unavailable", 409, `{"code":409,"message":"source version unavailable"}`, "version_unavailable"},
		{"version mismatch", 409, `{"code":409,"message":"summary version mismatch"}`, "version_mismatch"},
		{"conflict", 409, `{"code":409,"message":"node-secret cap-secret"}`, "version_conflict"},
		{"rate limited", 429, `{"code":429}`, "rate_limited"},
		{"server failure", 500, `node-secret cap-secret`, "unavailable"},
		{"redirect", 302, `node-secret cap-secret`, "unavailable"},
		{"envelope rejection", 200, `{"code":403,"message":"node-secret cap-secret","data":{}}`, "forbidden"},
		{"malformed response", 200, `node-secret cap-secret`, "invalid_response"},
		{"missing code", 200, `{"data":{}}`, "invalid_response"},
		{"invalid code", 200, `{"code":"200","data":{}}`, "invalid_response"},
		{"missing data", 200, `{"code":200}`, "invalid_response"},
		{"null data", 200, `{"code":200,"data":null}`, "invalid_response"},
		{"oversized response", 200, strings.Repeat("x", secretaryToolMaxBytes+1), "invalid_response"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "/must-not-follow")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			_, err := NewClient(server.URL, "node-secret").SecretaryTool(context.Background(), "agent_1", secretaryTestRequest())
			require.Error(t, err)
			code, message := SecretaryToolFailure(err)
			assert.Equal(t, tc.code, code)
			assert.NotContains(t, message, "node-secret")
			assert.NotContains(t, err.Error(), "cap-secret")
			assert.Equal(t, 1, calls)
		})
	}
}

func secretaryTestRequest() SecretaryToolRequest {
	return SecretaryToolRequest{SessionID: "execution", Capability: "cap-secret", Tool: "search", ArgumentsJSON: `{"query":"data"}`}
}

func TestSecretaryToolGivenInvalidContextWhenCalledThenDoesNotSend(t *testing.T) {
	cases := []struct {
		name, agent string
		change      func(*SecretaryToolRequest)
	}{
		{"empty agent", "", func(*SecretaryToolRequest) {}},
		{"path traversal", "..", func(*SecretaryToolRequest) {}},
		{"agent slash", "agent/foreign", func(*SecretaryToolRequest) {}},
		{"empty caller", "agent", func(r *SecretaryToolRequest) { r.SessionID = "" }},
		{"no capability", "agent", func(r *SecretaryToolRequest) { r.Capability = "" }},
		{"oversized capability", "agent", func(r *SecretaryToolRequest) { r.Capability = strings.Repeat("c", 16385) }},
		{"write tool", "agent", func(r *SecretaryToolRequest) { r.Tool = "publish_summary" }},
		{"malformed arguments", "agent", func(r *SecretaryToolRequest) { r.ArgumentsJSON = "{" }},
		{"array arguments", "agent", func(r *SecretaryToolRequest) { r.ArgumentsJSON = "[]" }},
		{"null arguments", "agent", func(r *SecretaryToolRequest) { r.ArgumentsJSON = "null" }},
		{"oversized arguments", "agent", func(r *SecretaryToolRequest) { r.ArgumentsJSON = strings.Repeat("x", secretaryToolMaxBytes+1) }},
		{"oversized envelope", "agent", func(r *SecretaryToolRequest) {
			raw, _ := json.Marshal(map[string]string{"query": strings.Repeat("x", secretaryToolMaxBytes-32)})
			r.ArgumentsJSON = string(raw)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := secretaryTestRequest()
			tc.change(&request)
			_, err := NewClient("http://127.0.0.1:1", "node-secret").SecretaryTool(context.Background(), tc.agent, request)
			require.Error(t, err)
			code, _ := SecretaryToolFailure(err)
			assert.Equal(t, "invalid_request", code)
		})
	}
}

func TestSecretaryToolGivenCancellationWhenRequestRunningThenStopsHTTP(t *testing.T) {
	for _, duringBody := range []bool{false, true} {
		t.Run(fmt.Sprint("during body ", duringBody), func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if duringBody {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				close(entered)
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer server.Close()
			defer close(release)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := NewClient(server.URL, "node-secret").SecretaryTool(ctx, "agent", secretaryTestRequest())
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("request did not start")
			}
			cancel()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
				code, _ := SecretaryToolFailure(err)
				assert.Equal(t, "canceled", code)
			case <-time.After(3 * time.Second):
				t.Fatal("request did not cancel")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewClient("http://127.0.0.1:1", "node-secret").SecretaryTool(ctx, "agent", secretaryTestRequest())
	assert.ErrorIs(t, err, context.Canceled)
}

func TestSecretaryToolGivenTransportFailureWhenCalledThenMasksDetails(t *testing.T) {
	client := NewClient("http://[invalid", "node-secret")
	_, err := client.SecretaryTool(context.Background(), "agent", secretaryTestRequest())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "[invalid")
	for _, failure := range []error{context.DeadlineExceeded, errors.New("node-secret cap-secret"), &SecretaryToolError{Code: "unsupported", Message: "node-secret"}} {
		code, message := SecretaryToolFailure(failure)
		assert.NotEmpty(t, code)
		assert.NotContains(t, message, "node-secret")
	}
}

func TestSecretaryToolGivenSummaryListWhenCalledThenPreservesFullPage(t *testing.T) {
	response := `{"items":[{"summary_id":"node-1","revision":2,"body":"Complete summary"}],"next_cursor":"next-page"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "node-key", r.Header.Get("X-Pax-Key"))
		var request SecretaryToolRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		assert.Equal(t, "execution-session", request.SessionID)
		assert.Equal(t, "summaries", request.Tool)
		assert.JSONEq(t, `{"session_id":"origin-session","limit":20}`, request.ArgumentsJSON)
		_, _ = w.Write([]byte(`{"code":200,"data":` + response + `}`))
	}))
	defer server.Close()
	data, err := NewClient(server.URL, "node-key").SecretaryTool(t.Context(), "agent", SecretaryToolRequest{
		SessionID: "execution-session", Capability: "cap", Tool: "summaries", ArgumentsJSON: `{"session_id":"origin-session","limit":20}`,
	})
	require.NoError(t, err)
	assert.JSONEq(t, response, string(data))
}
