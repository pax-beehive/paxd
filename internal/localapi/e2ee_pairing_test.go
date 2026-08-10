package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pax-beehive/paxd/internal/e2eepairing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestE2EEPairingGivenLocalRequestWhenCompletedThenSecretOnlyReachesPairingService(t *testing.T) {
	t.Parallel()
	pairer := &fakeE2EEPairer{}
	handler := NewHandlerWithE2EE(docsRouteService{}, pairer)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/e2ee/pairings/pair_1/complete",
		bytes.NewBufferString(`{"agent_id":"agent_1","pairing_secret":"c2VjcmV0"}`),
	)

	handler.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Equal(t, "agent_1", pairer.agentID)
	assert.Equal(t, "pair_1", pairer.pairingID)
	assert.Equal(t, "c2VjcmV0", pairer.secret)
	var result e2eepairing.Result
	require.NoError(t, json.NewDecoder(recorder.Body).Decode(&result))
	assert.Equal(t, "device_1", result.DeviceID)
}

func TestE2EEPairingGivenInvalidOrUnconfiguredLocalRequestWhenHandledThenItFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		handler http.Handler
		body    string
		want    int
	}{
		{name: "unconfigured", handler: NewHandler(docsRouteService{}), body: `{"agent_id":"agent_1","pairing_secret":"secret"}`, want: http.StatusServiceUnavailable},
		{name: "malformed JSON", handler: NewHandlerWithE2EE(docsRouteService{}, &fakeE2EEPairer{}), body: `{"agent_id"`, want: http.StatusBadRequest},
		{name: "missing field", handler: NewHandlerWithE2EE(docsRouteService{}, &fakeE2EEPairer{}), body: `{"agent_id":"agent_1"}`, want: http.StatusBadRequest},
		{name: "pairer rejects", handler: NewHandlerWithE2EE(docsRouteService{}, &fakeE2EEPairer{err: errors.New("commitment mismatch")}), body: `{"agent_id":"agent_1","pairing_secret":"secret"}`, want: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/e2ee/pairings/pair_1/complete", bytes.NewBufferString(test.body))
			test.handler.ServeHTTP(recorder, request)
			assert.Equal(t, test.want, recorder.Code, recorder.Body.String())
		})
	}
}

type fakeE2EEPairer struct {
	agentID   string
	pairingID string
	secret    string
	err       error
}

func (p *fakeE2EEPairer) CompletePairing(
	_ context.Context,
	agentID string,
	pairingID string,
	secret string,
) (e2eepairing.Result, error) {
	p.agentID = agentID
	p.pairingID = pairingID
	p.secret = secret
	return e2eepairing.Result{
		PairingID: pairingID, AgentID: "agent_1", DeviceID: "device_1", KeyEpoch: 1,
	}, p.err
}
