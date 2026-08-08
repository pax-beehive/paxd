package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestACPSessionBoundaryBindsSessionNewAndTranslatesBothDirections(t *testing.T) {
	t.Parallel()
	store := newFakeACPSessionBindingStore()
	boundary := newACPSessionBoundary("conn_1", store)
	ctx := context.Background()

	nativeID, request, err := boundary.inbound(ctx, "manager_1", "", []byte(
		`{"jsonrpc":"2.0","id":"new_1","method":"session/new","params":{"cwd":"/work","mcpServers":[]}}`,
	))
	require.NoError(t, err)
	assert.Empty(t, nativeID)
	assert.Contains(t, string(request), `"session/new"`)

	managerID, response, err := boundary.outbound(ctx, "native_1", []byte(
		`{"jsonrpc":"2.0","id":"new_1","result":{"sessionId":"native_1"}}`,
	))
	require.NoError(t, err)
	assert.Equal(t, "manager_1", managerID)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":"new_1","result":{"sessionId":"manager_1"}}`, string(response))
	assert.Equal(t, "native_1", store.native["conn_1\x00manager_1"])

	nativeID, prompt, err := boundary.inbound(ctx, "manager_1", "", []byte(
		`{"jsonrpc":"2.0","id":"prompt_1","method":"session/prompt","params":{"sessionId":"manager_1","prompt":[]}}`,
	))
	require.NoError(t, err)
	assert.Equal(t, "native_1", nativeID)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":"prompt_1","method":"session/prompt","params":{"sessionId":"native_1","prompt":[]}}`, string(prompt))

	managerID, update, err := boundary.outbound(ctx, "native_1", []byte(
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"native_1","update":{"sessionId":"application-owned"}}}`,
	))
	require.NoError(t, err)
	assert.Equal(t, "manager_1", managerID)
	assert.JSONEq(t, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"manager_1","update":{"sessionId":"application-owned"}}}`, string(update))
}

func TestACPSessionBoundaryBootstrapsPlaintextLegacyBindingFromNativeHint(t *testing.T) {
	t.Parallel()
	store := newFakeACPSessionBindingStore()
	boundary := newACPSessionBoundary("conn_1", store)

	nativeID, prompt, err := boundary.inbound(context.Background(), "manager_old", "native_old", []byte(
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"manager_old"}}`,
	))
	require.NoError(t, err)
	assert.Equal(t, "native_old", nativeID)
	assert.Contains(t, string(prompt), `"sessionId":"native_old"`)
	assert.Equal(t, "native_old", store.native["conn_1\x00manager_old"])
}

func TestACPSessionBoundaryRejectsUnknownOuterSession(t *testing.T) {
	t.Parallel()
	boundary := newACPSessionBoundary("conn_1", newFakeACPSessionBindingStore())

	_, _, err := boundary.inbound(context.Background(), "manager_missing", "", []byte(
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"manager_missing"}}`,
	))
	require.ErrorIs(t, err, ErrACPSessionBindingMissing)
}

func TestACPSessionBoundaryPropagatesStoreErrors(t *testing.T) {
	t.Parallel()
	store := newFakeACPSessionBindingStore()
	store.err = errors.New("sqlite unavailable")
	boundary := newACPSessionBoundary("conn_1", store)

	_, _, err := boundary.inbound(context.Background(), "manager_1", "", []byte(
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"manager_1"}}`,
	))
	require.ErrorContains(t, err, "sqlite unavailable")
}

func TestACPSessionBoundaryGivenSessionNewErrorWhenReturnedThenKeepsOuterContextWithoutBinding(t *testing.T) {
	t.Parallel()
	store := newFakeACPSessionBindingStore()
	boundary := newACPSessionBoundary("conn_1", store)
	ctx := context.Background()

	_, _, err := boundary.inbound(ctx, "manager_1", "", []byte(
		`{"jsonrpc":"2.0","id":7,"method":"session/new","params":{}}`,
	))
	require.NoError(t, err)
	managerID, response, err := boundary.outbound(ctx, "", []byte(
		`{"jsonrpc":"2.0","id":7,"error":{"code":-32000,"message":"failed"}}`,
	))
	require.NoError(t, err)
	assert.Equal(t, "manager_1", managerID)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":7,"error":{"code":-32000,"message":"failed"}}`, string(response))
	assert.Empty(t, store.native)
}

func TestACPSessionBoundaryGivenUnknownNativeOutputWhenReturnedThenLeavesPayloadOpaque(t *testing.T) {
	t.Parallel()
	boundary := newACPSessionBoundary("conn_1", newFakeACPSessionBindingStore())
	payload := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"native_missing"}}`)

	managerID, got, err := boundary.outbound(context.Background(), "native_missing", payload)
	require.NoError(t, err)
	assert.Empty(t, managerID)
	assert.Equal(t, payload, got)
}

func TestACPSessionBoundaryGivenNilStoreWhenTranslatingThenFailsClosedForInbound(t *testing.T) {
	t.Parallel()
	boundary := newACPSessionBoundary("conn_1", nil)

	_, _, err := boundary.inbound(context.Background(), "manager_1", "", []byte(
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"manager_1"}}`,
	))
	require.ErrorIs(t, err, ErrACPSessionBindingMissing)
	managerID, payload, err := boundary.outbound(context.Background(), "native_1", []byte(
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"native_1"}}`,
	))
	require.NoError(t, err)
	assert.Empty(t, managerID)
	assert.Contains(t, string(payload), "native_1")
	require.NoError(t, boundary.bind(context.Background(), "manager_1", "native_1"))
}

func TestRewriteACPSessionIDGivenInvalidOrUnrelatedPayloadWhenCalledThenPreservesBytes(t *testing.T) {
	t.Parallel()
	for _, payload := range [][]byte{
		[]byte("not-json"),
		[]byte(`{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`),
		[]byte(`{"jsonrpc":"2.0","params":{"session_id":"manager_1"}}`),
	} {
		got, changed, err := rewriteACPSessionID(payload, "manager_1")
		require.NoError(t, err)
		assert.False(t, changed)
		assert.Equal(t, payload, got)
	}
}

func TestRewriteACPSessionIDGivenSnakeCaseWhenCalledThenRewritesOnlyRoutingField(t *testing.T) {
	t.Parallel()
	got, changed, err := rewriteACPSessionID([]byte(
		`{"jsonrpc":"2.0","params":{"session_id":"native_1","nested":{"session_id":"domain_value"}}}`,
	), "manager_1")
	require.NoError(t, err)
	assert.True(t, changed)
	assert.JSONEq(t,
		`{"jsonrpc":"2.0","params":{"session_id":"manager_1","nested":{"session_id":"domain_value"}}}`,
		string(got),
	)
}

type fakeACPSessionBindingStore struct {
	err     error
	native  map[string]string
	manager map[string]string
}

func newFakeACPSessionBindingStore() *fakeACPSessionBindingStore {
	return &fakeACPSessionBindingStore{native: make(map[string]string), manager: make(map[string]string)}
}

func (s *fakeACPSessionBindingStore) NativeSessionID(_ context.Context, connectionID string, managerSessionID string) (string, bool, error) {
	if s.err != nil {
		return "", false, s.err
	}
	value, ok := s.native[connectionID+"\x00"+managerSessionID]
	return value, ok, nil
}

func (s *fakeACPSessionBindingStore) ManagerSessionID(_ context.Context, connectionID string, nativeSessionID string) (string, bool, error) {
	if s.err != nil {
		return "", false, s.err
	}
	value, ok := s.manager[connectionID+"\x00"+nativeSessionID]
	return value, ok, nil
}

func (s *fakeACPSessionBindingStore) BindSessionIDs(_ context.Context, connectionID string, managerSessionID string, nativeSessionID string) error {
	if s.err != nil {
		return s.err
	}
	s.native[connectionID+"\x00"+managerSessionID] = nativeSessionID
	s.manager[connectionID+"\x00"+nativeSessionID] = managerSessionID
	return nil
}
