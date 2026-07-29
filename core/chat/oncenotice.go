package chat

import "sync"

// OnceNotifier tells each destination something at most once, until the
// condition that prompted it clears.
//
// It exists for notices that would otherwise repeat on every message while a
// deployment-wide condition holds — today, "the provider is not authorized". The
// first message from a chat earns an explanation; the next fifty do not, because
// the user has already been told and the answer has not changed. Clear resets
// every destination at once, so a condition that returns is announced afresh.
//
// The zero value is not usable; call NewOnceNotifier. A nil *OnceNotifier always
// reports true, so a caller that never wired one keeps its old behaviour.
type OnceNotifier struct {
	mu   sync.Mutex
	told map[string]bool
}

// NewOnceNotifier returns an empty notifier.
func NewOnceNotifier() *OnceNotifier {
	return &OnceNotifier{told: map[string]bool{}}
}

// Should reports whether id still needs to be told, marking it as told.
func (n *OnceNotifier) Should(id string) bool {
	if n == nil {
		return true
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.told[id] {
		return false
	}
	n.told[id] = true
	return true
}

// Clear forgets every destination, so the next occurrence is announced again.
func (n *OnceNotifier) Clear() {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	clear(n.told)
}
