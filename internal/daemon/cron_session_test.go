package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pax-beehive/paxd/internal/cloud"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/cronjob"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	runtimes "github.com/pax-beehive/paxd/internal/runtime"
	"github.com/stretchr/testify/require"
)

type cronSlot struct {
	pool         *runtimes.ACPPool
	created      int
	failCreation bool
	prompts      []string
}

func (*cronSlot) SlotID() string       { return "slot" }
func (*cronSlot) ProcessEpoch() string { return "epoch" }
func (*cronSlot) Ordinal() int         { return 0 }
func (*cronSlot) Ready() bool          { return true }
func (s *cronSlot) Send(ctx context.Context, data []byte) error {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			SessionID string `json:"sessionId"`
			Prompt    []struct {
				Text string `json:"text"`
			} `json:"prompt"`
		} `json:"params"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return err
	}
	result := map[string]string{"stopReason": "end_turn"}
	if msg.Method == "session/new" {
		s.created++
		if s.failCreation {
			return errors.New("connection lost during session/new")
		}
		result["sessionId"] = fmt.Sprintf("native_%d", s.created)
	}
	if msg.Method == "session/prompt" {
		s.prompts = append(s.prompts, msg.Params.Prompt[0].Text)
		update, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": msg.Params.SessionID, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "Reviewed."}}}})
		if err := s.pool.HandleSlotFrame(ctx, "slot", "epoch", update); err != nil {
			return err
		}
	}
	response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	return s.pool.HandleSlotFrame(ctx, "slot", "epoch", response)
}

func TestCronExecutesACPWithNewAndReusedSessionsAndOfflineRegistration(t *testing.T) {
	for _, mode := range []string{"new", "reuse"} {
		t.Run(mode, func(t *testing.T) {
			store := openTestStore(t)
			var offline atomic.Bool
			offline.Store(true)
			origins := map[string]cloud.SessionOrigin{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if offline.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if r.URL.Path != "/api/v1/node/agents/agent/sessions/register" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				var req cloud.RegisterSessionRequest
				if json.NewDecoder(r.Body).Decode(&req) != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				origins[req.NativeID] = req.Origin
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"session_id": "manager_" + req.NativeID}})
			}))
			defer server.Close()
			enabled := true
			_, err := store.CreateRemote(t.Context(), control.CreateRemoteCommand{Remote: control.Remote{ID: "remote", Name: "Remote", CloudAPIURL: server.URL, Enabled: &enabled}})
			require.NoError(t, err)
			_, err = store.CreateAgentConnection(t.Context(), control.CreateAgentConnectionCommand{ID: "conn", RemoteID: "remote", Name: "Codex", InstanceID: "codex", AgentType: "codex", Harness: "codex", Command: []string{"codex-acp"}, DesiredState: control.DesiredStateRunning})
			require.NoError(t, err)
			require.NoError(t, store.DB().Model(&daemonstore.AgentConnection{}).Where("id = ?", "conn").Update("cloud_agent_id", "agent").Error)
			pools := runtimes.NewACPPoolRegistry(runtimes.ACPRouteStoreFactoryFunc(func(string) runtimes.ACPRouteStore { return acpRouteStoreAdapter{store: store} }))
			pool, err := pools.Get("conn")
			require.NoError(t, err)
			slot := &cronSlot{pool: pool}
			pool.UpsertSlot(slot)
			executor := &cronjob.ACPExecutor{Store: store, Pools: pools, Headers: fakeHeaderProvider{}}
			svc, err := cronjob.New(store.DB(), cronjob.Options{Executor: executor, ValidateTarget: executor.ValidateTarget})
			require.NoError(t, err)
			job, err := svc.Create(t.Context(), &cronjob.CreateRequest{ConnectionID: "conn", SessionMode: mode, Name: "Review", Prompt: "Original prompt", WorkingDir: t.TempDir(), Schedule: "0 9 * * *"})
			require.NoError(t, err)
			first, err := svc.RunNow(t.Context(), job.ID)
			require.NoError(t, err)
			job, err = svc.Get(t.Context(), job.ID)
			require.NoError(t, err)
			changed := "Next prompt"
			_, err = svc.Update(t.Context(), job.ID, &cronjob.UpdateRequest{Prompt: &changed, ExpectedRevision: job.Revision})
			require.NoError(t, err)
			_, err = svc.ExecuteNext(t.Context())
			require.NoError(t, err)
			_, err = svc.RunNow(t.Context(), job.ID)
			require.NoError(t, err)
			_, err = svc.ExecuteNext(t.Context())
			require.NoError(t, err)
			runs, err := svc.Runs(t.Context(), job.ID, 10)
			require.NoError(t, err)
			require.Len(t, runs, 2)
			for _, run := range runs {
				require.Equal(t, "succeeded", run.Status, run.Error)
				require.NotEmpty(t, run.NativeSessionID)
				require.Empty(t, run.ManagerSessionID)
				require.Equal(t, job.WorkingDir, run.WorkingDir)
			}
			require.Equal(t, []string{"Original prompt", "Next prompt"}, slot.prompts)
			if mode == "reuse" {
				require.Equal(t, 1, slot.created)
				require.Equal(t, runs[0].SessionID, runs[1].SessionID)
			} else {
				require.Equal(t, 2, slot.created)
				require.NotEqual(t, runs[0].SessionID, runs[1].SessionID)
			}
			var events []cronjob.SessionEvent
			require.NoError(t, store.DB().Find(&events).Error)
			require.Len(t, events, 6)
			offline.Store(false)
			// The tunnel is deliberately absent: registration still succeeds and the
			// journal remains available for later encrypted publication.
			require.ErrorContains(t, executor.Sync(t.Context()), "transport is not connected")
			runs, err = svc.Runs(t.Context(), job.ID, 10)
			require.NoError(t, err)
			for _, run := range runs {
				require.Equal(t, "manager_"+run.NativeSessionID, run.ManagerSessionID)
			}
			if mode == "reuse" {
				require.Equal(t, first.ID, origins[runs[0].NativeSessionID].RunID)
				current, err := svc.Get(t.Context(), job.ID)
				require.NoError(t, err)
				_, err = svc.Update(t.Context(), job.ID, &cronjob.UpdateRequest{ResetSession: true, ExpectedRevision: current.Revision})
				require.NoError(t, err)
				slot.failCreation = true
				_, err = svc.RunNow(t.Context(), job.ID)
				require.NoError(t, err)
				_, err = svc.ExecuteNext(t.Context())
				require.NoError(t, err)
				attempts := slot.created
				slot.failCreation = false
				_, err = svc.RunNow(t.Context(), job.ID)
				require.NoError(t, err)
				_, err = svc.ExecuteNext(t.Context())
				require.NoError(t, err)
				require.Equal(t, attempts, slot.created, "ambiguous creation must not create another session")
				latest, err := svc.Runs(t.Context(), job.ID, 1)
				require.NoError(t, err)
				require.Equal(t, "failed", latest[0].Status)
				require.Contains(t, latest[0].Error, "unknown outcome")

			}
		})
	}
}
