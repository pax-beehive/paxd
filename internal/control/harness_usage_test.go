package control

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeHarnessUsage struct {
	calls int
	err   error
}

func (f *fakeHarnessUsage) Usage(_ context.Context, q HarnessUsageStatusQuery) (HarnessUsageView, error) {
	f.calls++
	if f.err != nil {
		return HarnessUsageView{}, f.err
	}
	return HarnessUsageView{Harness: q.Harness, State: HarnessUsageAvailable, ObservedAt: "2026-11-12T17:31:51Z",
		Windows: []HarnessUsageWindow{{ID: "seven_day", WindowMinutes: 10080, UsedPercent: 0}}}, nil
}

func TestHarnessUsageQueryWireFormat(t *testing.T) {
	// The manager sends this exact shape over the node control tunnel.
	var query Query
	require.NoError(t, json.Unmarshal([]byte(`{"type":"harness_usage.status","harness_usage_status":{"harness":"codex"}}`), &query))
	require.NoError(t, query.Validate())
	assert.Equal(t, "codex", query.HarnessUsageStatus.Harness)

	f := &fakeHarnessUsage{}
	result, err := NewService(ServiceOptions{HarnessUsage: f}).HandleQuery(context.Background(), Source{Kind: SourceRemote, RemoteID: "remote-a"}, query)
	require.NoError(t, err)
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"harness_usage.status","harness_usage":{"harness":"codex","state":"available",`+
		`"observed_at":"2026-11-12T17:31:51Z","windows":[{"id":"seven_day","window_minutes":10080,"used_percent":0}]}}`, string(raw))
}

func TestHarnessUsageQueryValidation(t *testing.T) {
	f := &fakeHarnessUsage{}
	s := NewService(ServiceOptions{HarnessUsage: f})
	local := Source{Kind: SourceLocal}

	result, err := s.HandleQuery(context.Background(), local, Query{Type: QueryHarnessUsageStatus, HarnessUsageStatus: &HarnessUsageStatusQuery{Harness: "gemini"}})
	require.NoError(t, err)
	require.NotNil(t, result.Error)
	assert.Equal(t, ErrCodeInvalidArgument, result.Error.Code)

	query := Query{Type: QueryHarnessUsageStatus, HarnessUsageStatus: &HarnessUsageStatusQuery{Harness: "claude"}}
	result, err = s.HandleQuery(context.Background(), Source{Kind: SourceRemote}, query)
	require.NoError(t, err)
	require.NotNil(t, result.Error)
	assert.Equal(t, 0, f.calls)

	result, err = NewService(ServiceOptions{}).HandleQuery(context.Background(), local, query)
	require.NoError(t, err)
	require.NotNil(t, result.Error)
	assert.Equal(t, ErrCodeInternal, result.Error.Code)

	f.err = errors.New("stopped")
	result, err = s.HandleQuery(context.Background(), local, query)
	require.NoError(t, err)
	require.NotNil(t, result.Error)
	assert.Nil(t, result.HarnessUsage)
}
