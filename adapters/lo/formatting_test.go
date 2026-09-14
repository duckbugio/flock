package lo_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/duckbugio/flock/adapters/lo"
)

func TestFormattedSendAndEditPreserveKeyboardLifecycle(t *testing.T) {
	t.Parallel()
	calls := 0
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["parse_mode"] != "HTML" || body["text"] != "😀 <b>готово</b> <code>&lt;x&gt;</code>" {
			t.Errorf("body=%v", body)
		}
		markup, ok := body["reply_markup"].(map[string]any)
		if !ok {
			t.Fatalf("missing markup: %v", body)
		}
		rows, ok := markup["inline_keyboard"].([]any)
		if !ok {
			t.Fatalf("missing keyboard: %v", markup)
		}
		if calls == 1 && len(rows) != 1 || calls == 2 && len(rows) != 0 {
			t.Errorf("keyboard=%v", rows)
		}
		reply(w, `{"ok":true,"result":{"message_id":17}}`)
	})
	transport := lo.NewTransport(api, false).WithKeyboards(true)
	id, err := transport.Send(t.Context(), "77", "😀 **готово** `<x>`", "1", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.Edit(t.Context(), "77", id, "😀 **готово** `<x>`", "", true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestFormattingFallbackIsRestrictedToExplicitRejection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, description string
		code, wantCalls   int
	}{
		{"parser", "cannot parse HTML entities", 400, 2},
		{"empty formatted content", "message text is empty", 400, 2},
		{"entity limit", "too many entities", 400, 2},
		{"unsafe link", "invalid entity URL", 400, 2},
		{"older server", "parse_mode is not supported yet", 400, 2},
		{"chat", "chat not found", 400, 1},
		{"markup", "invalid reply_markup", 400, 1},
		{"rate limit", "cannot parse HTML entities", 429, 1},
		{"ambiguous server failure", "cannot parse HTML entities", 500, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, edit := range []bool{false, true} {
				calls := 0
				var firstMarkup string
				api := client(t, func(w http.ResponseWriter, r *http.Request) {
					calls++
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					markup, err := json.Marshal(body["reply_markup"])
					if err != nil {
						t.Error(err)
					}
					if calls == 1 {
						firstMarkup = string(markup)
						w.WriteHeader(tc.code)
						_ = json.NewEncoder(w).Encode(map[string]any{
							"ok": false, "error_code": tc.code, "description": "Bad Request: " + tc.description,
						})
						return
					}
					if _, ok := body["parse_mode"]; ok {
						t.Error("fallback still has parse_mode")
					}
					if body["text"] != "**answer**" || string(markup) != firstMarkup {
						t.Errorf("fallback changed text or keyboard: %v", body)
					}
					reply(w, `{"ok":true,"result":{"message_id":17}}`)
				})
				transport := lo.NewTransport(api, false).WithKeyboards(true)
				var err error
				if edit {
					err = transport.Edit(t.Context(), "77", "17", "**answer**", "", true)
				} else {
					_, err = transport.Send(t.Context(), "77", "**answer**", "1", true)
				}
				if calls != tc.wantCalls || (err == nil) != (tc.wantCalls == 2) {
					t.Fatalf("edit=%v calls=%d err=%v", edit, calls, err)
				}
			}
		})
	}
}

func TestPlainMessagesAndEscapedBoundary(t *testing.T) {
	t.Parallel()
	for _, markdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "escaped boundary"}[markdown], func(t *testing.T) {
			t.Parallel()
			source := strings.Repeat("<", 4096)
			api := client(t, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				want := source
				if markdown {
					want = strings.Repeat("&lt;", 4096)
				}
				if body["text"] != want {
					t.Error("text differs")
				}
				if _, has := body["parse_mode"]; has != markdown {
					t.Errorf("parse_mode=%v", body["parse_mode"])
				}
				reply(w, `{"ok":true,"result":{"message_id":17}}`)
			})
			if _, err := lo.NewTransport(api, false).Send(t.Context(), "77", source, "", markdown); err != nil {
				t.Fatal(err)
			}
		})
	}
}
