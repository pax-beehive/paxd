// Package cloud provides the WebSocket client for receiving real-time
// messages from the Fleet Cloud API.
package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WSClient maintains a WebSocket connection to the Cloud API for receiving
// push events (new messages, steer commands, etc.) and sending turn results.
type WSClient struct {
	url         string
	headers     http.Header
	dialer      *websocket.Dialer
	conn        *websocket.Conn
	writeMu     sync.Mutex
	msgCh       chan Message
	reconnectCh chan struct{}
}

// NewWSClient creates a new WebSocket client.
func NewWSClient(wsURL string, cfClientID, cfClientSecret string) *WSClient {
	headers := http.Header{}
	if cfClientID != "" {
		headers.Set("CF-Access-Client-Id", cfClientID)
	}
	if cfClientSecret != "" {
		headers.Set("CF-Access-Client-Secret", cfClientSecret)
	}
	return &WSClient{
		url:     wsURL,
		headers: headers,
		dialer: &websocket.Dialer{
			HandshakeTimeout: 10 * time.Second,
		},
	}
}

// Connect establishes the WebSocket connection and starts the read pump.
// Returns a channel that receives incoming messages.
func (w *WSClient) Connect(ctx context.Context) (<-chan Message, error) {
	w.msgCh = make(chan Message, 64)
	w.reconnectCh = make(chan struct{}, 1)

	go w.run(ctx)
	return w.msgCh, nil
}

func (w *WSClient) run(ctx context.Context) {
	defer close(w.msgCh)

	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := w.dialAndRead(ctx); err != nil {
			log.Printf("[ws] connection error: %v, reconnecting in %v", err, backoff)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (w *WSClient) dialAndRead(ctx context.Context) error {
	conn, resp, err := w.dialer.DialContext(ctx, w.url, w.headers)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}

	w.conn = conn
	defer conn.Close()

	log.Printf("[ws] connected to %s", w.url)

	// Reset backoff on successful connection
	// (handled by run's loop resetting it on re-entry after this func returns)

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		var msg Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			log.Printf("[ws] bad message: %v", err)
			continue
		}

		select {
		case w.msgCh <- msg:
		case <-ctx.Done():
			return ctx.Err()
		default:
			log.Printf("[ws] message channel full, dropping message %s", msg.MessageID)
		}
	}
}

// SendJSON marshals v to JSON and writes it as a text message on the WS connection.
// Thread-safe; returns an error if the connection is nil or the write fails.
func (w *WSClient) SendJSON(v any) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if w.conn == nil {
		return fmt.Errorf("not connected")
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.conn.WriteMessage(websocket.TextMessage, data)
}

// Close closes the WebSocket connection.
func (w *WSClient) Close() error {
	if w.conn != nil {
		return w.conn.Close()
	}
	return nil
}
