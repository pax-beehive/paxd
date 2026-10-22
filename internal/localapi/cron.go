package localapi

import (
	"errors"
	"net/http"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/cronjob"
)

func (h *Handler) cronRoutes() {
	h.mux.HandleFunc("GET /v1/cron-jobs", h.withCron(func(w http.ResponseWriter, r *http.Request) {
		jobs, err := h.cron.List(r.Context(), r.URL.Query().Get("agent"))
		h.cronResult(w, jobs, err)
	}))
	h.mux.HandleFunc("GET /v1/cron-status", h.withCron(func(w http.ResponseWriter, r *http.Request) {
		h.cronResult(w, map[string]bool{"running": h.cron.Running()}, nil)
	}))
	h.mux.HandleFunc("POST /v1/cron-jobs", h.withCron(h.cronCreate))
	h.mux.HandleFunc("GET /v1/cron-jobs/{id}", h.withCron(func(w http.ResponseWriter, r *http.Request) {
		job, err := h.cron.Get(r.Context(), r.PathValue("id"))
		h.cronResult(w, job, err)
	}))
	h.mux.HandleFunc("PATCH /v1/cron-jobs/{id}", h.withCron(h.cronUpdate))
	h.mux.HandleFunc("DELETE /v1/cron-jobs/{id}", h.withCron(func(w http.ResponseWriter, r *http.Request) {
		err := h.cron.Delete(r.Context(), r.PathValue("id"))
		h.cronResult(w, map[string]bool{"deleted": err == nil}, err)
	}))
	h.mux.HandleFunc("POST /v1/cron-jobs/{id}/run", h.withCron(func(w http.ResponseWriter, r *http.Request) {
		run, err := h.cron.RunNow(r.Context(), r.PathValue("id"))
		h.cronResult(w, run, err)
	}))
	h.mux.HandleFunc("GET /v1/cron-runs", h.withCron(func(w http.ResponseWriter, r *http.Request) {
		runs, err := h.cron.Runs(r.Context(), r.URL.Query().Get("job_id"), intQuery(r, "limit"))
		h.cronResult(w, runs, err)
	}))
}

func (h *Handler) withCron(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cron == nil {
			writeControlError(w, http.StatusServiceUnavailable, control.ControlError{Code: "cron_unavailable", Message: "Cron jobs are not configured."})
			return
		}
		next(w, r)
	}
}

func (h *Handler) cronCreate(w http.ResponseWriter, r *http.Request) {
	var req cronjob.CreateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	job, err := h.cron.Create(r.Context(), &req)
	h.cronResult(w, job, err)
}

func (h *Handler) cronUpdate(w http.ResponseWriter, r *http.Request) {
	var req cronjob.UpdateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	job, err := h.cron.Update(r.Context(), r.PathValue("id"), &req)
	h.cronResult(w, job, err)
}

func (h *Handler) cronResult(w http.ResponseWriter, result any, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, result)
		return
	}
	status, code := http.StatusInternalServerError, "cron_failed"
	switch {
	case errors.Is(err, cronjob.ErrNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, cronjob.ErrConflict):
		status, code = http.StatusConflict, "conflict"
	case errors.Is(err, cronjob.ErrInvalidArgument):
		status, code = http.StatusBadRequest, "invalid_argument"
	}
	writeControlError(w, status, control.ControlError{Code: code, Message: err.Error()})
}
