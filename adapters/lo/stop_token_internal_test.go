package lo

import (
	"strings"
	"testing"
)

func TestStopButtonRejectsRestartedAndForeignRuns(t *testing.T) {
	t.Parallel()
	transport := NewTransport(nil, false)
	token := transport.stopButtonData("77", "1")
	if token == "" || len(token) > 64 || transport.stopButtonRun("77", token) != "1" {
		t.Fatal("valid run token rejected")
	}
	if transport.stopButtonRun("88", token) != "" {
		t.Fatal("token accepted in another chat")
	}
	if NewTransport(nil, false).stopButtonRun("77", token) != "" {
		t.Fatal("old token authorizes restarted run counter")
	}
	if transport.stopButtonRun("77", strings.Replace(token, "stop:1:", "stop:2:", 1)) != "" {
		t.Fatal("token authorizes a different run")
	}
	if transport.stopButtonRun("77", token+"x") != "" {
		t.Fatal("modified signature accepted")
	}
}

func TestStopButtonBoundsAndCanonicalRunID(t *testing.T) {
	t.Parallel()
	transport := NewTransport(nil, false)
	for _, runID := range []string{"", "0", "01", "-1", "1:other", "18446744073709551616"} {
		if transport.stopButtonData("77", runID) != "" {
			t.Errorf("invalid run %q accepted", runID)
		}
	}
	const largestRun = "18446744073709551615"
	token := transport.stopButtonData("-77", largestRun)
	if len(token) > 64 || transport.stopButtonRun("-77", token) != largestRun {
		t.Fatal("largest run does not fit")
	}
	for _, data := range []string{"stop:", "star:confirm", "stop:1", strings.Repeat("x", 65)} {
		if transport.stopButtonRun("77", data) != "" {
			t.Errorf("malformed token %q accepted", data)
		}
	}
}
