// Package claude runs prompts through the Claude CLI and adapts its stream to
// the common agent.Runner contract.
package claude

import "github.com/duckbugio/flock/core/agent"

// EventType aliases agent.EventType for backward compatibility.
type EventType = agent.EventType

const (
	// SystemInit aliases agent.SystemInit.
	SystemInit = agent.SystemInit
	// Text aliases agent.Text.
	Text = agent.Text
	// ToolUse aliases agent.ToolUse.
	ToolUse = agent.ToolUse
	// ToolResult aliases agent.ToolResult.
	ToolResult = agent.ToolResult
	// Result aliases agent.Result.
	Result = agent.Result
	// RunError aliases agent.RunError.
	RunError = agent.RunError
)

// Event aliases agent.Event for backward compatibility.
type Event = agent.Event

// RunResult aliases agent.RunResult for backward compatibility.
type RunResult = agent.RunResult

// ImageInput aliases agent.ImageInput for backward compatibility.
type ImageInput = agent.ImageInput

// Options aliases agent.Options for backward compatibility.
type Options = agent.Options

// Runner aliases agent.Runner for backward compatibility.
type Runner = agent.Runner
