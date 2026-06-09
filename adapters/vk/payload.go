package vk

import "encoding/json"

// callbackPayload is the JSON object carried by an inline callback button. Stop
// buttons set Stop to the run id; the star-nudge button sets Star to true. A
// message_event update relays this payload back verbatim, so the handler decodes
// it to route the press.
type callbackPayload struct {
	// Stop, when non-empty, is the run id a Stop button cancels.
	Stop string `json:"stop,omitempty"`
	// Star, when true, marks the star-nudge confirm button.
	Star bool `json:"star,omitempty"`
}

// starButtonText labels the star-nudge confirm button. Pressing it triggers the
// server-side star (a callback button). It mirrors the Telegram nudge's copy.
const starButtonText = "⭐ Поставить звезду"

// stopPayload encodes the callback payload for a Stop button bound to runID.
func stopPayload(runID string) string {
	return marshalPayload(callbackPayload{Stop: runID})
}

// starPayloadJSON encodes the callback payload for the star-nudge confirm button.
func starPayloadJSON() string {
	return marshalPayload(callbackPayload{Star: true})
}

// marshalPayload JSON-encodes p. The shape is fixed and always marshalable, so on
// the impossible error path it returns an empty JSON object.
func marshalPayload(p callbackPayload) string {
	b, err := json.Marshal(p)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// parsePayload decodes a callback button payload (the raw JSON VK relays in a
// message_event). A malformed payload yields a zero-value callbackPayload, which
// routes to no action (a safe no-op).
func parsePayload(raw json.RawMessage) callbackPayload {
	var p callbackPayload
	if len(raw) == 0 {
		return p
	}
	// VK may relay the payload either as a JSON object or, when the button payload
	// was stored as a JSON-encoded string, as a quoted string. Try the object form
	// first, then the string-wrapped form.
	if err := json.Unmarshal(raw, &p); err == nil {
		return p
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		_ = json.Unmarshal([]byte(s), &p)
	}
	return p
}
