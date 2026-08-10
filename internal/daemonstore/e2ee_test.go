package daemonstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestE2EECommandLifecycleDeduplicatesCompletedCommandsAndRetriesIncompleteCommands(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	store := openTestStore(t)
	store.now = func() time.Time { return now }
	ctx := context.Background()

	accepted, err := store.BeginE2EECommand(ctx, "agent_1", "cmd_1", 1)
	require.NoError(t, err)
	assert.True(t, accepted)
	accepted, err = store.BeginE2EECommand(ctx, "agent_1", "cmd_1", 1)
	require.NoError(t, err)
	assert.True(t, accepted, "an interrupted command must be retried")
	require.NoError(t, store.CompleteE2EECommand(ctx, "agent_1", "cmd_1"))
	accepted, err = store.BeginE2EECommand(ctx, "agent_1", "cmd_1", 1)
	require.NoError(t, err)
	assert.False(t, accepted, "a completed command must only be acknowledged")

	accepted, err = store.BeginE2EECommand(ctx, "agent_1", "cmd_2", 2)
	require.NoError(t, err)
	assert.True(t, accepted)
	_, err = store.BeginE2EECommand(ctx, "agent_1", "cmd_old", 1)
	require.ErrorIs(t, err, ErrStaleE2EEConnection)
}

func TestPruneE2EECommandReceiptsOnlyDeletesOldCompletedRows(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	store := openTestStore(t)
	store.now = func() time.Time { return now }
	ctx := context.Background()

	accepted, err := store.BeginE2EECommand(ctx, "agent_1", "completed_old", 1)
	require.NoError(t, err)
	assert.True(t, accepted)
	require.NoError(t, store.CompleteE2EECommand(ctx, "agent_1", "completed_old"))
	accepted, err = store.BeginE2EECommand(ctx, "agent_1", "incomplete_old", 1)
	require.NoError(t, err)
	assert.True(t, accepted)

	deleted, err := store.PruneE2EECommandReceipts(ctx, now.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), deleted)
	accepted, err = store.BeginE2EECommand(ctx, "agent_1", "incomplete_old", 1)
	require.NoError(t, err)
	assert.True(t, accepted)
	accepted, err = store.BeginE2EECommand(ctx, "agent_1", "completed_old", 1)
	require.NoError(t, err)
	assert.True(t, accepted, "a pruned completed receipt may be accepted as new work")
}
