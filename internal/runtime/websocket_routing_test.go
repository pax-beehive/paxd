package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pax-beehive/paxd/internal/noderouting"
)

func TestWebSocketGivenWrongRoutingCacheThenHandshakeRepairsNextConnection(t *testing.T) {
	cache := &noderouting.Cache{Dir: t.TempDir()}
	hints := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hints <- r.Header.Get(noderouting.HeaderUserID)
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, http.Header{noderouting.HeaderUserID: []string{"usr_real"}})
		if err != nil {
			return
		}
		defer conn.Close()
		kind, message, err := conn.ReadMessage()
		if err == nil {
			_ = conn.WriteMessage(kind, message)
		}
	}))
	defer server.Close()
	cache.Save(server.URL, "node-key", "usr_wrong")
	dialer := GorillaWebSocketDialer{RoutingCache: cache}
	for range 2 {
		conn, response, err := dialer.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/node/control", http.Header{"X-Pax-Key": []string{"node-key"}})
		require.NoError(t, err)
		assert.Equal(t, http.StatusSwitchingProtocols, response.StatusCode)
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("hello")))
		_, message, err := conn.ReadMessage()
		require.NoError(t, err)
		assert.Equal(t, "hello", string(message))
		require.NoError(t, conn.Close())
	}
	assert.Equal(t, "usr_wrong", <-hints)
	assert.Equal(t, "usr_real", <-hints)
	assert.Equal(t, "usr_real", cache.Load(server.URL, "node-key"))
}
