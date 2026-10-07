package localapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/testkit/controltest"
	"github.com/stretchr/testify/require"
)

func TestHarnessAuthRoutesUseTypedControlAndDisableCaching(t *testing.T) {
	service := controltest.NewMockService(t)
	service.ExpectCommandFrom(control.Source{Kind: control.SourceLocal}, control.Command{CommandID: "auth-start", Type: control.CommandHarnessAuthLogin, HarnessAuthLogin: &control.HarnessAuthLoginCommand{Harness: "claude", Operation: "start"}}).ReturnCommandAck(control.CommandAck{CommandID: "auth-start", OK: true, Status: control.CommandStatusApplied})
	service.ExpectQueryFrom(control.Source{Kind: control.SourceLocal}, control.Query{Type: control.QueryHarnessAuthStatus, HarnessAuthStatus: &control.HarnessAuthStatusQuery{Harness: "claude", SessionID: "session-1"}}).ReturnQueryResult(control.QueryResult{Type: control.QueryHarnessAuthStatus})
	h := NewHandler(service)
	req := httptest.NewRequest(http.MethodPost, "/v1/harnesses/claude/auth/login", strings.NewReader(`{"operation":"start"}`))
	req.Header.Set(commandIDHeader, "auth-start")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/harnesses/claude/auth/status?session_id=session-1", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

func TestHarnessAuthRouteCannotTargetAnotherHarness(t *testing.T) {
	service := controltest.NewMockService(t)
	h := NewHandler(service)
	req := httptest.NewRequest(http.MethodPost, "/v1/harnesses/codex/auth/login", strings.NewReader(`{"harness":"claude","operation":"start"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}
