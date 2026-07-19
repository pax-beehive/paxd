package runtime

import (
	"bufio"
	"io"
	"log"
	"sync"
)

const (
	stderrTailLimit       = 8192
	stderrStatusTailLimit = 2048
)

// stderrTail drains a child process stderr stream: every line is forwarded to
// the process log with a routing prefix, and a bounded ring of the most recent
// bytes is kept for failure reporting.
type stderrTail struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func newStderrTail(limit int) *stderrTail {
	if limit <= 0 {
		limit = stderrTailLimit
	}
	return &stderrTail{limit: limit}
}

func (t *stderrTail) Consume(r io.Reader, logPrefix string) {
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadBytes('\n')
		if trimmed := trimLineDelimiter(line); len(trimmed) > 0 {
			log.Printf("%s %s", logPrefix, trimmed)
			t.append(trimmed)
		}
		if err != nil {
			return
		}
	}
}

func (t *stderrTail) append(line []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(line) >= t.limit {
		t.buf = append(t.buf[:0], line[len(line)-t.limit:]...)
		return
	}
	t.buf = append(t.buf, line...)
	t.buf = append(t.buf, '\n')
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
}

func (t *stderrTail) Tail(max int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.buf
	if max > 0 && len(out) > max {
		out = out[len(out)-max:]
	}
	return string(out)
}
