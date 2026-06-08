package voice

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostedCase drives the mistral/openai shared multipart path against a fake
// server that asserts the request shape and replies with a canned transcript.
func TestHostedProviders(t *testing.T) {
	tests := []struct {
		name      string
		provider  string
		key       string
		wantPath  string
		wantModel string
		makeCfg   func(srv string, key string) Config
	}{
		{
			name:      "mistral default model",
			provider:  "mistral",
			key:       "mk-secret",
			wantPath:  "/v1/audio/transcriptions",
			wantModel: defaultMistralModel,
			makeCfg: func(srv, key string) Config {
				return Config{Provider: "Mistral", MistralAPIKey: key}
			},
		},
		{
			name:      "openai default model",
			provider:  "openai",
			key:       "ok-secret",
			wantPath:  "/v1/audio/transcriptions",
			wantModel: defaultOpenAIModel,
			makeCfg: func(srv, key string) Config {
				return Config{Provider: "OPENAI", OpenAIAPIKey: key}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				gotPath  string
				gotAuth  string
				gotModel string
				gotFile  string
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Errorf("ParseMultipartForm: %v", err)
				}
				gotModel = r.FormValue("model")
				if f, _, err := r.FormFile("file"); err == nil {
					b, _ := io.ReadAll(f)
					gotFile = string(b)
					_ = f.Close()
				} else {
					t.Errorf("missing file part: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"text":"hello world"}`))
			}))
			defer srv.Close()

			cfg := tt.makeCfg(srv.URL, tt.key)
			tr, err := New(cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			// Point the transcriber at the fake server.
			hm := tr.(*httpMultipartTranscriber)
			hm.endpoint = srv.URL + tt.wantPath

			got, err := tr.Transcribe(context.Background(), strings.NewReader("AUDIODATA"), "voice.ogg")
			if err != nil {
				t.Fatalf("Transcribe: %v", err)
			}
			if got != "hello world" {
				t.Errorf("transcript = %q, want %q", got, "hello world")
			}
			if gotPath != tt.wantPath {
				t.Errorf("path = %q, want %q", gotPath, tt.wantPath)
			}
			if gotAuth != "Bearer "+tt.key {
				t.Errorf("auth = %q, want %q", gotAuth, "Bearer "+tt.key)
			}
			if gotModel != tt.wantModel {
				t.Errorf("model = %q, want %q", gotModel, tt.wantModel)
			}
			if gotFile != "AUDIODATA" {
				t.Errorf("file = %q, want %q", gotFile, "AUDIODATA")
			}
		})
	}
}

func TestNewErrors(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"unknown provider", Config{Provider: "bogus"}},
		{"empty provider", Config{Provider: ""}},
		{"mistral missing key", Config{Provider: "mistral"}},
		{"openai missing key", Config{Provider: "openai"}},
		{"local missing command", Config{Provider: "local"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.cfg); err == nil {
				t.Fatalf("New(%+v) = nil error, want error", tt.cfg)
			}
		})
	}
}

func TestHostedProviderServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	tr, err := New(Config{Provider: "mistral", MistralAPIKey: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tr.(*httpMultipartTranscriber).endpoint = srv.URL

	_, err = tr.Transcribe(context.Background(), strings.NewReader("x"), "voice.ogg")
	if err == nil {
		t.Fatal("Transcribe on 500 = nil error, want error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %q, want it to mention status 500", err.Error())
	}
}

func TestLocalProvider(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "transcribe.sh")
	// Ignores its input file and prints a fixed transcript.
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'local transcript\\n'\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	tr, err := New(Config{Provider: "local", LocalCommand: script})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := tr.Transcribe(context.Background(), strings.NewReader("AUDIO"), "voice.ogg")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if got != "local transcript" {
		t.Errorf("transcript = %q, want %q", got, "local transcript")
	}
}
