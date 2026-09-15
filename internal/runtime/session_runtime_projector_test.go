package runtime

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionRuntimeProjectorTracksTurnLifecycle(t *testing.T) {
	t.Run("Given a prompt lease when approval is rejected and the prompt later completes then status returns to running before becoming absent", func(t *testing.T) {
		projector := NewSessionRuntimeProjector()

		turn, err := projector.StartTurn("native_1", json.RawMessage(`1`), "slot_1", "epoch_1")
		require.NoError(t, err)
		assert.Equal(t, SessionRuntimeRunning, turn.RuntimeStatus)
		assert.NotEmpty(t, turn.TurnID)

		assert.True(t, projector.WaitForApproval("native_1", turn.TurnID, json.RawMessage(`"approval-1"`)))
		snapshot := projector.Snapshot()
		require.Len(t, snapshot.ActiveTurns, 1)
		assert.Equal(t, SessionRuntimeWaitingApproval, snapshot.ActiveTurns[0].RuntimeStatus)
		assert.JSONEq(t, `"approval-1"`, string(snapshot.ActiveTurns[0].PendingApprovalID))

		assert.True(t, projector.ResolveApproval("native_1", turn.TurnID))
		snapshot = projector.Snapshot()
		require.Len(t, snapshot.ActiveTurns, 1)
		assert.Equal(t, SessionRuntimeRunning, snapshot.ActiveTurns[0].RuntimeStatus)

		assert.True(t, projector.CompleteTurn("native_1", turn.TurnID))
		assert.Empty(t, projector.Snapshot().ActiveTurns)
	})

	t.Run("Given a delayed event from an older turn when a new turn is active then it cannot mutate the new turn", func(t *testing.T) {
		projector := NewSessionRuntimeProjector()
		oldTurn, err := projector.StartTurn("native_1", json.RawMessage(`1`), "slot_1", "epoch_1")
		require.NoError(t, err)
		require.True(t, projector.CompleteTurn("native_1", oldTurn.TurnID))
		newTurn, err := projector.StartTurn("native_1", json.RawMessage(`1`), "slot_1", "epoch_1")
		require.NoError(t, err)

		assert.False(t, projector.WaitForApproval("native_1", oldTurn.TurnID, json.RawMessage(`2`)))
		assert.False(t, projector.CompleteTurn("native_1", oldTurn.TurnID))
		snapshot := projector.Snapshot()
		require.Len(t, snapshot.ActiveTurns, 1)
		assert.Equal(t, newTurn.TurnID, snapshot.ActiveTurns[0].TurnID)
		assert.Equal(t, SessionRuntimeRunning, snapshot.ActiveTurns[0].RuntimeStatus)
	})

	t.Run("Given numeric and string request IDs when turns start then diagnostics preserve their JSON types", func(t *testing.T) {
		projector := NewSessionRuntimeProjector()
		_, err := projector.StartTurn("numeric", json.RawMessage(`1`), "slot_1", "epoch_1")
		require.NoError(t, err)
		_, err = projector.StartTurn("string", json.RawMessage(`"1"`), "slot_2", "epoch_2")
		require.NoError(t, err)

		snapshot := projector.Snapshot()
		require.Len(t, snapshot.ActiveTurns, 2)
		assert.JSONEq(t, `1`, string(snapshot.ActiveTurns[0].PromptRequestID))
		assert.JSONEq(t, `"1"`, string(snapshot.ActiveTurns[1].PromptRequestID))
	})
}

func TestSessionRuntimeProjectorSnapshotSubscriptionHasNoGap(t *testing.T) {
	projector := NewSessionRuntimeProjector()

	initial, changes, unsubscribe := projector.SnapshotAndSubscribe()
	defer unsubscribe()
	assert.Empty(t, initial.ActiveTurns)

	_, err := projector.StartTurn("native_1", json.RawMessage(`7`), "slot_1", "epoch_1")
	require.NoError(t, err)
	select {
	case <-changes:
	case <-t.Context().Done():
		require.Fail(t, "subscription did not observe the post-snapshot mutation")
	}
	assert.Greater(t, projector.Snapshot().Revision, initial.Revision)
}

func TestSessionRuntimeProjectorCompletesOnlyMatchingSlotEpoch(t *testing.T) {
	projector := NewSessionRuntimeProjector()
	matching, err := projector.StartTurn("native_matching", json.RawMessage(`1`), "slot_1", "epoch_1")
	require.NoError(t, err)
	otherEpoch, err := projector.StartTurn("native_other_epoch", json.RawMessage(`2`), "slot_1", "epoch_2")
	require.NoError(t, err)
	_, err = projector.StartTurn("native_other_slot", json.RawMessage(`3`), "slot_2", "epoch_1")
	require.NoError(t, err)

	assert.Equal(t, 1, projector.CompleteSlot("slot_1", "epoch_1"))
	_, matchingActive := projector.ActiveTurn("native_matching")
	assert.False(t, matchingActive)
	_, otherEpochActive := projector.ActiveTurn("native_other_epoch")
	assert.True(t, otherEpochActive)
	assert.Equal(t, SessionRuntimeResetAlreadyTerminal, projector.Reset("native_matching", matching.TurnID).Status)

	assert.Equal(t, 1, projector.CompleteSlot("slot_1", ""))
	assert.Equal(t, SessionRuntimeResetAlreadyTerminal, projector.Reset("native_other_epoch", otherEpoch.TurnID).Status)
	require.Len(t, projector.Snapshot().ActiveTurns, 1)
}

func TestSessionRuntimeProjectorResetSuppressesOnlyExpectedTurn(t *testing.T) {
	t.Run("Given an active turn when reset repeats then it is idempotent without deleting router lifecycle state", func(t *testing.T) {
		projector := NewSessionRuntimeProjector()
		turn, err := projector.StartTurn("native_1", json.RawMessage(`1`), "slot_1", "epoch_1")
		require.NoError(t, err)

		result := projector.Reset("native_1", turn.TurnID)
		assert.Equal(t, SessionRuntimeResetSuppressed, result.Status)
		assert.Empty(t, projector.Snapshot().ActiveTurns)

		repeated := projector.Reset("native_1", turn.TurnID)
		assert.Equal(t, SessionRuntimeResetAlreadySuppressed, repeated.Status)
		assert.True(t, projector.CompleteTurn("native_1", turn.TurnID))
		assert.Equal(t, SessionRuntimeResetAlreadyTerminal, projector.Reset("native_1", turn.TurnID).Status)
	})

	t.Run("Given a reset racing a replacement turn when the expected instance mismatches then the new turn remains visible", func(t *testing.T) {
		projector := NewSessionRuntimeProjector()
		oldTurn, err := projector.StartTurn("native_1", json.RawMessage(`1`), "slot_1", "epoch_1")
		require.NoError(t, err)
		require.True(t, projector.CompleteTurn("native_1", oldTurn.TurnID))
		newTurn, err := projector.StartTurn("native_1", json.RawMessage(`1`), "slot_1", "epoch_1")
		require.NoError(t, err)

		result := projector.Reset("native_1", oldTurn.TurnID)
		assert.Equal(t, SessionRuntimeResetAlreadyTerminal, result.Status)
		snapshot := projector.Snapshot()
		require.Len(t, snapshot.ActiveTurns, 1)
		assert.Equal(t, newTurn.TurnID, snapshot.ActiveTurns[0].TurnID)
	})
}

func TestSessionRuntimeProjectorResetAlwaysRequestsACompleteSnapshot(t *testing.T) {
	projector := NewSessionRuntimeProjector()
	_, changes, unsubscribe := projector.SnapshotAndSubscribe()
	defer unsubscribe()

	result := projector.Reset("missing", "turn_missing")

	assert.Equal(t, SessionRuntimeResetNotFound, result.Status)
	select {
	case <-changes:
	case <-time.After(time.Second):
		require.Fail(t, "a not-found reset must still trigger a self-healing complete snapshot")
	}
}
