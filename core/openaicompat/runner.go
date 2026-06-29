// Package openaicompat runs prompts through an OpenAI-compatible chat
// completions API and adapts responses to the common agent.Runner contract.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/duckbugio/flock/core/agent"
)

// Config configures the answer-only OpenAI-compatible provider.
type Config struct {
	BaseURL string
	Model   string
	APIKey  string
	Timeout time.Duration
	Client  *http.Client
}

// New returns an answer-only agent.Runner backed by a Chat Completions endpoint.
func New(cfg Config) agent.Runner {
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: cfg.Timeout}
	}
	return &runner{cfg: cfg}
}

type runner struct {
	cfg Config
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func (r *runner) Run(ctx context.Context, prompt string, o agent.Options) (<-chan agent.Event, error) {
	out := make(chan agent.Event)
	go r.run(ctx, prompt, o, out)
	return out, nil
}

func (r *runner) run(ctx context.Context, prompt string, o agent.Options, out chan<- agent.Event) {
	defer close(out)

	text, _, err := r.complete(ctx, prompt, o)
	if err != nil {
		emit(ctx, out, agent.Event{Type: agent.RunError, Err: err})
		return
	}
	if text != "" {
		if !emit(ctx, out, agent.Event{Type: agent.Text, Text: text}) {
			return
		}
	}
	emit(ctx, out, agent.Event{
		Type: agent.Result,
		Result: &agent.RunResult{
			Text:     text,
			NumTurns: 1,
			Subtype:  "success",
			// CostUSD is intentionally left zero: generic OpenAI-compatible APIs
			// do not share one pricing table. A future estimated-cost mode can use
			// provider token usage with operator-configured prices.
			CostUSD: 0,
		},
	})
}

func (r *runner) complete(ctx context.Context, prompt string, o agent.Options) (string, tokenUsage, error) {
	model := strings.TrimSpace(o.Model)
	if model == "" {
		model = strings.TrimSpace(r.cfg.Model)
	}
	if model == "" {
		return "", tokenUsage{}, errors.New("openai-compatible model is required")
	}
	endpoint := endpoint(r.cfg.BaseURL)
	if endpoint == "" {
		return "", tokenUsage{}, errors.New("openai-compatible base URL is required")
	}

	body, err := json.Marshal(chatRequest{
		Model:    model,
		Messages: []chatMessage{{Role: "user", Content: prompt}},
		Stream:   false,
	})
	if err != nil {
		return "", tokenUsage{}, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", tokenUsage{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(r.cfg.APIKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(r.cfg.APIKey))
	}

	resp, err := r.cfg.Client.Do(req)
	if err != nil {
		return "", tokenUsage{}, fmt.Errorf("openai-compatible request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		tail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if len(tail) > 0 {
			return "", tokenUsage{}, fmt.Errorf("openai-compatible HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(tail)))
		}
		return "", tokenUsage{}, fmt.Errorf("openai-compatible HTTP %d", resp.StatusCode)
	}

	var decoded chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return "", tokenUsage{}, fmt.Errorf("decode response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return "", tokenUsage{}, errors.New("openai-compatible response had no choices")
	}
	text := decoded.Choices[0].Message.Content
	usage := tokenUsage{
		PromptTokens:     decoded.Usage.PromptTokens,
		CompletionTokens: decoded.Usage.CompletionTokens,
		TotalTokens:      decoded.Usage.TotalTokens,
	}
	return text, usage, nil
}

type tokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

func endpoint(baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return ""
	}
	return strings.TrimRight(baseURL, "/") + "/chat/completions"
}

func emit(ctx context.Context, out chan<- agent.Event, e agent.Event) bool {
	select {
	case out <- e:
		return true
	case <-ctx.Done():
		return false
	}
}
