//nolint:testpackage // intentionally whitebox to test the unexported decode path.
package claude

import "testing"

// TestDecodeAssistantParentAndToolID (AC2) feeds a raw `assistant` stream-json
// line that carries a top-level "parent_tool_use_id" and a tool_use content block
// with an "id", and asserts the derived events surface both: every event carries
// ParentID from the envelope, and the ToolUse event carries ToolID from the block.
func TestDecodeAssistantParentAndToolID(t *testing.T) {
	line := []byte(`{"type":"assistant","parent_tool_use_id":"toolu_X",` +
		`"message":{"role":"assistant","content":[` +
		`{"type":"text","text":"thinking"},` +
		`{"type":"tool_use","name":"Bash","id":"toolu_Y","input":{"command":"go test ./..."}}` +
		`]}}`)

	evs, ok := decode(line)
	if !ok {
		t.Fatal("decode returned ok=false for a valid assistant line")
	}
	if len(evs) != 2 {
		t.Fatalf("expected 2 events (text + tool_use), got %d: %+v", len(evs), evs)
	}

	// Every derived event carries the envelope's parent_tool_use_id.
	for i, e := range evs {
		if e.ParentID != "toolu_X" {
			t.Errorf("event %d ParentID = %q, want %q", i, e.ParentID, "toolu_X")
		}
	}

	// The text event has no ToolID; the tool_use event carries the block id.
	text, tool := evs[0], evs[1]
	if text.Type != Text {
		t.Fatalf("event 0 type = %v, want Text", text.Type)
	}
	if text.ToolID != "" {
		t.Errorf("text event ToolID = %q, want empty", text.ToolID)
	}
	if tool.Type != ToolUse {
		t.Fatalf("event 1 type = %v, want ToolUse", tool.Type)
	}
	if tool.ToolID != "toolu_Y" {
		t.Errorf("tool_use ToolID = %q, want %q", tool.ToolID, "toolu_Y")
	}
}

// TestDecodeUserToolResultCarriesParentID (AC2) asserts the user/tool_result path
// also threads parent_tool_use_id onto its derived ToolResult event, so an inner
// subagent's tool results are linked back to the subagent too.
func TestDecodeUserToolResultCarriesParentID(t *testing.T) {
	line := []byte(`{"type":"user","parent_tool_use_id":"toolu_Z",` +
		`"message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"toolu_Y","content":"ok"}` +
		`]}}`)

	evs, ok := decode(line)
	if !ok {
		t.Fatal("decode returned ok=false for a valid user line")
	}
	if len(evs) != 1 {
		t.Fatalf("expected 1 ToolResult event, got %d: %+v", len(evs), evs)
	}
	if evs[0].Type != ToolResult {
		t.Fatalf("event type = %v, want ToolResult", evs[0].Type)
	}
	if evs[0].ParentID != "toolu_Z" {
		t.Errorf("tool_result ParentID = %q, want %q", evs[0].ParentID, "toolu_Z")
	}
}

// TestDecodeTopLevelEventsHaveNoParent guards the no-subagent case: a top-level
// assistant line with no parent_tool_use_id yields events whose ParentID is empty,
// so per-line attribution stays a no-op (the frame is byte-identical to before).
func TestDecodeTopLevelEventsHaveNoParent(t *testing.T) {
	line := []byte(`{"type":"assistant",` +
		`"message":{"role":"assistant","content":[` +
		`{"type":"tool_use","name":"Agent","id":"toolu_A","input":{"subagent_type":"coder"}}` +
		`]}}`)

	evs, ok := decode(line)
	if !ok {
		t.Fatal("decode returned ok=false for a valid assistant line")
	}
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(evs), evs)
	}
	if evs[0].ParentID != "" {
		t.Errorf("top-level event ParentID = %q, want empty", evs[0].ParentID)
	}
	if evs[0].ToolID != "toolu_A" {
		t.Errorf("ToolUse ToolID = %q, want %q", evs[0].ToolID, "toolu_A")
	}
}
