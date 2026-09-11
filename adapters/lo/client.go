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
	requestTimeout = 65 * time.Second
	// fileTimeout covers a whole download, body included, and is deliberately not
	// requestTimeout. That one is sized for a 30-second long poll; reusing it here caps an
	// upload-sized file at what fits in 65 seconds — 20 MiB needs a sustained 320 KB/s — and
	// the user is then told to "try sending it again", which on that link never works. The
	// Telegram and VK adapters carry the same separate client for the same reason.
	fileTimeout        = 120 * time.Second
	maxResponseBytes   = 2 << 20
	pollTimeoutSeconds = 30
	// Leave room for full-size Unicode text plus quoted messages under the response cap.
	pollBatchSize = 25
)

// Client sends Bot API requests only to its configured LO endpoint.
type Client struct {
	base, token string
	http        *http.Client
	// fileHTTP reads file BYTES. A second client rather than a second timeout, because
	// http.Client.Timeout is per-client and covers reading the body — see fileTimeout.
	fileHTTP *http.Client
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
	// The download client copies everything but the timeout — the redirect refusal above most
	// of all. Following one would let Go put the previous URL, which carries the bot token,
	// into the Referer of a request to wherever the redirect points.
	files := client
	files.Timeout = fileTimeout
	return &Client{
		base: strings.TrimRight(base, "/"), token: token,
		http: &client, fileHTTP: &files,
	}, nil
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
	Text    string `json:"text"`
	Caption string `json:"caption"`
	// Photo is the size ladder Bot API sends for one image, ascending. LargestPhoto
	// picks from it; the adapter never assumes a position.
	Photo     []PhotoSize `json:"photo"`
	Document  *Attachment `json:"document"`
	Voice     *Attachment `json:"voice"`
	Audio     *Attachment `json:"audio"`
	Video     *Attachment `json:"video"`
	Animation *Attachment `json:"animation"`
	Sticker   *Attachment `json:"sticker"`
	VideoNote *Attachment `json:"video_note"`       //nolint:tagliatelle // Bot API wire spelling.
	Reply     *Message    `json:"reply_to_message"` //nolint:tagliatelle // Bot API wire spelling.
}

// PhotoSize is one rung of an inbound photo's size ladder.
type PhotoSize struct {
	FileID   string `json:"file_id"`   //nolint:tagliatelle // Bot API wire spelling.
	FileSize int64  `json:"file_size"` //nolint:tagliatelle // Bot API wire spelling.
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

// Attachment is the shared shape of every non-photo inbound file. LO fills the id and
// usually the name; file_size is optional on this platform (see File.FileSize).
type Attachment struct {
	FileID   string `json:"file_id"`   //nolint:tagliatelle // Bot API wire spelling.
	FileName string `json:"file_name"` //nolint:tagliatelle // Bot API wire spelling.
	MimeType string `json:"mime_type"` //nolint:tagliatelle // Bot API wire spelling.
	FileSize int64  `json:"file_size"` //nolint:tagliatelle // Bot API wire spelling.
}

// File is the getFile result. FilePath is EMPTY for media whose bytes this platform does
// not serve (LO answers audio and video references without a path on purpose: an LO audio
// is a catalogue track, and there is no address for the bytes). A caller must check the
// field before attempting a download — that empty path is the platform saying "reference
// yes, bytes no", not a malformed response.
type File struct {
	FileID   string `json:"file_id"`   //nolint:tagliatelle // Bot API wire spelling.
	FilePath string `json:"file_path"` //nolint:tagliatelle // Bot API wire spelling.
	FileSize int64  `json:"file_size"` //nolint:tagliatelle // Bot API wire spelling.
}

// LargestPhoto returns the highest-resolution rung of a photo ladder. An agent reading a
// screenshot needs the pixels, so the largest rung is the only useful one; ok is false for
// a ladder with no usable id.
func LargestPhoto(sizes []PhotoSize) (PhotoSize, bool) {
	var best PhotoSize
	var bestScore int64
	for _, size := range sizes {
		if size.FileID == "" {
			continue
		}
		// FileSize first, pixel area as the fallback — the Telegram adapter's rule, and it
		// matters for the same reason: a rung can be the biggest on paper and the most
		// compressed in fact, so bytes describe "most detail" better than dimensions. LO
		// omits file_size for rungs it has not measured, which is what the fallback is for.
		score := size.FileSize
		if score == 0 {
			score = int64(size.Width) * int64(size.Height)
		}
		if best.FileID == "" || score > bestScore {
			best, bestScore = size, score
		}
	}
	return best, best.FileID != ""
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
//
// The batch is decoded one update at a time, and an update that will not decode is SKIPPED
// rather than failing the batch. Since the media fields became typed, an unexpected shape in
// any one of them — a field this adapter models as an object arriving as something else —
// would fail the whole json.Unmarshal; the receiver would then not advance its offset, ask for
// the same batch again, and the bot would stop answering anyone until that update expired.
// Losing one message the adapter cannot read costs a message; the alternative costs the bot.
//
// The update_id is still read from the skipped update where possible, so the offset advances
// past it — an id is one integer, and the shapes that break here are inside `message`.
func (c *Client) GetUpdates(ctx context.Context, offset int64) ([]Update, error) {
	var raw []json.RawMessage
	body := map[string]any{
		"offset": offset, "timeout": pollTimeoutSeconds, "limit": pollBatchSize, "allowed_updates": []string{"message"},
	}
	if err := c.call(ctx, "getUpdates", body, &raw); err != nil {
		return nil, err
	}
	updates := make([]Update, 0, len(raw))
	for _, item := range raw {
		var update Update
		if err := json.Unmarshal(item, &update); err == nil {
			updates = append(updates, update)
			continue
		}
		// json.Number rather than int64: an id that arrives as a STRING would fail an int64
		// decode, and this adapter would then skip the update without advancing past it — so
		// a poisoned LAST update in a batch would be re-fetched forever. That is the same
		// stall this whole loop removes, just narrower, and the fallback costs one conversion.
		var header struct {
			ID json.Number `json:"update_id"` //nolint:tagliatelle // Bot API wire spelling.
		}
		var (
			id    int64
			idErr = json.Unmarshal(item, &header)
		)
		if idErr == nil {
			id, idErr = header.ID.Int64()
		}
		if idErr != nil || id <= 0 {
			// Not even an id: nothing to acknowledge, and nothing to answer.
			slog.Warn("LO sent an update this adapter cannot read at all")
			continue
		}
		// An id and a body that will not decode: keep the id so the offset moves past it,
		// and leave Message nil, which the receiver already treats as nothing to do.
		slog.Warn("LO sent an update this adapter cannot read", "update_id", id)
		updates = append(updates, Update{ID: id})
	}
	return updates, nil
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

// GetFile resolves an inbound file reference to a downloadable path. A successful call with
// an EMPTY FilePath is a valid answer, not a failure: the reference is real (sendPhoto and
// friends take it back) while the bytes are not served. Callers must branch on that rather
// than treat it as an error, because the two cases need different words to the user.
func (c *Client) GetFile(ctx context.Context, fileID string) (File, error) {
	if strings.TrimSpace(fileID) == "" {
		return File{}, errors.New("LO getFile requires a file id")
	}
	var file File
	if err := c.call(ctx, "getFile", map[string]any{"file_id": fileID}, &file); err != nil {
		return File{}, err
	}
	return file, nil
}

// downloadURL builds the byte address for a file_path from GetFile. The result embeds the
// bot token: it must never be logged, returned in an error, or handed to anything that
// records URLs. Keep it inside the request that uses it.
func (c *Client) downloadURL(filePath string) string {
	return c.base + "/file/bot" + c.token + "/" + strings.TrimLeft(filePath, "/")
}

// Download streams a file_path's bytes. The caller closes the returned body. Errors are
// scrubbed of the token, including the URL a transport error would otherwise carry.
func (c *Client) Download(ctx context.Context, filePath string) (io.ReadCloser, error) {
	if strings.TrimSpace(filePath) == "" {
		return nil, errors.New("LO download requires a file path")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.downloadURL(filePath), nil)
	if err != nil {
		return nil, errors.New("cannot construct LO download request")
	}
	resp, err := c.fileHTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err // The outer URL carries the bot credential.
		}
		return nil, fmt.Errorf("LO download failed: %s", strings.ReplaceAll(err.Error(), c.token, "[REDACTED]"))
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, &APIError{Code: resp.StatusCode, Description: "file download refused"}
	}
	return resp.Body, nil
}
