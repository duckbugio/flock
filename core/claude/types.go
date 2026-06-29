package claude

import "github.com/duckbugio/flock/core/agent"

type EventType = agent.EventType

const (
	SystemInit = agent.SystemInit
	Text       = agent.Text
	ToolUse    = agent.ToolUse
	ToolResult = agent.ToolResult
	Result     = agent.Result
	RunError   = agent.RunError
)

type Event = agent.Event
type RunResult = agent.RunResult
type ImageInput = agent.ImageInput
type Options = agent.Options
type Runner = agent.Runner
