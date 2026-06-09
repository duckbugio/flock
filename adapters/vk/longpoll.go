package vk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// longPollWait is the Bots Long Poll wait window (seconds) — how long the server
// holds an a_check request open before returning an empty update set.
const longPollWait = 25

// pollBackoff is the delay before retrying after a transient HTTP/transport error
// in the receive loop, so a flapping network doesn't spin the loop hot.
const pollBackoff = 3 * time.Second

// maxLongPollBytes caps a single Long Poll response read.
const maxLongPollBytes = 16 << 20 // 16 MiB

// VK Long Poll "failed" codes (returned instead of updates):
//
//	1 → ts is outdated: continue with the returned ts, reuse key.
//	2 → key expired: re-fetch the server (new key), keep ts.
//	3 → information lost: re-fetch the server (new key AND ts).
const (
	failedOutdatedTS = 1
	failedKeyExpired = 2
	failedInfoLost   = 3
)

// longPollServer is the groups.getLongPollServer response.
type longPollServer struct {
	Key    string `json:"key"`
	Server string `json:"server"`
	TS     string `json:"ts"`
}

// longPollResponse is one a_check result: either updates (+ a new ts) or a failed
// code (with, for code 1, a new ts to resume from).
type longPollResponse struct {
	TS      json.Number `json:"ts"`
	Updates []update    `json:"updates"`
	Failed  int         `json:"failed"`
}

// update is one Long Poll update envelope. Only message_new and message_event are
// handled; other types are ignored.
type update struct {
	Type    string          `json:"type"`
	Object  json.RawMessage `json:"object"`
	GroupID int64           `json:"group_id"` //nolint:tagliatelle // VK API uses snake_case.
}

// messageNewObject is the object of a message_new update.
type messageNewObject struct {
	Message messageObject `json:"message"`
}

// messageObject is a VK message. peer_id is the conversation key; from_id is the
// sender (negative = a community, ignored); conversation_message_id is the
// per-conversation id preferred for edits but the run loop keys on it as the
// neutral MessageID for supersede ordering.
type messageObject struct {
	FromID                int64        `json:"from_id"` //nolint:tagliatelle // VK API uses snake_case.
	PeerID                int64        `json:"peer_id"` //nolint:tagliatelle // VK API uses snake_case.
	Text                  string       `json:"text"`
	ID                    int64        `json:"id"`
	ConversationMessageID int64        `json:"conversation_message_id"` //nolint:tagliatelle // VK API uses snake_case.
	Date                  int64        `json:"date"`
	Attachments           []attachment `json:"attachments"`
}

// messageEventObject is the object of a message_event update (a callback button
// press): the pressed run's payload plus the ids needed to ack it.
type messageEventObject struct {
	UserID                int64           `json:"user_id"`  //nolint:tagliatelle // VK API uses snake_case.
	PeerID                int64           `json:"peer_id"`  //nolint:tagliatelle // VK API uses snake_case.
	EventID               string          `json:"event_id"` //nolint:tagliatelle // VK API uses snake_case.
	Payload               json.RawMessage `json:"payload"`
	ConversationMessageID int64           `json:"conversation_message_id"` //nolint:tagliatelle // VK API uses snake_case.
}

// attachment is one message attachment. Only the kinds the download seam handles
// are modeled (doc, photo, audio_message); other types are ignored.
type attachment struct {
	Type         string           `json:"type"`
	Doc          *docAttachment   `json:"doc"`
	Photo        *photoAttachment `json:"photo"`
	AudioMessage *audioMsgAttach  `json:"audio_message"` //nolint:tagliatelle // VK API uses snake_case.
}

type docAttachment struct {
	URL   string `json:"url"`
	Ext   string `json:"ext"`
	Title string `json:"title"`
}

type photoAttachment struct {
	Sizes []photoSize `json:"sizes"`
}

type photoSize struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type audioMsgAttach struct {
	LinkOGG  string `json:"link_ogg"` //nolint:tagliatelle // VK API uses snake_case.
	LinkMP3  string `json:"link_mp3"` //nolint:tagliatelle // VK API uses snake_case.
	Duration int    `json:"duration"`
}

// longPoller fetches the Long Poll server descriptor (groups.getLongPollServer)
// and runs a_check polls. *Client satisfies it; tests use a fake to script the
// poll responses (so the receive loop is driven deterministically without a real
// server).
type longPoller interface {
	getLongPollServer(ctx context.Context, groupID int64) (longPollServer, error)
	poll(ctx context.Context, server longPollServer) (longPollResponse, error)
}

// getLongPollServer fetches a fresh Long Poll server descriptor for groupID.
func (c *Client) getLongPollServer(ctx context.Context, groupID int64) (longPollServer, error) {
	params := url.Values{}
	params.Set("group_id", strconv.FormatInt(groupID, 10))
	var server longPollServer
	if err := c.call(ctx, "groups.getLongPollServer", params, &server); err != nil {
		return longPollServer{}, err
	}
	return server, nil
}

// poll performs one a_check request against the Long Poll server, decoding the
// response. It is the poll the Receiver's Run loop calls via the longPoller
// interface (separated out so the failed-code handling and the unit tests stay
// readable). The client and wait window are the Client's.
func (c *Client) poll(ctx context.Context, server longPollServer) (longPollResponse, error) {
	params := url.Values{}
	params.Set("act", "a_check")
	params.Set("key", server.Key)
	params.Set("ts", server.TS)
	params.Set("wait", strconv.Itoa(longPollWait))
	endpoint := server.Server + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return longPollResponse{}, fmt.Errorf("build poll request: %w", redactURLError(err))
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return longPollResponse{}, fmt.Errorf("poll: %w", redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLongPollBytes))
	if err != nil {
		return longPollResponse{}, fmt.Errorf("read poll response: %w", err)
	}
	var out longPollResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return longPollResponse{}, fmt.Errorf("decode poll response: %w", err)
	}
	return out, nil
}
