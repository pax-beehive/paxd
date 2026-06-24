package localapi

import (
	"net/http"

	"github.com/pax-beehive/paxd/internal/control"
)

func NewHandler(service control.Service) http.Handler {
	handler := &Handler{service: service, mux: http.NewServeMux()}
	handler.routes()
	return handler
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.service == nil && !isDocumentationRequest(r) {
		writeControlError(w, http.StatusInternalServerError, control.ControlError{Code: control.ErrCodeInternal, Message: "control service is not configured"})
		return
	}
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) routes() {
	h.mux.HandleFunc("GET /docs", h.routeDocsGet)
	h.mux.HandleFunc("GET /openapi.json", h.routeOpenAPIGet)

	h.mux.HandleFunc("GET /v1/status", h.routeStatusGet)
	h.mux.HandleFunc("GET /v1/remotes", h.routeRemotesList)
	h.mux.HandleFunc("POST /v1/remotes", h.routeRemoteCreate)
	h.mux.HandleFunc("GET /v1/remotes/{id}", h.routeRemoteGet)
	h.mux.HandleFunc("PATCH /v1/remotes/{id}", h.routeRemoteUpdate)
	h.mux.HandleFunc("DELETE /v1/remotes/{id}", h.routeRemoteDelete)
	h.mux.HandleFunc("POST /v1/remotes/{id}/restart", h.routeRemoteRestart)
	h.mux.HandleFunc("PUT /v1/remotes/{id}/auth", h.routeRemoteAuthConfigure)
	h.mux.HandleFunc("DELETE /v1/remotes/{id}/auth", h.routeRemoteAuthClear)

	h.mux.HandleFunc("GET /v1/agent-connections", h.routeAgentConnectionsList)
	h.mux.HandleFunc("POST /v1/agent-connections", h.routeAgentConnectionCreate)
	h.mux.HandleFunc("GET /v1/agent-connections/{id}", h.routeAgentConnectionGet)
	h.mux.HandleFunc("PATCH /v1/agent-connections/{id}", h.routeAgentConnectionUpdate)
	h.mux.HandleFunc("DELETE /v1/agent-connections/{id}", h.routeAgentConnectionDelete)
	h.mux.HandleFunc("POST /v1/agent-connections/{id}/restart", h.routeAgentConnectionRestart)

	h.mux.HandleFunc("GET /v1/harnesses", h.routeHarnessesList)
	h.mux.HandleFunc("POST /v1/harnesses/discover", h.routeHarnessesDiscover)

	h.mux.HandleFunc("GET /v1/local/overview", h.routeLocalOverviewGet)
	h.mux.HandleFunc("GET /v1/local/sessions", h.routeLocalSessionsList)
	h.mux.HandleFunc("POST /v1/local/sessions/sync", h.routeLocalSessionsSync)
	h.mux.HandleFunc("GET /v1/local/sessions/{id}", h.routeLocalSessionGet)

	h.mux.HandleFunc("GET /v1/commands/{id}", h.routeCommandGet)
	h.mux.HandleFunc("/", h.routeNotFound)
}
