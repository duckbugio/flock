package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/duckbugio/flock/core/agent"
)

const (
	secretaryMaxOutputTokens = 4096
	secretaryHTTPTimeout     = 2 * time.Minute
	secretaryMaxResponseSize = 1 << 20
)

// answerOnly uses the selected Codex model through Responses without offering
// any tools. Subscription login is CLI-only and cannot provide this guarantee.
func (r *runner) answerOnly(ctx context.Context, prompt string, o agent.Options) (<-chan agent.Event, error) {
	if r.authMode() != AuthBilling {
		return nil, errors.New("codex secretary requires CODEX_AUTH_MODE=billing for a tool-free Responses API request")
	}
	if len(r.cfg.ExtraArgs) > 0 {
		return nil, errors.New("codex secretary cannot use custom Codex CLI arguments with the direct Responses API")
	}
	for _, key := range []string{"OPENAI_BASE_URL", "CODEX_BASE_URL", "CODEX_API_BASE_URL"} {
		if envValue(o.Env, key) != "" {
			return nil, fmt.Errorf("codex secretary cannot use %s with the direct Responses API", key)
		}
	}
	home := envValue(o.Env, "CODEX_HOME")
	if home != "" {
		//nolint:gosec // CODEX_HOME is operator config; only its fixed config.toml is read.
		data, err := os.ReadFile(filepath.Join(home, "config.toml"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read Codex config for secretary: %w", err)
		}
		if strings.Contains(string(data), "model_provider") {
			return nil, errors.New("codex secretary cannot use a custom Codex model provider with the direct Responses API")
		}
	}
	key := envValue(r.childEnv(o.Env), "CODEX_API_KEY")
	if key == "" {
		return nil, errors.New("codex secretary requires CODEX_API_KEY")
	}
	if strings.TrimSpace(o.Model) == "" {
		return nil, errors.New("codex secretary requires the selected CODEX_MODEL")
	}
	out := make(chan agent.Event)
	go r.streamAnswer(ctx, prompt, o.Model, key, out)
	return out, nil
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimSpace(strings.TrimPrefix(env[i], prefix))
		}
	}
	return ""
}

func (r *runner) streamAnswer(ctx context.Context, prompt, model, key string, out chan<- agent.Event) {
	defer close(out)
	//nolint:tagliatelle // OpenAI Responses API uses snake_case request fields.
	requestBody, err := json.Marshal(struct {
		Model     string `json:"model"`
		Input     string `json:"input"`
		Tools     []any  `json:"tools"`
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		Store           bool `json:"store"`
		MaxOutputTokens int  `json:"max_output_tokens"`
	}{
		Model: model, Input: prompt, Tools: []any{}, Store: false,
		Reasoning: struct {
			Effort string `json:"effort"`
		}{Effort: "low"},
		MaxOutputTokens: secretaryMaxOutputTokens,
	})
	if err != nil {
		out <- agent.Event{Type: agent.RunError, Err: err}
		return
	}
	endpoint := r.cfg.AnswerAPIURL
	if endpoint == "" {
		endpoint = "https://api.openai.com/v1/responses"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(requestBody))
	if err != nil {
		out <- agent.Event{Type: agent.RunError, Err: err}
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Timeout: secretaryHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		out <- agent.Event{Type: agent.RunError, Err: fmt.Errorf("codex secretary response: %w", err)}
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Do not forward API error bodies: they may echo private message text.
		out <- agent.Event{Type: agent.RunError, Err: fmt.Errorf("codex secretary response HTTP %d", resp.StatusCode)}
		return
	}
	var answer struct {
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, secretaryMaxResponseSize)).Decode(&answer); err != nil {
		out <- agent.Event{Type: agent.RunError, Err: fmt.Errorf("decode Codex secretary response: %w", err)}
		return
	}
	if answer.Status != "completed" {
		out <- agent.Event{Type: agent.RunError, Err: errors.New("codex secretary response incomplete")}
		return
	}
	var text strings.Builder
	for _, item := range answer.Output {
		if item.Type == "reasoning" {
			continue
		}
		if item.Type != "message" {
			out <- agent.Event{Type: agent.RunError, Err: errors.New("codex secretary response contained a tool item")}
			return
		}
		for _, part := range item.Content {
			if part.Type != "output_text" {
				out <- agent.Event{Type: agent.RunError, Err: errors.New("codex secretary response contained non-text output")}
				return
			}
			_, _ = text.WriteString(part.Text)
		}
	}
	if text.Len() == 0 {
		out <- agent.Event{Type: agent.RunError, Err: errors.New("codex secretary response was empty")}
		return
	}
	select {
	case out <- agent.Event{Type: agent.Result, Result: &agent.RunResult{Text: text.String(), NumTurns: 1, Subtype: "success"}}:
	case <-ctx.Done():
	}
}
