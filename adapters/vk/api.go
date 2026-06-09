// Package vk is the VKontakte (VK) community-bot transport adapter for the
// core/chat conversation layer. It is a hand-rolled net/http client against the
// VK API (https://api.vk.com/method/<method>, v=5.199) — deliberately NO VK SDK,
// to keep the dependency tree minimal (net/http only). It implements
// core/chat.Transport (Send/Edit/Delete/SendDocument/StreamDraft/SendStarNudge/
// Capabilities) over messages.send/messages.edit/messages.delete and the
// docs.* upload dance, classifies VK flood/rate errors for the RetryAfter seam,
// renders the inline Stop keyboard, runs the Bots Long Poll receive loop, maps
// VK updates onto the neutral Service, parses the [club<id>|...] community
// mention for the gate, and downloads inbound docs/photos/voice (with URL
// redaction). The transport-neutral run loop lives in core/chat; this package
// holds only VK-specific glue.
package vk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// apiBase is the default VK API method endpoint. It is a struct field on Client
// (not a hardcoded constant in the call path) so unit tests can point it at an
// httptest.Server.
const apiBase = "https://api.vk.com/method/"

// apiVersion is the VK API version every call carries (v=). Pinned per the locked
// tech decision (§3 of docs/multi-transport-plan.md).
const apiVersion = "5.199"

// Client is a tiny VK API client over net/http. BaseURL and HTTPClient are
// injectable (default to the real endpoint / http.DefaultClient) so tests drive
// it against a fake server; Token is the community access token sent as the
// access_token POST form field on every call.
type Client struct {
	// Token is the VK community access token. It is sent as a POST form field, never
	// embedded in a URL, so it does not leak into a logged request line.
	Token string
	// BaseURL is the VK API method endpoint (defaults to apiBase). Tests override it
	// to an httptest.Server URL (with a trailing slash).
	BaseURL string
	// HTTPClient performs the requests (defaults to http.DefaultClient). Tests inject
	// a server-scoped client.
	HTTPClient *http.Client
}

// NewClient builds a Client for token, defaulting BaseURL to the real VK endpoint
// and HTTPClient to http.DefaultClient.
func NewClient(token string) *Client {
	return &Client{Token: token, BaseURL: apiBase, HTTPClient: http.DefaultClient}
}

// base returns the configured BaseURL or the default endpoint.
func (c *Client) base() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return apiBase
}

// httpClient returns the configured HTTPClient or http.DefaultClient.
func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// apiError is a VK API error envelope ({"error":{"error_code":N,...}}). It
// satisfies error so callers can errors.As it (RetryAfter inspects the code).
type apiError struct {
	Code int    `json:"error_code"` //nolint:tagliatelle // VK API uses snake_case.
	Msg  string `json:"error_msg"`  //nolint:tagliatelle // VK API uses snake_case.
}

func (e *apiError) Error() string {
	return fmt.Sprintf("vk api error %d: %s", e.Code, e.Msg)
}

// VK flood/rate-limit error codes. Code 6 ("Too many requests per second") and
// code 9 ("Flood control") are the throttle signals the RetryAfter seam maps to a
// fixed back-off (VK returns no Retry-After header).
const (
	errCodeTooManyRequests = 6
	errCodeFloodControl    = 9
)

// call invokes a VK API method with the given params, decoding the "response"
// field of the envelope into out (which may be nil to discard it). The access
// token and version are added automatically. A VK error envelope is returned as
// an *apiError; a transport/HTTP error has its URL redacted (it carries no token,
// but stays consistent with the upload seam's discipline).
func (c *Client) call(ctx context.Context, method string, params url.Values, out any) error {
	if params == nil {
		params = url.Values{}
	}
	params.Set("access_token", c.Token)
	params.Set("v", apiVersion)

	endpoint := c.base() + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(params.Encode()))
	if err != nil {
		return fmt.Errorf("build %s request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseBytes))
	if err != nil {
		return fmt.Errorf("read %s response: %w", method, err)
	}
	return decodeEnvelope(method, body, out)
}

// maxAPIResponseBytes caps an API response body read so a misbehaving endpoint
// can't exhaust memory. VK method responses are small JSON; Long Poll responses
// (read by the receive loop, which uses its own reader) are bounded separately.
const maxAPIResponseBytes = 8 << 20 // 8 MiB

// envelope is the common VK response shape: exactly one of response/error.
type envelope struct {
	Response json.RawMessage `json:"response"`
	Error    *apiError       `json:"error"`
}

// decodeEnvelope unmarshals a VK response body, surfacing a VK error envelope as
// an *apiError and otherwise decoding the response field into out.
func decodeEnvelope(method string, body []byte, out any) error {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("decode %s response: %w", method, err)
	}
	if env.Error != nil {
		return env.Error
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Response, out); err != nil {
		return fmt.Errorf("decode %s response payload: %w", method, err)
	}
	return nil
}

// MessagesSend posts a message to peerID. randomID MUST be unique per logical
// send within ~1h (VK silently dedupes a repeated random_id); keyboard is the
// JSON inline keyboard (empty = none) and attachment is the optional attachment
// spec (e.g. "doc{owner}_{id}"; empty = none). It returns the GLOBAL message id
// VK assigns, which is what Edit/Delete address (using the global message_id is
// the least error-prone path — VK's messages.edit accepts message_id directly,
// avoiding the conversation_message_id round-trip).
func (c *Client) MessagesSend(
	ctx context.Context, peerID int64, message string, randomID int64, keyboard, attachment string,
) (int64, error) {
	params := url.Values{}
	params.Set("peer_id", strconv.FormatInt(peerID, 10))
	params.Set("message", message)
	params.Set("random_id", strconv.FormatInt(randomID, 10))
	if keyboard != "" {
		params.Set("keyboard", keyboard)
	}
	if attachment != "" {
		params.Set("attachment", attachment)
	}
	// messages.send returns the bare message id as the "response" value.
	var msgID int64
	if err := c.call(ctx, "messages.send", params, &msgID); err != nil {
		return 0, err
	}
	return msgID, nil
}

// MessagesEdit edits messageID (a GLOBAL message id from MessagesSend) in peerID.
// keyboard replaces the inline keyboard (empty clears it). VK allows editing
// within 24h; throttling is handled by core/chat.Service.
func (c *Client) MessagesEdit(ctx context.Context, peerID, messageID int64, message, keyboard string) error {
	params := url.Values{}
	params.Set("peer_id", strconv.FormatInt(peerID, 10))
	params.Set("message_id", strconv.FormatInt(messageID, 10))
	params.Set("message", message)
	// Always set keyboard (empty string clears markup on the final answer).
	params.Set("keyboard", keyboard)
	return c.call(ctx, "messages.edit", params, nil)
}

// MessagesDelete deletes messageID (a GLOBAL message id) in peerID for everyone
// (delete_for_all=1).
func (c *Client) MessagesDelete(ctx context.Context, peerID, messageID int64) error {
	params := url.Values{}
	params.Set("peer_id", strconv.FormatInt(peerID, 10))
	params.Set("message_ids", strconv.FormatInt(messageID, 10))
	params.Set("delete_for_all", "1")
	return c.call(ctx, "messages.delete", params, nil)
}

// SendMessageEventAnswer acknowledges a callback-button press (a message_event
// update) so VK clears the button's loading spinner. It carries no visible
// payload (the Stop action is handled server-side by the Service).
func (c *Client) SendMessageEventAnswer(ctx context.Context, eventID string, userID, peerID int64) error {
	params := url.Values{}
	params.Set("event_id", eventID)
	params.Set("user_id", strconv.FormatInt(userID, 10))
	params.Set("peer_id", strconv.FormatInt(peerID, 10))
	return c.call(ctx, "messages.sendMessageEventAnswer", params, nil)
}

// docUploadServer is the docs.getMessagesUploadServer response.
type docUploadServer struct {
	UploadURL string `json:"upload_url"` //nolint:tagliatelle // VK API uses snake_case.
}

// docSaved is the docs.save response: a typed wrapper around the saved doc.
type docSaved struct {
	Type string `json:"type"`
	Doc  struct {
		ID      int64 `json:"id"`
		OwnerID int64 `json:"owner_id"` //nolint:tagliatelle // VK API uses snake_case.
	} `json:"doc"`
}

// UploadDocument runs VK's three-step document upload for a message attachment:
// docs.getMessagesUploadServer (type=doc, peer_id) → multipart POST the streamed
// file to upload_url as field "file" → docs.save. It returns the attachment spec
// "doc{owner_id}_{id}" to pass to MessagesSend. The file is STREAMED (io.Reader),
// never written to a token-bearing URL.
func (c *Client) UploadDocument(ctx context.Context, peerID int64, name string, data io.Reader) (string, error) {
	server, err := c.docsGetMessagesUploadServer(ctx, peerID)
	if err != nil {
		return "", err
	}
	uploaded, err := c.uploadFile(ctx, server.UploadURL, name, data)
	if err != nil {
		return "", err
	}
	saved, err := c.docsSave(ctx, uploaded)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("doc%d_%d", saved.Doc.OwnerID, saved.Doc.ID), nil
}

// docsGetMessagesUploadServer fetches a one-shot document upload URL for peerID.
func (c *Client) docsGetMessagesUploadServer(ctx context.Context, peerID int64) (docUploadServer, error) {
	params := url.Values{}
	params.Set("type", "doc")
	params.Set("peer_id", strconv.FormatInt(peerID, 10))
	var server docUploadServer
	if err := c.call(ctx, "docs.getMessagesUploadServer", params, &server); err != nil {
		return docUploadServer{}, err
	}
	return server, nil
}

// uploadFileResult is the upload server's response: an opaque "file" token passed
// on to docs.save.
type uploadFileResult struct {
	File  string `json:"file"`
	Error string `json:"error"`
}

// uploadFile POSTs data as multipart field "file" to the (one-shot, non-token)
// upload URL, returning the opaque file token for docs.save. The upload URL is a
// short-lived VK CDN URL with its own key — it carries no community token — but
// any transport error still has its URL redacted for consistency.
func (c *Client) uploadFile(ctx context.Context, uploadURL, name string, data io.Reader) (uploadFileResult, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, err := mw.CreateFormFile("file", name)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, data); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.CloseWithError(mw.Close())
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, pr)
	if err != nil {
		return uploadFileResult{}, fmt.Errorf("build upload request: %w", redactURLError(err))
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return uploadFileResult{}, fmt.Errorf("upload file: %w", redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseBytes))
	if err != nil {
		return uploadFileResult{}, fmt.Errorf("read upload response: %w", err)
	}
	var out uploadFileResult
	if err := json.Unmarshal(body, &out); err != nil {
		return uploadFileResult{}, fmt.Errorf("decode upload response: %w", err)
	}
	if out.File == "" {
		return uploadFileResult{}, fmt.Errorf("upload returned no file token (error=%q)", out.Error)
	}
	return out, nil
}

// docsSave persists the uploaded file token, returning the saved doc descriptor
// used to build the attachment spec.
func (c *Client) docsSave(ctx context.Context, uploaded uploadFileResult) (docSaved, error) {
	params := url.Values{}
	params.Set("file", uploaded.File)
	var saved docSaved
	if err := c.call(ctx, "docs.save", params, &saved); err != nil {
		return docSaved{}, err
	}
	return saved, nil
}

// redactURLError replaces a *url.Error (whose Error() includes the request URL)
// with its underlying cause so a URL never reaches a log or a user-facing error.
// Mirrors adapters/telegram/voice.go's discipline.
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}
