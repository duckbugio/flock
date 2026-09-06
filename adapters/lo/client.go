// Package lo adapts LO's supported Bot API subset to Flock's chat service.
package lo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	requestTimeout     = 65 * time.Second
	maxResponseBytes   = 2 << 20
	pollTimeoutSeconds = 30
	// Leave room for full-size Unicode text plus quoted messages under the response cap.
	pollBatchSize = 25
)

// Client sends Bot API requests only to its configured LO endpoint.
type Client struct {
	base, token string
	http        *http.Client
}

// NewClient accepts HTTPS endpoints or loopback HTTP for local integration tests.
// Redirects are refused: the bot credential is part of every request path.
func NewClient(base, token string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("LO_API_URL must be a base URL without credentials, query or fragment")
	}
	loopback := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback) {
		return nil, errors.New("LO_API_URL requires HTTPS (loopback HTTP is allowed for development)")
	}
	if token == "" || strings.ContainsAny(token, "/?#% \t\r\n") {
		return nil, errors.New("LO_BOT_TOKEN is missing or malformed")
	}
	var client http.Client
	if hc != nil {
		client = *hc
	}
	if client.Timeout == 0 {
		client.Timeout = requestTimeout
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: &client}, nil
}

// APIError preserves the numeric status and retry delay without exposing credentials.
type APIError struct {
	Code        int
	Description string
	Delay       time.Duration
}

func (e *APIError) Error() string { return fmt.Sprintf("LO Bot API %d: %s", e.Code, e.Description) }

// RetryAfter classifies the Bot API 429 envelope for the shared delivery loop.
func RetryAfter(err error) (time.Duration, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusTooManyRequests {
		if apiErr.Delay > 0 {
			return apiErr.Delay, true
		}
		return time.Second, true
	}
	return 0, false
}

func (c *Client) call(ctx context.Context, method string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode LO request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/bot"+c.token+"/"+method, bytes.NewReader(raw))
	if err != nil {
		return errors.New("cannot construct LO request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err // The outer URL contains the bot credential.
		}
		reason := strings.ReplaceAll(err.Error(), c.token, "[REDACTED]")
		return fmt.Errorf("LO request failed before receiving a response: %s", reason)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return errors.New("cannot read LO response")
	}
	if len(data) > maxResponseBytes {
		return errors.New("LO response exceeds size limit")
	}
	var envelope struct {
		OK          bool            `json:"ok"`
		Code        int             `json:"error_code"` //nolint:tagliatelle // Bot API wire spelling.
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
		Parameters  struct {
			RetryAfter int64 `json:"retry_after"` //nolint:tagliatelle // Bot API wire spelling.
		} `json:"parameters"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		if resp.StatusCode >= http.StatusMultipleChoices {
			return &APIError{Code: resp.StatusCode, Description: "non-JSON HTTP error response"}
		}
		return fmt.Errorf("LO returned an invalid API envelope (HTTP %d)", resp.StatusCode)
	}
	if !envelope.OK || resp.StatusCode != http.StatusOK {
		code := envelope.Code
		if code == 0 {
			code = resp.StatusCode
		}
		delay := envelope.Parameters.RetryAfter
		if delay <= 0 {
			delay, _ = strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64)
		}
		// Preserve the server minimum without overflowing time.Duration.
		const maxBackoffSeconds = int64((1<<63 - 1) / time.Second)
		if delay > maxBackoffSeconds {
			delay = maxBackoffSeconds
		}
		return &APIError{
			Code: code, Description: strings.ReplaceAll(envelope.Description, c.token, "[redacted]"),
			Delay: time.Duration(max(delay, 0)) * time.Second,
		}
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return errors.New("LO response contains no result")
	}
	if err := json.Unmarshal(envelope.Result, out); err != nil {
		return errors.New("LO response contains an invalid result")
	}
	return nil
}

// User is the Bot API actor; LO and Telegram IDs are independent.
type User struct {
	ID       int64  `json:"id"`
	IsBot    bool   `json:"is_bot"` //nolint:tagliatelle // Bot API wire spelling.
	Username string `json:"username"`
}

// Message is the inbound subset the text adapter understands.
type Message struct {
	ID   int64 `json:"message_id"` //nolint:tagliatelle // Bot API wire spelling.
	From *User `json:"from"`
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Text      string          `json:"text"`
	Caption   string          `json:"caption"`
	Photo     json.RawMessage `json:"photo"`
	Document  json.RawMessage `json:"document"`
	Voice     json.RawMessage `json:"voice"`
	Audio     json.RawMessage `json:"audio"`
	Video     json.RawMessage `json:"video"`
	Animation json.RawMessage `json:"animation"`
	Sticker   json.RawMessage `json:"sticker"`
	VideoNote json.RawMessage `json:"video_note"`       //nolint:tagliatelle // Bot API wire spelling.
	Reply     *Message        `json:"reply_to_message"` //nolint:tagliatelle // Bot API wire spelling.
}

// Update is an ordered getUpdates item. Unsupported event kinds are acknowledged and ignored.
type Update struct {
	ID      int64    `json:"update_id"` //nolint:tagliatelle // Bot API wire spelling.
	Message *Message `json:"message"`
}

// GetMe validates the token and discovers the authoritative username at startup.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	var user User
	if err := c.call(ctx, "getMe", struct{}{}, &user); err != nil {
		return User{}, err
	}
	if user.ID <= 0 || !user.IsBot {
		return User{}, errors.New("LO getMe did not return a bot")
	}
	return user, nil
}

// GetUpdates requests only new messages; it never drops queued updates or disables webhooks.
func (c *Client) GetUpdates(ctx context.Context, offset int64) ([]Update, error) {
	var updates []Update
	body := map[string]any{
		"offset": offset, "timeout": pollTimeoutSeconds, "limit": pollBatchSize, "allowed_updates": []string{"message"},
	}
	err := c.call(ctx, "getUpdates", body, &updates)
	return updates, err
}

// CheckPolling refuses an active webhook without mutating it. Older deployments
// that explicitly lack this read method fall back to getUpdates' conflict check.
func (c *Client) CheckPolling(ctx context.Context) error {
	var info struct {
		URL string `json:"url"`
	}
	if err := c.call(ctx, "getWebhookInfo", struct{}{}, &info); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && (apiErr.Code == http.StatusNotFound || apiErr.Code == http.StatusNotImplemented) {
			slog.Warn("LO deployment does not support getWebhookInfo; continuing with polling")
			return nil
		}
		return err
	}
	if info.URL != "" {
		return errors.New("LO bot has an active webhook; use a dedicated polling bot or remove its webhook explicitly")
	}
	return nil
}
