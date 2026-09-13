package lo

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
)

const stopButtonPrefix = "stop:"

// stopButtonData binds a run to its chat and this transport's process lifetime.
// Run counters restart from one, so an old message must not authorize a new run.
func (t *Transport) stopButtonData(chatID, runID string) string {
	run, err := strconv.ParseUint(runID, 10, 64)
	if err != nil || run == 0 || strconv.FormatUint(run, 10) != runID || t.callbackKey == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(t.callbackKey))
	_, _ = mac.Write([]byte(chatID + "\x00" + runID))
	// A 128-bit tag keeps even the largest run ID below callback_data's 64-byte limit.
	tag := base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
	return stopButtonPrefix + runID + ":" + tag
}

// stopButtonRun accepts only a token generated for this chat during this process.
func (t *Transport) stopButtonRun(chatID, data string) string {
	if len(data) > 64 || !strings.HasPrefix(data, stopButtonPrefix) {
		return ""
	}
	runID, _, found := strings.Cut(strings.TrimPrefix(data, stopButtonPrefix), ":")
	if !found {
		return ""
	}
	expected := t.stopButtonData(chatID, runID)
	if expected == "" || !hmac.Equal([]byte(data), []byte(expected)) {
		return ""
	}
	return runID
}
