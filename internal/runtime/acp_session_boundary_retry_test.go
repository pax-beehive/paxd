package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestACPSessionBoundaryGivenFailedBindingThenCreationResponseCanRetry(t *testing.T) {
	store := &retryBindingStore{fakeACPSessionBindingStore: newFakeACPSessionBindingStore()}
	boundary := newACPSessionBoundary("conn", store)
	_, _, err := boundary.inbound(t.Context(), "manager", "", []byte(`{"id":7,"method":"session/new","params":{}}`))
	require.NoError(t, err)
	failure := errors.New("sqlite write failed")
	store.bindErr = failure
	response := []byte(`{"id":7,"result":{"sessionId":"native"}}`)
	_, _, err = boundary.outbound(t.Context(), "native", response)
	require.ErrorIs(t, err, failure)
	assert.Empty(t, store.native)
	store.bindErr = nil
	managerID, rewritten, err := boundary.outbound(t.Context(), "native", response)
	require.NoError(t, err)
	assert.Equal(t, "manager", managerID)
	assert.JSONEq(t, `{"id":7,"result":{"sessionId":"manager"}}`, string(rewritten))
	assert.Equal(t, "native", store.native["conn\x00manager"])
}

func TestACPSessionBoundaryGivenNativeHintBindingFailureThenDoNotDispatchUnpersistedMapping(t *testing.T) {
	failure := errors.New("sqlite unavailable")
	store := &retryBindingStore{fakeACPSessionBindingStore: newFakeACPSessionBindingStore(), bindErr: failure}
	boundary := newACPSessionBoundary("conn", store)
	request := []byte(`{"id":8,"method":"session/prompt","params":{"sessionId":"manager"}}`)
	_, payload, err := boundary.inbound(t.Context(), "manager", "native", request)
	require.ErrorIs(t, err, failure)
	assert.Nil(t, payload)
	store.bindErr = nil
	nativeID, payload, err := boundary.inbound(t.Context(), "manager", "native", request)
	require.NoError(t, err)
	assert.Equal(t, "native", nativeID)
	assert.Contains(t, string(payload), `"sessionId":"native"`)
}

func TestACPSessionBoundaryGivenWorkerRequestIDCollisionThenKeepCreationCorrelation(t *testing.T) {
	store := newFakeACPSessionBindingStore()
	require.NoError(t, store.BindSessionIDs(t.Context(), "conn", "existing", "existing-native"))
	boundary := newACPSessionBoundary("conn", store)
	_, _, err := boundary.inbound(t.Context(), "new-manager", "", []byte(`{"id":7,"method":"session/new","params":{}}`))
	require.NoError(t, err)
	managerID, permission, err := boundary.outbound(t.Context(), "existing-native", []byte(`{"id":7,"method":"session/request_permission","params":{"sessionId":"existing-native"}}`))
	require.NoError(t, err)
	assert.Equal(t, "existing", managerID)
	assert.Contains(t, string(permission), `"sessionId":"existing"`)
	assert.NotContains(t, store.native, "conn\x00new-manager")
	managerID, _, err = boundary.outbound(t.Context(), "new-native", []byte(`{"id":7,"result":{"sessionId":"new-native"}}`))
	require.NoError(t, err)
	assert.Equal(t, "new-manager", managerID)
}

func TestACPSessionBoundaryGivenPoolReconnectThenPendingCreationKeepsItsManagerID(t *testing.T) {
	store := newFakeACPSessionBindingStore()
	pool := &ACPPool{}
	first := &AgentTunnelSession{
		spec: AgentConnectionSpec{ConnectionID: "conn"},
		deps: AgentTunnelSessionDeps{ACPSessionBindings: store},
	}
	beforeReconnect := first.poolSessionBoundary(pool)
	_, _, err := beforeReconnect.inbound(t.Context(), "manager", "", []byte(`{"id":7,"method":"session/new","params":{}}`))
	require.NoError(t, err)
	second := &AgentTunnelSession{spec: first.spec, deps: first.deps}
	afterReconnect := second.poolSessionBoundary(pool)
	managerID, rewritten, err := afterReconnect.outbound(t.Context(), "native", []byte(`{"id":7,"result":{"sessionId":"native"}}`))
	require.NoError(t, err)
	assert.Same(t, beforeReconnect, afterReconnect)
	assert.Equal(t, "manager", managerID)
	assert.Contains(t, string(rewritten), `"sessionId":"manager"`)

	other := &AgentTunnelSession{spec: AgentConnectionSpec{ConnectionID: "other"}, deps: first.deps}
	assert.NotSame(t, afterReconnect, other.poolSessionBoundary(pool))
}

type retryBindingStore struct {
	*fakeACPSessionBindingStore
	bindErr error
}

func (s *retryBindingStore) BindSessionIDs(ctx context.Context, connectionID, managerID, nativeID string) error {
	if s.bindErr != nil {
		return s.bindErr
	}
	return s.fakeACPSessionBindingStore.BindSessionIDs(ctx, connectionID, managerID, nativeID)
}
