package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type session struct {
	SessionID   string `json:"sessionId"`
	AgentType   string `json:"agentType"`
	NativeID    string `json:"nativeId"`
	Name        string `json:"name,omitempty"`
	LastActive  string `json:"lastActive"`
	Preview     string `json:"preview,omitempty"`
	Status      string `json:"status,omitempty"`
	CurrentTask string `json:"currentTask,omitempty"`
	TokenUsage  int64  `json:"tokenUsage,omitempty"`
	UpdatedAt   string `json:"updatedAt,omitempty"`
}

type server struct {
	mu       sync.RWMutex
	sessions map[string]session
}

func main() {
	addr := ":" + firstNonEmpty(os.Getenv("MOCK_HERMES_PORT"), "8642")
	srv := &server{sessions: make(map[string]session)}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/sessions", srv.handleSessions)
	mux.HandleFunc("/api/sessions/", srv.handleSession)
	mux.HandleFunc("/v1/chat/completions", srv.handleChat)
	mux.HandleFunc("/v1/runs/", srv.handleRun)

	log.Printf("mockhermes listening on http://localhost%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func (s *server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	sessions := make([]session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (s *server) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sessionID := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	s.mu.RLock()
	sess, ok := s.sessions[sessionID]
	s.mu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sessionID := r.Header.Get("X-Hermes-Session-Id")
	if sessionID == "" {
		sessionID = "sess-mock-" + time.Now().UTC().Format("20060102150405")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	s.mu.Lock()
	s.sessions[sessionID] = session{
		SessionID:  sessionID,
		AgentType:  "hermes",
		NativeID:   "resp-" + sessionID,
		Name:       "Mock Hermes",
		LastActive: now,
		Preview:    "mock response",
		Status:     "idle",
		TokenUsage: 17,
		UpdatedAt:  now,
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Hermes-Session-Id", sessionID)
	w.WriteHeader(http.StatusOK)

	turnID := "turn-" + time.Now().UTC().Format("150405.000")
	writeSSE(w, map[string]any{
		"id": turnID,
		"choices": []map[string]any{{
			"delta": map[string]string{"role": "assistant", "content": "mock "},
		}},
	})
	writeSSE(w, map[string]any{
		"id": turnID,
		"choices": []map[string]any{{
			"delta": map[string]string{"content": "completed"},
		}},
	})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

func (s *server) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/stop") {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func writeSSE(w http.ResponseWriter, payload any) {
	data, _ := json.Marshal(payload)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
