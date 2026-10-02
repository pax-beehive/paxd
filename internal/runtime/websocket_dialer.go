package runtime

import (
	"context"
	"net/http"

	"github.com/gorilla/websocket"

	"github.com/pax-beehive/paxd/internal/noderouting"
)

type GorillaWebSocketDialer struct {
	Dialer       *websocket.Dialer
	RoutingCache *noderouting.Cache
}

func (d GorillaWebSocketDialer) Dial(ctx context.Context, rawURL string, header http.Header) (WebSocketConn, *http.Response, error) {
	dialer := d.Dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	cache := d.RoutingCache
	if cache == nil {
		cache = noderouting.DefaultCache()
	}
	routed := cache.Headers(rawURL, header)
	conn, resp, err := dialer.DialContext(ctx, rawURL, routed)
	if err != nil {
		return nil, resp, err
	}
	cache.Observe(rawURL, routed, resp)
	return conn, resp, nil
}
