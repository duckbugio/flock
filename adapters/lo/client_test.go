package lo_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/duckbugio/flock/adapters/lo"
)

const testToken = "123:secret-token"

func client(t *testing.T, handler http.HandlerFunc) *lo.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	api, err := lo.NewClient(server.URL, testToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return api
}

func reply(w http.ResponseWriter, value string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(value))
}

func TestTransportUsesOnlySupportedLOParameters(t *testing.T) {
	t.Parallel()
	var methods []string
	var mu sync.Mutex
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Error("expected POST")
		}
		method := strings.TrimPrefix(r.URL.Path, "/bot"+testToken+"/")
		mu.Lock()
		methods = append(methods, method)
		mu.Unlock()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		allowed := map[string]bool{"chat_id": true, "text": true}
		if method != "sendMessage" {
			allowed["message_id"] = true
		}
		for key := range body {
			if !allowed[key] {
				t.Errorf("unsupported field %s", key)
			}
		}
		if method == "deleteMessage" {
			reply(w, `{"ok":true,"result":true}`)
			return
		}
		reply(w, `{"ok":true,"result":{"message_id":17}}`)
	})
	transport := lo.NewTransport(api, false)
	id, err := transport.Send(t.Context(), "77", "**answer**", "run", true)
	if err != nil || id != "17" {
		t.Fatalf("id=%s err=%v", id, err)
	}
	if err := transport.Edit(t.Context(), "77", id, "updated", "run", true); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.SendReply(t.Context(), "77", id, "done", true); err != nil {
		t.Fatal(err)
	}
	if err := transport.Delete(t.Context(), "77", id); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(methods, ",") != "sendMessage,editMessageText,sendMessage,deleteMessage" {
		t.Fatal(methods)
	}
	caps := transport.Capabilities()
	if caps.CanSendDocument || caps.CanSendRich || caps.CanSendDraft || caps.MaxMessageRunes != 2048 {
		t.Fatal(caps)
	}
}

func TestRetryAfterAndCredentialRedaction(t *testing.T) {
	t.Parallel()
	api := client(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		reply(w, `{"ok":false,"error_code":429,"description":"`+testToken+`","parameters":{"retry_after":3}}`)
	})
	_, err := api.GetMe(t.Context())
	delay, ok := lo.RetryAfter(err)
	if !ok || delay != 3*time.Second || strings.Contains(err.Error(), testToken) {
		t.Fatalf("delay=%v err=%v", delay, err)
	}
}

func TestClientRefusesRedirects(t *testing.T) {
	t.Parallel()
	var reached atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { reached.Store(true) }))
	defer destination.Close()
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	})
	_, err := api.GetMe(t.Context())
	if err == nil || reached.Load() {
		t.Fatalf("err=%v reached=%v", err, reached.Load())
	}
}

func TestMalformedResponsesNeverLookLikeSuccess(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"ok":true}`, `{"ok":true,"result":null}`, `{"ok":true,"result":{}}`, `not json`,
		`{"ok":false,"error_code":501}`,
	} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			api := client(t, func(w http.ResponseWriter, _ *http.Request) { reply(w, body) })
			if _, err := api.GetMe(t.Context()); err == nil {
				t.Fatal("accepted invalid result")
			}
		})
	}
}

func TestDraftIDAndUTF16Budget(t *testing.T) {
	t.Parallel()
	var ids []json.Number
	var mu sync.Mutex
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		var id json.Number
		if err := json.Unmarshal(body["draft_id"], &id); err != nil {
			t.Error(err)
		}
		mu.Lock()
		ids = append(ids, id)
		mu.Unlock()
		reply(w, `{"ok":true,"result":true}`)
	})
	transport := lo.NewTransport(api, true)
	for _, text := range []string{"first", "second"} {
		if err := transport.SendDraft(t.Context(), "77", "same-run", text); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 2 || ids[0] != ids[1] || ids[0] == "0" {
		t.Fatal(ids)
	}
	if _, err := transport.Send(t.Context(), "77", strings.Repeat("😀", 2049), "", false); err == nil {
		t.Fatal("accepted oversized UTF-16 text")
	}
	if err := transport.SendDraft(t.Context(), "-77", "same-run", "text"); !errors.Is(err, lo.ErrUnsupported) {
		t.Fatal(err)
	}
}

func TestGetUpdatesPreservesInt64AndOffset(t *testing.T) {
	t.Parallel()
	const offset = int64(9007199254740993)
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Offset int64 `json:"offset"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Offset != offset {
			t.Errorf("offset=%d", body.Offset)
		}
		reply(w, fmt.Sprintf(`{"ok":true,"result":[{"update_id":%d,"message":{
"message_id":17,"from":{"id":77},"chat":{"id":77,"type":"private"},"text":"hello"}}]}`, offset))
	})
	updates, err := api.GetUpdates(t.Context(), offset)
	if err != nil || len(updates) != 1 || updates[0].ID != offset {
		t.Fatalf("updates=%+v err=%v", updates, err)
	}
}

func TestClientValidationAndCancellation(t *testing.T) {
	t.Parallel()
	for _, base := range []string{
		"https://user:pass@example.com", "http://example.com", "https://example.com?token=secret", "not-url",
	} {
		if _, err := lo.NewClient(base, testToken, nil); err == nil {
			t.Fatal(base)
		}
	}
	api := client(t, func(w http.ResponseWriter, _ *http.Request) { reply(w, `{"ok":true,"result":[]}`) })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := api.GetUpdates(ctx, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestActiveWebhookIsNeverDeleted(t *testing.T) {
	t.Parallel()
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/getWebhookInfo") {
			t.Errorf("unexpected mutation: %s", r.URL.Path)
		}
		reply(w, `{"ok":true,"result":{"url":"https://example.com/hook"}}`)
	})
	if err := api.CheckPolling(t.Context()); err == nil {
		t.Fatal("accepted active webhook")
	}
}

func TestClearDraftUsesSameIDAndEmptyText(t *testing.T) {
	t.Parallel()
	type payload struct {
		ID   json.Number `json:"draft_id"` //nolint:tagliatelle // Bot API wire spelling.
		Text string      `json:"text"`
	}
	var mu sync.Mutex
	var requests []payload
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		var body payload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		requests = append(requests, body)
		mu.Unlock()
		reply(w, `{"ok":true,"result":true}`)
	})
	transport := lo.NewTransport(api, true)
	if err := transport.SendDraft(t.Context(), "77", "run", "working"); err != nil {
		t.Fatal(err)
	}
	if err := transport.ClearDraft(t.Context(), "77", "run"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 || requests[0].ID != requests[1].ID || requests[1].Text != "" {
		t.Fatalf("requests=%+v", requests)
	}
}

func TestPollingBatchFitsFullSizeUnicodeReplies(t *testing.T) {
	t.Parallel()
	const batchSize = 25
	text := strings.Repeat("界", 4096)
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Limit int `json:"limit"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Limit != batchSize {
			t.Errorf("limit=%d", body.Limit)
		}
		updates := make([]lo.Update, batchSize)
		for i := range updates {
			message := inbound(text)
			message.Reply = inbound(text)
			updates[i] = lo.Update{ID: int64(i + 1), Message: message}
		}
		raw, err := json.Marshal(map[string]any{"ok": true, "result": updates})
		if err != nil {
			t.Error(err)
		}
		reply(w, string(raw))
	})
	updates, err := api.GetUpdates(t.Context(), 0)
	if err != nil || len(updates) != batchSize {
		t.Fatalf("count=%d err=%v", len(updates), err)
	}
}

func TestWebhookPreflightOnlyFallsBackForUnsupportedMethods(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		code      int
		wantError bool
	}{
		{http.StatusNotFound, false},
		{http.StatusNotImplemented, false},
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, true},
		{http.StatusInternalServerError, true},
		{http.StatusTooManyRequests, true},
	} {
		t.Run(strconv.Itoa(test.code), func(t *testing.T) {
			t.Parallel()
			api := client(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.code)
				reply(w, fmt.Sprintf(`{"ok":false,"error_code":%d,"description":"fixture"}`, test.code))
			})
			if err := api.CheckPolling(t.Context()); (err != nil) != test.wantError {
				t.Fatalf("error=%v wantError=%v", err, test.wantError)
			}
		})
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused for " + testToken)
}

func TestNetworkErrorKeepsReasonWithoutCredential(t *testing.T) {
	t.Parallel()
	api, err := lo.NewClient("https://lo.example", testToken, &http.Client{Transport: failingTransport{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = api.GetMe(t.Context())
	if err == nil || !strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), testToken) {
		t.Fatalf("unsafe or unhelpful error: %v", err)
	}
}
