package chat

// NewSession resets the calling chat's stored Claude session so the next message
// starts a brand-new conversation with no --resume. It delegates to the session
// store's Delete, which treats an absent chat as a harmless no-op. A nil store
// (continuity disabled) is also a no-op: there is nothing to reset, and the next
// message already starts fresh. It returns any store error so the caller can log
// it; delivery of the confirmation is the caller's concern.
func (s *Service) NewSession(chatID ChatID) error {
	if s.sessions == nil {
		return nil
	}
	return s.sessions.Delete(chatID)
}

// StopChat cancels chatID's in-flight run, if any, via the same dispatch.Cancel
// primitive the inline Stop button uses (there is no second cancel path). It
// reports whether a run was active so the caller can tailor the acknowledgement
// ("stopping" vs "nothing to stop"). Cancel on an idle chat is a no-op.
func (s *Service) StopChat(chatID ChatID) bool {
	s.mu.Lock()
	running := false
	for _, c := range s.runChat {
		if c == chatID {
			running = true
			break
		}
	}
	s.mu.Unlock()
	s.dispatch.Cancel(chatID)
	return running
}
