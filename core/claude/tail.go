package claude

import "sync"

// tail is an io.Writer that retains only the last n bytes written to it,
// used to keep a bounded stderr tail for enriching error messages without
// buffering unbounded output. It is safe for concurrent writes.
type tail struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newTail(maxBytes int) *tail {
	return &tail{max: maxBytes}
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
