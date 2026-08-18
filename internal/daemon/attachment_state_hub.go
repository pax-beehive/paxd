package daemon

import (
	"sync"

	"github.com/pax-beehive/paxd/internal/control"
)

type attachmentStateHub struct {
	mu          sync.Mutex
	subscribers map[string]map[chan control.AttachmentLocalState]struct{}
}

func newAttachmentStateHub() *attachmentStateHub {
	return &attachmentStateHub{subscribers: make(map[string]map[chan control.AttachmentLocalState]struct{})}
}

func (h *attachmentStateHub) Publish(source control.Source, state control.AttachmentLocalState) {
	if h == nil || source.Kind != control.SourceRemote || source.RemoteID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subscribers[source.RemoteID] {
		select {
		case ch <- state:
		default:
		}
	}
}

func (h *attachmentStateHub) Subscribe(remoteID string) (<-chan control.AttachmentLocalState, func()) {
	ch := make(chan control.AttachmentLocalState, 32)
	if h == nil || remoteID == "" {
		close(ch)
		return ch, func() {}
	}
	h.mu.Lock()
	if h.subscribers[remoteID] == nil {
		h.subscribers[remoteID] = make(map[chan control.AttachmentLocalState]struct{})
	}
	h.subscribers[remoteID][ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subscribers[remoteID], ch)
			if len(h.subscribers[remoteID]) == 0 {
				delete(h.subscribers, remoteID)
			}
			close(ch)
			h.mu.Unlock()
		})
	}
}
