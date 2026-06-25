package runtime

import (
	"sync"
	"time"
)

const (
	websocketTextMessage   = 1
	websocketBinaryMessage = 2
	websocketPingMessage   = 9
)

type lockedWebSocketConn struct {
	conn WebSocketConn
	mu   sync.Mutex
}

func (c *lockedWebSocketConn) ReadMessage() (int, []byte, error) {
	return c.conn.ReadMessage()
}

func (c *lockedWebSocketConn) WriteMessage(messageType int, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteMessage(messageType, payload)
}

func (c *lockedWebSocketConn) Close() error {
	return c.conn.Close()
}

type pongHandlerConn interface {
	SetPongHandler(func(string) error)
}

type heartbeatConn struct {
	*lockedWebSocketConn

	cfg          HeartbeatConfig
	done         chan struct{}
	closeOnce    sync.Once
	activityMu   sync.Mutex
	lastActivity time.Time
	timedOut     bool
}

func newHeartbeatConn(conn WebSocketConn, cfg HeartbeatConfig) *heartbeatConn {
	cfg = cfg.WithDefaults()
	heartbeat := &heartbeatConn{
		lockedWebSocketConn: &lockedWebSocketConn{conn: conn},
		cfg:                 cfg,
		done:                make(chan struct{}),
		lastActivity:        time.Now(),
	}
	if pongConn, ok := conn.(pongHandlerConn); ok {
		pongConn.SetPongHandler(func(string) error {
			heartbeat.markActivity()
			return nil
		})
	}
	return heartbeat
}

func (c *heartbeatConn) ReadMessage() (int, []byte, error) {
	messageType, payload, err := c.lockedWebSocketConn.ReadMessage()
	if err == nil {
		c.markActivity()
	}
	return messageType, payload, err
}

func (c *heartbeatConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
	})
	return c.lockedWebSocketConn.Close()
}

func (c *heartbeatConn) Start() {
	go c.pingLoop()
	go c.watchdogLoop()
}

func (c *heartbeatConn) TimedOut() bool {
	c.activityMu.Lock()
	defer c.activityMu.Unlock()
	return c.timedOut
}

func (c *heartbeatConn) markActivity() {
	c.activityMu.Lock()
	c.lastActivity = time.Now()
	c.activityMu.Unlock()
}

func (c *heartbeatConn) pingLoop() {
	ticker := time.NewTicker(c.cfg.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			if err := c.WriteMessage(websocketPingMessage, nil); err != nil {
				_ = c.Close()
				return
			}
		}
	}
}

func (c *heartbeatConn) watchdogLoop() {
	ticker := time.NewTicker(c.cfg.ReadTimeout / 2)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.activityMu.Lock()
			expired := time.Since(c.lastActivity) > c.cfg.ReadTimeout
			if expired {
				c.timedOut = true
			}
			c.activityMu.Unlock()
			if expired {
				_ = c.Close()
				return
			}
		}
	}
}
