package controlws

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/stretchr/testify/require"
)

type mobileWireClient struct{ source string }

func (c *mobileWireClient) Request(_ context.Context, source, operation string, payload json.RawMessage) (json.RawMessage, error) {
	c.source = source
	return json.RawMessage(`{"available":true,"remoteWebRTC":false}`), nil
}

func TestMobileQueryUsesAuthenticatedNodeSourceAndResponseEnvelope(t *testing.T) {
	client := &mobileWireClient{}
	service := control.NewService(control.ServiceOptions{MobileControl: client})
	conn := newFakeWebSocketConn([]byte(`{"kind":"query","request_id":"mobile-state-1","query":{"type":"mobile.control","mobile_control":{"operation":"state","payload":{}}}}`))
	Run(context.Background(), conn, remoteSource(), service)
	require.Equal(t, "remote:"+remoteSource().RemoteID, client.source)
	require.Len(t, conn.writes(), 1)
	var response QueryResponseFrame
	require.NoError(t, json.Unmarshal(conn.writes()[0].payload, &response))
	require.Equal(t, KindResponse, response.Kind)
	require.Equal(t, "mobile-state-1", response.RequestID)
	require.Equal(t, control.QueryMobileControl, response.QueryResult.Type)
	require.Nil(t, response.QueryResult.Error)
	require.JSONEq(t, `{"available":true,"remoteWebRTC":false}`, string(response.QueryResult.MobileControl))
}
