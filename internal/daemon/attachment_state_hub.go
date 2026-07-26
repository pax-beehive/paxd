package daemon

import (
	"sync"

	"github.com/pax-beehive/paxd/internal/control"
)

type attachmentStateHub struct {
	mu          sync.Mutex
	subscribers map[chan control.AttachmentLocalState]struct{}
}

func newAttachmentStateHub() *attachmentStateHub {
	return &attachmentStateHub{subscribers: make(map[chan control.AttachmentLocalState]struct{})}
}

func (h *attachmentStateHub) Publish(state control.AttachmentLocalState) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subscribers {
		select {
		case ch <- state:
		default:
		}
	}
}

func (h *attachmentStateHub) Subscribe(_ string) (<-chan control.AttachmentLocalState, func()) {
	ch := make(chan control.AttachmentLocalState, 32)
	if h == nil {
		close(ch)
		return ch, func() {}
	}
	h.mu.Lock()
	h.subscribers[ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subscribers, ch)
			close(ch)
			h.mu.Unlock()
		})
	}
}
