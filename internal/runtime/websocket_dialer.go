package runtime

import (
	"context"
	"net/http"

	"github.com/gorilla/websocket"
)

type GorillaWebSocketDialer struct {
	Dialer *websocket.Dialer
}

func (d GorillaWebSocketDialer) Dial(ctx context.Context, rawURL string, header http.Header) (WebSocketConn, *http.Response, error) {
	dialer := d.Dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	conn, resp, err := dialer.DialContext(ctx, rawURL, header)
	if err != nil {
		return nil, resp, err
	}
	return conn, resp, nil
}
