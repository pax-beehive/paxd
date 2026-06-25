package daemon

import "sync"

type statusHub struct {
	mu          sync.Mutex
	subscribers map[string]map[chan struct{}]struct{}
}

func newStatusHub() *statusHub {
	return &statusHub{subscribers: make(map[string]map[chan struct{}]struct{})}
}

func (h *statusHub) Poke(remoteID string) {
	if h == nil || remoteID == "" {
		return
	}
	h.mu.Lock()
	subs := h.subscribers[remoteID]
	for ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	h.mu.Unlock()
}

func (h *statusHub) Subscribe(remoteID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	if h == nil || remoteID == "" {
		close(ch)
		return ch, func() {}
	}
	h.mu.Lock()
	if h.subscribers[remoteID] == nil {
		h.subscribers[remoteID] = make(map[chan struct{}]struct{})
	}
	h.subscribers[remoteID][ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			h.mu.Lock()
			if subs := h.subscribers[remoteID]; subs != nil {
				delete(subs, ch)
				if len(subs) == 0 {
					delete(h.subscribers, remoteID)
				}
			}
			close(ch)
			h.mu.Unlock()
		})
	}
	return ch, unsubscribe
}
