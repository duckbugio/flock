package agent

import (
	"encoding/json"
	"testing"
)

func TestEventContractCarriesProviderNeutralRunData(t *testing.T) {
	input := json.RawMessage(`{"command":"go test ./..."}`)
	ev := Event{
		Type:      ToolUse,
		Tool:      "command_execution",
		ToolInput: input,
		SessionID: "session-1",
		ParentID:  "parent-tool",
		ToolID:    "tool-1",
		Result: &RunResult{
			Text:      "done",
			SessionID: "session-1",
			IsError:   false,
			Subtype:   "success",
		},
	}
	if ev.Type != ToolUse || ev.Tool != "command_execution" {
		t.Fatalf("event = %+v, want provider-neutral tool event", ev)
	}
	if string(ev.ToolInput) != string(input) {
		t.Fatalf("ToolInput = %s, want %s", ev.ToolInput, input)
	}
	if ev.Result.Text != "done" || ev.Result.SessionID != "session-1" {
		t.Fatalf("Result = %+v, want final text/session", ev.Result)
	}
	if ev.ParentID != "parent-tool" || ev.ToolID != "tool-1" {
		t.Fatalf("event ids = parent:%q tool:%q, want parent-tool/tool-1", ev.ParentID, ev.ToolID)
	}
}

func TestCapabilitiesExposeProviderFeatureFlags(t *testing.T) {
	caps := Capabilities{
		ResumeSessions:       true,
		ToolEvents:           true,
		SubscriptionAuth:     true,
		APIKeyBilling:        true,
		SupportsSandbox:      true,
		SupportsApprovalMode: true,
	}
	if !caps.ResumeSessions || !caps.ToolEvents || !caps.SubscriptionAuth || !caps.APIKeyBilling {
		t.Fatalf("capabilities = %+v, want resume/tool/auth/billing flags", caps)
	}
	if !caps.SupportsSandbox || !caps.SupportsApprovalMode {
		t.Fatalf("capabilities = %+v, want sandbox/approval flags", caps)
	}
}

func TestProviderMetadataInterface(t *testing.T) {
	var p Provider = staticProvider{}
	if p.Name() != "test" || p.DisplayName() != "Test Provider" {
		t.Fatalf("provider metadata = %q/%q", p.Name(), p.DisplayName())
	}
	if !p.Capabilities().StreamEvents {
		t.Fatalf("provider capabilities = %+v, want stream events", p.Capabilities())
	}
}

type staticProvider struct{}

func (staticProvider) Name() string        { return "test" }
func (staticProvider) DisplayName() string { return "Test Provider" }
func (staticProvider) Capabilities() Capabilities {
	return Capabilities{StreamEvents: true}
}
