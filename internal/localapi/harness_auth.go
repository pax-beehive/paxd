package localapi

import (
	"github.com/pax-beehive/paxd/internal/control"
	"net/http"
)

func (h *Handler) routeHarnessAuthLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var payload control.HarnessAuthLoginCommand
	if !decodeBody(w, r, &payload) {
		return
	}
	if payload.Harness == "" {
		payload.Harness = r.PathValue("harness")
	}
	if payload.Harness != r.PathValue("harness") {
		http.Error(w, "harness path and body must match", http.StatusBadRequest)
		return
	}
	h.handleCommand(w, r, control.Command{CommandID: commandID(r), Type: control.CommandHarnessAuthLogin, HarnessAuthLogin: &payload})
}

func (h *Handler) routeHarnessAuthStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	h.handleQuery(w, r, control.Query{Type: control.QueryHarnessAuthStatus, HarnessAuthStatus: &control.HarnessAuthStatusQuery{Harness: r.PathValue("harness"), SessionID: r.URL.Query().Get("session_id")}})
}
