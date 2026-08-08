package localapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/pax-beehive/paxd/internal/control"
)

const commandIDHeader = "X-Pax-Command-Id"

type Handler struct {
	service     control.Service
	e2eePairing E2EEPairingService
	mux         *http.ServeMux
}

type completeE2EEPairingRequest struct {
	AgentID       string `json:"agent_id"`
	PairingSecret string `json:"pairing_secret"`
}

func (h *Handler) routeE2EEPairingComplete(w http.ResponseWriter, r *http.Request) {
	if h.e2eePairing == nil {
		writeControlError(w, http.StatusServiceUnavailable, control.ControlError{
			Code: control.ErrCodeInternal, Message: "E2EE pairing is not configured",
		})
		return
	}
	var payload completeE2EEPairingRequest
	if !decodeBody(w, r, &payload) {
		return
	}
	if strings.TrimSpace(payload.AgentID) == "" ||
		strings.TrimSpace(payload.PairingSecret) == "" ||
		strings.TrimSpace(r.PathValue("pairing_id")) == "" {
		writeControlError(w, http.StatusBadRequest, control.ControlError{
			Code:    control.ErrCodeInvalidArgument,
			Message: "agent_id, pairing_id, and pairing_secret are required",
		})
		return
	}
	result, err := h.e2eePairing.CompletePairing(
		r.Context(), payload.AgentID, r.PathValue("pairing_id"), payload.PairingSecret,
	)
	if err != nil {
		writeControlError(w, http.StatusBadRequest, control.ControlError{
			Code: control.ErrCodeInvalidArgument, Message: err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) routeStatusGet(w http.ResponseWriter, r *http.Request) {
	h.handleQuery(w, r, control.Query{Type: control.QueryStatusGet, GetStatus: &control.GetStatusQuery{}})
}

func (h *Handler) routeDiagnosticsGet(w http.ResponseWriter, r *http.Request) {
	h.handleQuery(w, r, control.Query{Type: control.QueryDiagnosticsGet, GetDiagnostics: &control.GetDiagnosticsQuery{}})
}

func (h *Handler) routeRemotesList(w http.ResponseWriter, r *http.Request) {
	h.handleQuery(w, r, control.Query{Type: control.QueryRemotesList, ListRemotes: &control.ListRemotesQuery{IncludeDisabled: boolQuery(r, "include_disabled")}})
}

func (h *Handler) routeRemoteGet(w http.ResponseWriter, r *http.Request) {
	h.handleQuery(w, r, control.Query{Type: control.QueryRemoteGet, GetRemote: &control.GetRemoteQuery{RemoteID: r.PathValue("id")}})
}

func (h *Handler) routeRemoteCreate(w http.ResponseWriter, r *http.Request) {
	var payload control.CreateRemoteCommand
	if !decodeBody(w, r, &payload) {
		return
	}
	h.handleCommand(w, r, control.Command{CommandID: commandID(r), Type: control.CommandRemoteCreate, CreateRemote: &payload})
}

func (h *Handler) routeRemoteUpdate(w http.ResponseWriter, r *http.Request) {
	var payload control.UpdateRemoteCommand
	if !decodeBody(w, r, &payload) {
		return
	}
	payload.RemoteID = r.PathValue("id")
	h.handleCommand(w, r, control.Command{CommandID: commandID(r), Type: control.CommandRemoteUpdate, UpdateRemote: &payload})
}

func (h *Handler) routeRemoteDelete(w http.ResponseWriter, r *http.Request) {
	h.handleCommand(w, r, control.Command{
		CommandID: commandID(r),
		Type:      control.CommandRemoteDelete,
		DeleteRemote: &control.DeleteRemoteCommand{
			RemoteID:                r.PathValue("id"),
			CascadeAgentConnections: boolQuery(r, "cascade_agent_connections"),
		},
	})
}

func (h *Handler) routeRemoteRestart(w http.ResponseWriter, r *http.Request) {
	h.handleCommand(w, r, control.Command{CommandID: commandID(r), Type: control.CommandRemoteRestart, RestartRemote: &control.RestartRemoteCommand{RemoteID: r.PathValue("id")}})
}

func (h *Handler) routeRemoteAuthConfigure(w http.ResponseWriter, r *http.Request) {
	var payload control.ConfigureRemoteAuthCommand
	if !decodeBody(w, r, &payload) {
		return
	}
	payload.RemoteID = r.PathValue("id")
	h.handleCommand(w, r, control.Command{CommandID: commandID(r), Type: control.CommandRemoteAuthConfigure, ConfigureRemoteAuth: &payload})
}

func (h *Handler) routeRemoteAuthClear(w http.ResponseWriter, r *http.Request) {
	h.handleCommand(w, r, control.Command{CommandID: commandID(r), Type: control.CommandRemoteAuthClear, ClearRemoteAuth: &control.ClearRemoteAuthCommand{RemoteID: r.PathValue("id")}})
}

func (h *Handler) routeAgentConnectionsList(w http.ResponseWriter, r *http.Request) {
	h.handleQuery(w, r, control.Query{
		Type: control.QueryAgentConnectionsList,
		ListAgentConnections: &control.ListAgentConnectionsQuery{
			RemoteID:        r.URL.Query().Get("remote_id"),
			IncludeDisabled: boolQuery(r, "include_disabled"),
		},
	})
}

func (h *Handler) routeAgentConnectionGet(w http.ResponseWriter, r *http.Request) {
	h.handleQuery(w, r, control.Query{Type: control.QueryAgentConnectionGet, GetAgentConnection: &control.GetAgentConnectionQuery{ConnectionID: r.PathValue("id")}})
}

func (h *Handler) routeAgentConnectionCreate(w http.ResponseWriter, r *http.Request) {
	var payload control.CreateAgentConnectionCommand
	if !decodeBody(w, r, &payload) {
		return
	}
	h.handleCommand(w, r, control.Command{CommandID: commandID(r), Type: control.CommandAgentConnectionCreate, CreateAgentConnection: &payload})
}

func (h *Handler) routeAgentConnectionUpdate(w http.ResponseWriter, r *http.Request) {
	var payload control.UpdateAgentConnectionCommand
	if !decodeBody(w, r, &payload) {
		return
	}
	payload.ConnectionID = r.PathValue("id")
	h.handleCommand(w, r, control.Command{CommandID: commandID(r), Type: control.CommandAgentConnectionUpdate, UpdateAgentConnection: &payload})
}

func (h *Handler) routeAgentConnectionDelete(w http.ResponseWriter, r *http.Request) {
	h.handleCommand(w, r, control.Command{
		CommandID:             commandID(r),
		Type:                  control.CommandAgentConnectionDelete,
		DeleteAgentConnection: &control.DeleteAgentConnectionCommand{ConnectionID: r.PathValue("id"), Deregister: boolQuery(r, "deregister")},
	})
}

func (h *Handler) routeAgentConnectionRestart(w http.ResponseWriter, r *http.Request) {
	h.handleCommand(w, r, control.Command{CommandID: commandID(r), Type: control.CommandAgentConnectionRestart, RestartAgentConnection: &control.RestartAgentConnectionCommand{ConnectionID: r.PathValue("id")}})
}

func (h *Handler) routeHarnessesList(w http.ResponseWriter, r *http.Request) {
	h.handleQuery(w, r, control.Query{Type: control.QueryHarnessesList, ListHarnesses: &control.ListHarnessesQuery{IncludeMissing: boolQuery(r, "include_missing")}})
}

func (h *Handler) routeHarnessesDiscover(w http.ResponseWriter, r *http.Request) {
	var payload control.DiscoverHarnessesQuery
	if !decodeBody(w, r, &payload) {
		return
	}
	h.handleQuery(w, r, control.Query{Type: control.QueryHarnessesDiscover, DiscoverHarnesses: &payload})
}

func (h *Handler) routeLocalOverviewGet(w http.ResponseWriter, r *http.Request) {
	h.handleQuery(w, r, control.Query{Type: control.QueryLocalOverviewGet, GetLocalOverview: &control.GetLocalOverviewQuery{}})
}

func (h *Handler) routeLocalSessionsList(w http.ResponseWriter, r *http.Request) {
	h.handleQuery(w, r, control.Query{Type: control.QueryLocalSessionsList, ListLocalSessions: &control.ListLocalSessionsQuery{Agent: r.URL.Query().Get("agent"), Limit: intQuery(r, "limit")}})
}

func (h *Handler) routeLocalSessionsSync(w http.ResponseWriter, r *http.Request) {
	var payload control.SyncLocalSessionsQuery
	if !decodeBody(w, r, &payload) {
		return
	}
	h.handleQuery(w, r, control.Query{Type: control.QueryLocalSessionsSync, SyncLocalSessions: &payload})
}

func (h *Handler) routeLocalSessionGet(w http.ResponseWriter, r *http.Request) {
	h.handleQuery(w, r, control.Query{Type: control.QueryLocalSessionGet, GetLocalSession: &control.GetLocalSessionQuery{SessionID: r.PathValue("id")}})
}

func (h *Handler) routeCommandGet(w http.ResponseWriter, r *http.Request) {
	h.handleQuery(w, r, control.Query{Type: control.QueryCommandGet, GetCommand: &control.GetCommandQuery{CommandID: r.PathValue("id")}})
}

func (h *Handler) routeNotFound(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		h.routeDocsGet(w, r)
		return
	}
	writeControlError(w, http.StatusNotFound, control.ControlError{Code: control.ErrCodeNotFound, Message: "route not found"})
}

func (h *Handler) handleCommand(w http.ResponseWriter, r *http.Request, cmd control.Command) {
	ack, err := h.service.HandleCommand(r.Context(), control.Source{Kind: control.SourceLocal}, cmd)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, control.ControlError{Code: control.ErrCodeInternal, Message: "control command failed"})
		return
	}
	status := http.StatusAccepted
	if !ack.OK {
		status = statusForControlError(ack.Error, ack.Status)
	}
	writeJSON(w, status, ack)
}

func (h *Handler) handleQuery(w http.ResponseWriter, r *http.Request, query control.Query) {
	result, err := h.service.HandleQuery(r.Context(), control.Source{Kind: control.SourceLocal}, query)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, control.ControlError{Code: control.ErrCodeInternal, Message: "control query failed"})
		return
	}
	status := http.StatusOK
	if result.Error != nil {
		status = statusForControlError(result.Error, control.CommandStatusUnknown)
	}
	writeJSON(w, status, result)
}

func decodeBody(w http.ResponseWriter, r *http.Request, dest any) bool {
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dest); err != nil {
		writeControlError(w, http.StatusBadRequest, control.ControlError{Code: control.ErrCodeInvalidArgument, Message: "malformed JSON request body"})
		return false
	}
	var extra struct{}
	if err := decoder.Decode(&extra); err != io.EOF {
		writeControlError(w, http.StatusBadRequest, control.ControlError{Code: control.ErrCodeInvalidArgument, Message: "request body contains multiple JSON values"})
		return false
	}
	return true
}

func writeControlError(w http.ResponseWriter, status int, err control.ControlError) {
	writeJSON(w, status, err)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func statusForControlError(err *control.ControlError, commandStatus control.CommandStatus) int {
	if commandStatus == control.CommandStatusFailed {
		return http.StatusInternalServerError
	}
	if err == nil {
		return http.StatusInternalServerError
	}
	switch err.Code {
	case control.ErrCodeInvalidArgument:
		return http.StatusBadRequest
	case control.ErrCodeConflict:
		return http.StatusConflict
	case control.ErrCodeNotFound:
		return http.StatusNotFound
	case control.ErrCodeInternal:
		return http.StatusInternalServerError
	default:
		return http.StatusBadRequest
	}
}

func commandID(r *http.Request) string {
	if id := strings.TrimSpace(r.Header.Get(commandIDHeader)); id != "" {
		return id
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "cmd_local_fallback"
	}
	return "cmd_local_" + hex.EncodeToString(random[:])
}

func boolQuery(r *http.Request, key string) bool {
	value := strings.TrimSpace(r.URL.Query().Get(key))
	if value == "" {
		return false
	}
	parsed, err := strconv.ParseBool(value)
	return err == nil && parsed
}

func intQuery(r *http.Request, key string) int {
	value := strings.TrimSpace(r.URL.Query().Get(key))
	if value == "" {
		return 0
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return parsed
}

func IsLoopbackAddr(addr string) bool {
	host := addr
	if splitHost, _, err := net.SplitHostPort(addr); err == nil {
		host = splitHost
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
