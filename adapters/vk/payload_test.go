//nolint:testpackage // intentionally whitebox to test unexported VK payload internals
package vk

import (
	"encoding/json"
	"testing"
)

func TestStopPayloadRoundTrip(t *testing.T) {
	raw := stopPayload("run-7")
	p := parsePayload(json.RawMessage(raw))
	if p.Stop != "run-7" {
		t.Errorf("parsed stop = %q, want run-7", p.Stop)
	}
	if p.Star {
		t.Error("stop payload should not set star")
	}
}

func TestStarPayloadRoundTrip(t *testing.T) {
	raw := starPayloadJSON()
	p := parsePayload(json.RawMessage(raw))
	if !p.Star {
		t.Error("parsed star = false, want true")
	}
	if p.Stop != "" {
		t.Errorf("star payload should not set stop, got %q", p.Stop)
	}
}

func TestParsePayloadStringWrapped(t *testing.T) {
	// VK may relay the payload as a JSON-encoded STRING; parsePayload must unwrap it.
	wrapped, err := json.Marshal(`{"stop":"run-9"}`)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	p := parsePayload(wrapped)
	if p.Stop != "run-9" {
		t.Errorf("string-wrapped parsed stop = %q, want run-9", p.Stop)
	}
}

func TestParsePayloadMalformedIsNoop(t *testing.T) {
	p := parsePayload(json.RawMessage(`not json`))
	if p.Stop != "" || p.Star {
		t.Errorf("malformed payload = %+v, want zero value (no action)", p)
	}
	if p := parsePayload(nil); p.Stop != "" || p.Star {
		t.Errorf("nil payload = %+v, want zero value", p)
	}
}
