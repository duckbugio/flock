package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-telegram/bot/models"
)

// defaultTelegramAPIBase is the Bot API origin the rich HTTP shim targets,
// matching the go-telegram/bot default. The lib keeps its own server URL private
// (no accessor), so the shim defaults to the same value; a deployment using a
// custom Telegram server URL would need this overridden too (a known limitation
// of the shim, removed once the lib models the rich methods natively).
const defaultTelegramAPIBase = "https://api.telegram.org"

// richTransport sends Bot API 10.1 rich messages. It is an interface so the
// adapter's Send/Edit/StreamDraft can be unit-tested with a fake, and so the HTTP
// shim is swappable when the upstream lib gains native rich support.
type richTransport interface {
	send(ctx context.Context, chatID int64, msg inputRichMessage, markup models.ReplyMarkup) (messageID int, err error)
	edit(ctx context.Context, chatID int64, messageID int, msg inputRichMessage, markup models.ReplyMarkup) error
	streamDraft(ctx context.Context, chatID, draftID int64, msg inputRichMessage) error
}

// httpRichTransport calls the rich methods directly over HTTP. It exists because
// go-telegram/bot v1.21.0 (the latest) does not expose them; it reuses the bot's
// token (b.Token()) and posts JSON to <base>/bot<token>/<method>, exactly the
// endpoint shape the lib itself uses (see FileDownloadLink). The token rides the
// path as the Bot API requires and is never logged here.
type httpRichTransport struct {
	token string
	base  string
	hc    *http.Client
}

func newHTTPRichTransport(token, base string, hc *http.Client) *httpRichTransport {
	if base == "" {
		base = defaultTelegramAPIBase
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	return &httpRichTransport{token: token, base: base, hc: hc}
}

// send posts sendRichMessage and returns the new message id. markup carries the
// Stop inline keyboard (sendRichMessage accepts reply_markup); nil sends none.
func (t *httpRichTransport) send(
	ctx context.Context, chatID int64, msg inputRichMessage, markup models.ReplyMarkup,
) (int, error) {
	body := map[string]any{"chat_id": chatID, "rich_message": msg}
	if markup != nil {
		body["reply_markup"] = markup
	}
	raw, err := t.call(ctx, "sendRichMessage", body)
	if err != nil {
		return 0, err
	}
	var res struct {
		MessageID int `json:"message_id"` //nolint:tagliatelle // Telegram Bot API uses snake_case.
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("decode sendRichMessage result: %w", err)
	}
	return res.MessageID, nil
}

// edit posts editMessageText with the rich_message parameter (Bot API 10.1).
func (t *httpRichTransport) edit(
	ctx context.Context, chatID int64, messageID int, msg inputRichMessage, markup models.ReplyMarkup,
) error {
	body := map[string]any{"chat_id": chatID, "message_id": messageID, "rich_message": msg}
	if markup != nil {
		body["reply_markup"] = markup
	}
	_, err := t.call(ctx, "editMessageText", body)
	return err
}

// streamDraft posts sendRichMessageDraft: an ephemeral ~30s live preview keyed by
// an INTEGER draft_id (non-zero; same id animates an update). A draft carries no
// inline keyboard, so no markup is passed (the Stop button rides the separate
// anchor message, as on the legacy draft path).
func (t *httpRichTransport) streamDraft(
	ctx context.Context, chatID, draftID int64, msg inputRichMessage,
) error {
	body := map[string]any{"chat_id": chatID, "draft_id": draftID, "rich_message": msg}
	_, err := t.call(ctx, "sendRichMessageDraft", body)
	return err
}

// apiEnvelope is the subset of the Bot API response envelope the shim reads.
type apiEnvelope struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

// call POSTs a JSON body to a Bot API method and returns the raw "result" on
// success, or an error (HTTP, transport, or ok:false) the caller turns into a
// fallback to the legacy rendering.
func (t *httpRichTransport) call(ctx context.Context, method string, body map[string]any) (json.RawMessage, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal %s body: %w", method, err)
	}
	url := fmt.Sprintf("%s/bot%s/%s", t.base, t.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.hc.Do(req)
	if err != nil {
		// net/http embeds the request URL — which carries the bot token — in its
		// error string. Redact it before the error escapes, so a caller that logs the
		// rich fallback can never leak the token.
		return nil, t.redactErr(err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var env apiEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("decode %s envelope: %w", method, err)
	}
	if !env.OK {
		return nil, fmt.Errorf("telegram %s failed: %s", method, env.Description)
	}
	return env.Result, nil
}

// redactErr returns a copy of err with the bot token masked, so a token embedded
// in a transport error (net/http puts the request URL in its message) never
// reaches a log. The concrete error type is dropped, which is fine: rich errors
// are only logged and used to trigger the legacy fallback, never type-inspected.
func (t *httpRichTransport) redactErr(err error) error {
	if t.token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), t.token, "***"))
}
