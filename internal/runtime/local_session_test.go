package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type localTestSlot struct {
	pool         *ACPPool
	cwd          string
	exitMethod   string
	hangPrompt   bool
	resumeConfig bool
	resumes      int
	cancelled    bool
}

func (s *localTestSlot) SlotID() string       { return "slot" }
func (s *localTestSlot) ProcessEpoch() string { return "epoch" }
func (s *localTestSlot) Ordinal() int         { return 0 }
func (s *localTestSlot) Ready() bool          { return true }
func (s *localTestSlot) Send(ctx context.Context, payload []byte) error {
	var request acpRPCMessage
	if err := json.Unmarshal(payload, &request); err != nil {
		return err
	}
	if request.Method == s.exitMethod {
		s.pool.RemoveSlot(ctx, "slot", "epoch")
		return nil
	}
	if s.hangPrompt && request.Method == "session/prompt" {
		return nil
	}
	if request.Method == "session/new" {
		var params struct {
			CWD string `json:"cwd"`
		}
		_ = json.Unmarshal(request.Params, &params)
		s.cwd = params.CWD
	}
	if request.Method == "session/cancel" {
		return nil
	}
	result := json.RawMessage(`{"sessionId":"native","stopReason":"end_turn"}`)
	if s.cancelled && request.Method == "session/prompt" {
		result = json.RawMessage(`{"stopReason":"cancelled"}`)
	}
	if request.Method == "session/resume" {
		s.resumes++
		if s.resumeConfig {
			result = json.RawMessage(`{"configOptions":[]}`)
		}
	}
	response, _ := json.Marshal(acpRPCMessage{JSONRPC: "2.0", ID: request.ID, Result: result})
	return s.pool.HandleSlotFrame(ctx, "slot", "epoch", response)
}

func TestLocalSessionCanCreateAndPromptWithoutManagerOutputSink(t *testing.T) {
	pool := NewACPPool("conn", newFakeACPRouteStore("conn"))
	slot := &localTestSlot{pool: pool}
	pool.UpsertSlot(slot)
	client := NewLocalSessionClient(pool)
	created, err := client.CreateSession(t.Context(), t.TempDir())
	require.NoError(t, err)
	require.Equal(t, "native", created.NativeID)
	require.NotEmpty(t, slot.cwd)
	result, err := client.Prompt(t.Context(), created.NativeID, "Review this.", nil)
	require.NoError(t, err)
	require.Contains(t, string(result), "end_turn")
}

func TestLocalSessionStopsWaitingWhenAgentProcessExits(t *testing.T) {
	for _, method := range []string{"session/new", "session/prompt"} {
		t.Run(method, func(t *testing.T) {
			pool := NewACPPool("conn", newFakeACPRouteStore("conn"))
			slot := &localTestSlot{pool: pool}
			pool.UpsertSlot(slot)
			client := NewLocalSessionClient(pool)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if method == "session/new" {
				slot.exitMethod = method
				_, err := client.CreateSession(ctx, t.TempDir())
				require.ErrorContains(t, err, "Agent process exited")
				return
			}
			created, err := client.CreateSession(ctx, t.TempDir())
			require.NoError(t, err)
			slot.exitMethod = method
			_, err = client.Prompt(ctx, created.NativeID, "Review", nil)
			require.ErrorContains(t, err, "Agent process exited")
		})
	}
}
func TestLocalPromptDeadlineRecordsTerminalErrorAndPreservesOtherOutput(t *testing.T) {
	pool := NewACPPool("conn", newFakeACPRouteStore("conn"))
	slot := &localTestSlot{pool: pool, hangPrompt: true}
	pool.UpsertSlot(slot)
	client := NewLocalSessionClient(pool)
	created, err := client.CreateSession(t.Context(), t.TempDir())
	require.NoError(t, err)
	var frames [][]byte
	release, err := client.ObserveSession(created.NativeID, func(_ context.Context, data []byte) error {
		frames = append(frames, append([]byte(nil), data...))
		return nil
	})
	require.NoError(t, err)
	defer release()
	handled, err := pool.deliverLocalFrame(t.Context(), created.NativeID, []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"native"}}`))
	require.NoError(t, err)
	require.False(t, handled)
	require.Empty(t, frames)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err = client.Prompt(ctx, created.NativeID, "Review", nil)
	require.True(t, errors.Is(err, context.DeadlineExceeded))
	require.Len(t, frames, 1)
	require.Contains(t, string(frames[0]), "deadline exceeded")
}

func TestLocalSessionResumesColdRouteWithoutManagerConnection(t *testing.T) {
	pool := NewACPPool("conn", newFakeACPRouteStore("conn"))
	slot := &localTestSlot{pool: pool, resumeConfig: true}
	pool.UpsertSlot(slot)
	client := NewLocalSessionClient(pool)
	created, err := client.CreateSession(t.Context(), t.TempDir())
	require.NoError(t, err)
	pool.RemoveSlot(t.Context(), slot.SlotID(), slot.ProcessEpoch())
	pool.UpsertSlot(slot)
	var frames []string
	release, err := client.ObserveSession(created.NativeID, func(_ context.Context, data []byte) error { frames = append(frames, string(data)); return nil })
	require.NoError(t, err)
	defer release()
	require.NoError(t, client.ResumeSession(t.Context(), created.NativeID, slot.cwd))
	_, err = client.Prompt(t.Context(), created.NativeID, "Resume offline", nil)
	require.NoError(t, err)
	require.Equal(t, 1, slot.resumes)
	require.Len(t, frames, 2)
}

func TestLocalSessionReportsAgentCancellationAsFailure(t *testing.T) {
	pool := NewACPPool("conn", newFakeACPRouteStore("conn"))
	pool.UpsertSlot(&localTestSlot{pool: pool, cancelled: true})
	client := NewLocalSessionClient(pool)
	created, err := client.CreateSession(t.Context(), t.TempDir())
	require.NoError(t, err)
	_, err = client.Prompt(t.Context(), created.NativeID, "Review", nil)
	require.ErrorContains(t, err, "cancelled")
}
