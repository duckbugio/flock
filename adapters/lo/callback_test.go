package lo_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestCallbackUpdatesKeepMalformedItemIsolated(t *testing.T) {
	t.Parallel()
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Allowed []string `json:"allowed_updates"` //nolint:tagliatelle // Bot API wire spelling.
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if !reflect.DeepEqual(body.Allowed, []string{"message", "callback_query"}) {
			t.Errorf("allowed updates: %v", body.Allowed)
		}
		reply(w, `{"ok":true,"result":[
{"update_id":31,"callback_query":{"id":"query-1","from":{"id":77},
"message":{"message_id":9,"from":{"id":1000000000000001,"is_bot":true},
"chat":{"id":77,"type":"private"}},"data":"stop:run"}},
{"update_id":32,"callback_query":{"from":"broken"}},
{"update_id":33,"message":{"message_id":10,"text":"still delivered"}},
{"update_id":34,"callback_query":null}]}`)
	})
	updates, err := api.GetUpdates(t.Context(), 31)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 4 {
		t.Fatalf("updates: %v", updates)
	}
	callback := updates[0].CallbackQuery
	if callback == nil || callback.ID != "query-1" || callback.From.ID != 77 ||
		callback.Message.ID != 9 || callback.Data != "stop:run" {
		t.Fatalf("callback: %+v", callback)
	}
	if updates[1].ID != 32 || updates[1].CallbackQuery != nil {
		t.Fatalf("malformed item: %+v", updates[1])
	}
	if updates[2].Message == nil || updates[2].Message.Text != "still delivered" {
		t.Fatal("following message lost")
	}
	if updates[3].CallbackQuery != nil {
		t.Fatal("null callback became a usable callback")
	}
}

func TestCallbackAnswerRequiresAcknowledgement(t *testing.T) {
	t.Parallel()
	for _, accepted := range []bool{true, false} {
		t.Run(map[bool]string{true: "accepted", false: "rejected"}[accepted], func(t *testing.T) {
			t.Parallel()
			api := client(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/answerCallbackQuery") {
					t.Error(r.URL.Path)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if !reflect.DeepEqual(body, map[string]any{"callback_query_id": "query-1", "text": "Stopping…"}) {
					t.Errorf("body: %v", body)
				}
				if accepted {
					reply(w, `{"ok":true,"result":true}`)
				} else {
					reply(w, `{"ok":true,"result":false}`)
				}
			})
			err := api.AnswerCallbackQuery(t.Context(), "query-1", "Stopping…")
			if (err == nil) != accepted {
				t.Fatalf("accepted=%v err=%v", accepted, err)
			}
		})
	}
}

func TestCallbackAnswerRejectsInvalidInputBeforeHTTP(t *testing.T) {
	t.Parallel()
	api := client(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("invalid callback answer reached HTTP")
		reply(w, `{"ok":true,"result":true}`)
	})
	for _, input := range [][2]string{{"", "text"}, {"query", strings.Repeat("x", 201)}, {"query", string([]byte{0xff})}} {
		if err := api.AnswerCallbackQuery(t.Context(), input[0], input[1]); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
}
