package lo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/duckbugio/flock/core/chat"
)

// HelpText describes the functional fallback when LO cannot display callback keyboards.
const HelpText = `Flock LO assistant

/start, /help — show commands
/new — reset the session
/stop — stop the current run
/goal <criterion> — arm a goal (/goal off to disarm)
/schedule — manage scheduled jobs

Send text to work with the assistant. Use /stop instead of a Stop button.
Attachments, rich messages and native replies are not supported yet.`

func (r *Receiver) reserved(ctx context.Context, chatID string, userID int64, name, args string) {
	switch name {
	case "start", "help":
		r.notify(ctx, chatID, HelpText)
	case "stop":
		text := "No active run."
		if r.cfg.Service.StopChat(chatID) {
			text = "Stopping the current run."
		}
		r.notify(ctx, chatID, text)
	case "new":
		if err := r.cfg.Service.NewSession(chatID); err != nil {
			r.cfg.Logger.Warn("lo: reset session failed", "error", err)
			r.notify(ctx, chatID, "Could not reset the session. Please retry.")
			return
		}
		r.notify(ctx, chatID, "Started a fresh session.")
	case "schedule":
		if r.cfg.Scheduler == nil {
			r.notify(ctx, chatID, "Scheduler is disabled. Set ENABLE_SCHEDULER=true to enable it.")
			return
		}
		r.notify(ctx, chatID, r.cfg.Scheduler.Dispatch(chatID, userID, args))
	case "goal":
		r.goal(ctx, chatID, args)
	}
}

func (r *Receiver) goal(ctx context.Context, chatID, args string) {
	switch strings.ToLower(args) {
	case "":
		if g, ok := r.cfg.Service.GoalStatus(chatID); ok {
			r.notify(ctx, chatID, fmt.Sprintf("Goal (%d/%d): %s", g.Attempts, g.MaxAttempts, g.Criterion))
			return
		}
		r.notify(ctx, chatID, "No goal armed. Use /goal <criterion>.")
	case "off", "clear", "stop":
		r.cfg.Service.DisarmGoal(chatID)
		r.notify(ctx, chatID, "Goal disarmed.")
	default:
		if _, ok := r.cfg.Service.ArmGoal(chatID, args); !ok {
			r.notify(ctx, chatID, "The goal evaluator is disabled on this deployment.")
			return
		}
		r.notify(ctx, chatID, "Goal armed. The evaluator will check completed runs.")
	}
}

func command(text string) (name, args string) {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return "", ""
	}
	return strings.ToLower(strings.TrimPrefix(fields[0], "/")), strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
}

// addressedText rejects /command@other_bot and removes only exact mention tokens.
func addressedText(text, username string) (string, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return text, true
	}
	if strings.HasPrefix(fields[0], "/") {
		if cmd, target, ok := strings.Cut(fields[0], "@"); ok {
			if username == "" || !strings.EqualFold(target, username) {
				return "", false
			}
			return cmd + strings.TrimPrefix(text, fields[0]), true
		}
	}
	if username != "" {
		for _, field := range fields {
			if strings.EqualFold(field, "@"+username) {
				return strings.TrimSpace(strings.Replace(text, field, "", 1)), true
			}
		}
	}
	return text, true
}

func fatalAPIError(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) &&
		(apiErr.Code == http.StatusUnauthorized || apiErr.Code == http.StatusForbidden || apiErr.Code == http.StatusConflict)
}

// SetCommands registers the shared reserved command list only when explicitly enabled.
func (c *Client) SetCommands(ctx context.Context) error {
	commands := make([]map[string]string, 0, len(chat.ReservedCommands))
	for _, cmd := range chat.ReservedCommands {
		commands = append(commands, map[string]string{"command": cmd.Name, "description": cmd.Description})
	}
	var result bool
	if err := c.call(ctx, "setMyCommands", map[string]any{"commands": commands}, &result); err != nil {
		return err
	}
	if !result {
		return errors.New("LO did not confirm command registration")
	}
	return nil
}
