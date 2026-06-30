// Package agent defines the provider-neutral runtime contract spoken by the
// conversation layer and implemented by AI backends.
package agent

import (
	"context"
	"encoding/json"
)

// EventType discriminates the kinds of Event emitted on a run's channel.
type EventType int

const (
	// SystemInit is emitted once for the provider's session/thread init event.
	SystemInit EventType = iota
	// Text is emitted for assistant text content.
	Text
	// ToolUse is emitted when the provider starts a tool/command/action.
	ToolUse
	// ToolResult is emitted when a tool/command/action finishes.
	ToolResult
	// Result is the terminal event for a successful stream.
	Result
	// RunError is emitted when the provider run fails.
	RunError
)

// Event is a provider-neutral item from a run's event stream.
type Event struct {
	Type      EventType
	Text      string
	Tool      string
	ToolInput json.RawMessage
	SessionID string
	Result    *RunResult
	Err       error
	// ParentID links provider events produced inside a nested/subagent run back to
	// the launching tool call, when the provider exposes that relationship.
	ParentID string
	// ToolID is the provider's id for a tool-use event. When a subagent launch has
	// an id, later nested events may carry it as ParentID.
	ToolID string
}

// RunResult is the parsed terminal result of a provider run.
type RunResult struct {
	Text       string
	SessionID  string
	NumTurns   int
	CostUSD    float64
	DurationMS int64
	IsError    bool
	Subtype    string
}

// ImageInput is a single image attachment for a run.
type ImageInput struct {
	MediaType string
	Data      []byte
	Path      string
}

// Options configures a single provider run.
type Options struct {
	SessionID string
	Workdir   string
	Model     string
	MaxTurns  int
	MCPConfig string
	Effort    string
	Env       []string
	Images    []ImageInput
}

// Runner runs prompts through a provider.
type Runner interface {
	Run(ctx context.Context, prompt string, o Options) (<-chan Event, error)
}

// Capabilities describes which parts of the common agent contract a provider
// can actually support.
type Capabilities struct {
	ResumeSessions       bool
	StreamEvents         bool
	ToolEvents           bool
	ShellExecution       bool
	FileEditing          bool
	Images               bool
	CostReporting        bool
	SubscriptionAuth     bool
	APIKeyBilling        bool
	RequiresGitRepo      bool
	SupportsSandbox      bool
	SupportsApprovalMode bool
}

// ProviderInfo is the selected provider metadata surfaced at startup.
type ProviderInfo struct {
	Name         string
	DisplayName  string
	Capabilities Capabilities
}

// Provider exposes provider metadata without tying core/agent to app config.
type Provider interface {
	Name() string
	DisplayName() string
	Capabilities() Capabilities
}
