package claude

import "encoding/json"

// roleUser is the stream-json role/type value for user-authored messages.
const roleUser = "user"

// envelope is the tolerant top-level shape of a stream-json line. Only the
// fields we act on are decoded; unknown fields and event types are ignored.
type envelope struct {
	Type      string       `json:"type"`
	Subtype   string       `json:"subtype"`
	SessionID string       `json:"session_id"` //nolint:tagliatelle // Claude CLI emits snake_case.
	Message   *messageBody `json:"message"`
	IsError   bool         `json:"is_error"` //nolint:tagliatelle // Claude CLI emits snake_case.
	Result    string       `json:"result"`
	NumTurns  int          `json:"num_turns"`      //nolint:tagliatelle // Claude CLI emits snake_case.
	CostUSD   float64      `json:"total_cost_usd"` //nolint:tagliatelle // Claude CLI emits snake_case.
	Duration  int64        `json:"duration_ms"`    //nolint:tagliatelle // Claude CLI emits snake_case.
	// ParentToolUseID links an event to the subagent that produced it: when the
	// CLI runs a subagent (an Agent/Task tool_use), the subagent's own assistant/
	// user envelopes carry the launching tool_use's id here. It is empty for
	// top-level events. Surfaced on every derived Event as Event.ParentID.
	ParentToolUseID string `json:"parent_tool_use_id"` //nolint:tagliatelle // Claude CLI emits snake_case.
}

// messageBody is the assistant/user message wrapper holding content blocks.
type messageBody struct {
	Content []contentBlock `json:"content"`
}

// contentBlock is a single content item; the shape varies by Type.
type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`  // text block
	Name  string          `json:"name"`  // tool_use block
	Input json.RawMessage `json:"input"` // tool_use block
	ID    string          `json:"id"`    // tool_use block: the tool_use id
}

// decode parses one stream-json line into zero or more Events. The bool is
// false when the line is not valid JSON (skip it) or carries no actionable
// content. Unknown event types and subtypes are skipped gracefully.
func decode(line []byte) ([]Event, bool) {
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, false
	}

	switch env.Type {
	case "system":
		if env.Subtype == "init" {
			return []Event{{Type: SystemInit, SessionID: env.SessionID}}, true
		}
		// system/status and other subtypes are progress noise; skip.
		return nil, false

	case "assistant":
		return assistantEvents(env.Message, env.ParentToolUseID), true

	case roleUser:
		return userEvents(env.Message, env.ParentToolUseID), true

	case "result":
		res := &RunResult{
			Text:       env.Result,
			SessionID:  env.SessionID,
			NumTurns:   env.NumTurns,
			CostUSD:    env.CostUSD,
			DurationMS: env.Duration,
			IsError:    env.IsError,
			Subtype:    env.Subtype,
		}
		return []Event{{Type: Result, Result: res}}, true

	default:
		return nil, false
	}
}

// assistantEvents converts an assistant message's content blocks into events.
// parentID is the envelope's parent_tool_use_id, stamped on every derived event
// so an inner subagent's activity can be attributed to its launching tool_use.
func assistantEvents(msg *messageBody, parentID string) []Event {
	if msg == nil {
		return nil
	}
	var evs []Event
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				evs = append(evs, Event{Type: Text, Text: b.Text, ParentID: parentID})
			}
		case "tool_use":
			evs = append(evs, Event{Type: ToolUse, Tool: b.Name, ToolInput: b.Input, ToolID: b.ID, ParentID: parentID})
		}
	}
	return evs
}

// userEvents emits a ToolResult for each tool_result block in a user message.
// parentID is the envelope's parent_tool_use_id, carried through for consistency
// with assistantEvents (inner tool results are linked to their subagent too).
func userEvents(msg *messageBody, parentID string) []Event {
	if msg == nil {
		return nil
	}
	var evs []Event
	for _, b := range msg.Content {
		if b.Type == "tool_result" {
			evs = append(evs, Event{Type: ToolResult, ParentID: parentID})
		}
	}
	return evs
}
