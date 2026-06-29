package openaicompat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/duckbugio/flock/core/agent"
)

func collect(t *testing.T, ch <-chan agent.Event) []agent.Event {
	t.Helper()
	var events []agent.Event
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, ev)
		case <-deadline:
			t.Fatalf("runner channel did not close; got %d events", len(events))
		}
	}
}

func TestRunPostsChatCompletionAndMapsResult(t *testing.T) {
	var gotAuth string
	var gotReq chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"content":"Qwen-compatible answer"}}],
			"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}
		}`))
	}))
	defer server.Close()

	r := New(Config{
		BaseURL: server.URL + "/v1/",
		Model:   "qwen-plus",
		APIKey:  "secret-key",
		Client:  server.Client(),
	})
	ch, err := r.Run(context.Background(), "hello qwen", agent.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	events := collect(t, ch)

	if gotAuth != "Bearer secret-key" {
		t.Fatalf("Authorization = %q, want bearer token", gotAuth)
	}
	if gotReq.Model != "qwen-plus" {
		t.Fatalf("request model = %q, want qwen-plus", gotReq.Model)
	}
	if len(gotReq.Messages) != 1 || gotReq.Messages[0].Role != "user" || gotReq.Messages[0].Content != "hello qwen" {
		t.Fatalf("request messages = %+v, want one user prompt", gotReq.Messages)
	}
	wantTypes := []agent.EventType{agent.Text, agent.Result}
	if len(events) != len(wantTypes) {
		t.Fatalf("events = %+v, want %v", events, wantTypes)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Fatalf("event %d type = %v, want %v", i, events[i].Type, want)
		}
	}
	if events[0].Text != "Qwen-compatible answer" {
		t.Fatalf("Text = %q, want answer", events[0].Text)
	}
	if events[1].Result == nil || events[1].Result.Text != "Qwen-compatible answer" {
		t.Fatalf("Result = %+v, want final answer", events[1].Result)
	}
	if events[1].Result.NumTurns != 1 {
		t.Fatalf("Result.NumTurns = %d, want 1", events[1].Result.NumTurns)
	}
}

func TestRunUsesOptionsModelOverride(t *testing.T) {
	var gotReq chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()

	r := New(Config{BaseURL: server.URL, Model: "default-model", Client: server.Client()})
	ch, err := r.Run(context.Background(), "prompt", agent.Options{Model: "override-model"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = collect(t, ch)
	if gotReq.Model != "override-model" {
		t.Fatalf("request model = %q, want override-model", gotReq.Model)
	}
}

func TestRunErrorOnHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer server.Close()

	r := New(Config{BaseURL: server.URL, Model: "qwen-plus", APIKey: "bad", Client: server.Client()})
	ch, err := r.Run(context.Background(), "hello", agent.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var runErr error
	for _, ev := range collect(t, ch) {
		if ev.Type == agent.RunError {
			runErr = ev.Err
		}
	}
	if runErr == nil {
		t.Fatal("expected RunError")
	}
}
