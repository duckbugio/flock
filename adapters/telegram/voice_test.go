//nolint:testpackage // intentionally whitebox to test unexported telegram voice handling internals
package telegram

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeFileSource serves DownloadURL from a test server and records the file id.
type fakeFileSource struct {
	filePath    string
	downloadURL string
}

func (f *fakeFileSource) FileInfo(_ context.Context, _ string) (string, error) {
	return f.filePath, nil
}

func (f *fakeFileSource) DownloadURL(_ string) string {
	return f.downloadURL
}

// recordingTranscriber records the bytes/filename it received and returns a
// fixed transcript.
type recordingTranscriber struct {
	gotBytes    []byte
	gotFilename string
}

func (r *recordingTranscriber) Transcribe(_ context.Context, audio io.Reader, filename string) (string, error) {
	b, err := io.ReadAll(audio)
	if err != nil {
		return "", err
	}
	r.gotBytes = b
	r.gotFilename = filename
	return "transcribed text", nil
}

func TestVoiceTranscriberRoundTrip(t *testing.T) {
	const audio = "OGGOPUSBYTES"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, audio)
	}))
	defer srv.Close()

	src := &fakeFileSource{filePath: "voice/file_42.oga", downloadURL: srv.URL}
	rec := &recordingTranscriber{}
	vt := NewVoiceTranscriber(src, srv.Client(), rec, 0, nil)

	got, err := vt.Transcribe(context.Background(), "file-id")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if got != "transcribed text" {
		t.Errorf("transcript = %q, want %q", got, "transcribed text")
	}
	if string(rec.gotBytes) != audio {
		t.Errorf("audio bytes = %q, want %q", rec.gotBytes, audio)
	}
	if rec.gotFilename != "file_42.oga" {
		t.Errorf("filename = %q, want %q (path.Base of file path)", rec.gotFilename, "file_42.oga")
	}
}

func TestVoiceTranscriberDownloadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	src := &fakeFileSource{filePath: "voice/file.oga", downloadURL: srv.URL}
	vt := NewVoiceTranscriber(src, srv.Client(), &recordingTranscriber{}, 0, nil)

	if _, err := vt.Transcribe(context.Background(), "file-id"); err == nil {
		t.Fatal("Transcribe on 404 = nil error, want error")
	}
}

func TestVoiceTranscriberTransportErrorRedactsToken(t *testing.T) {
	// A transport failure (connection refused) yields a *url.Error whose message
	// embeds the request URL — which carries the bot token. The returned error
	// must NOT leak it, so it can be safely logged.
	const token = "123456:SECRET-BOT-TOKEN"
	// A closed server's address gives a deterministic connection-refused error.
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := closed.URL
	closed.Close()
	downloadURL := addr + "/file/bot" + token + "/voice/file.oga"

	src := &fakeFileSource{filePath: "voice/file.oga", downloadURL: downloadURL}
	vt := NewVoiceTranscriber(src, http.DefaultClient, &recordingTranscriber{}, 0, nil)

	_, err := vt.Transcribe(context.Background(), "file-id")
	if err == nil {
		t.Fatal("Transcribe on transport error = nil, want error")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("download error leaks the bot token: %q", err.Error())
	}
}

func TestVoiceTranscriberLimitReader(t *testing.T) {
	// Server sends more than the cap; the transcriber must see at most maxBytes.
	const maxBytes = 8
	big := strings.Repeat("A", 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, big)
	}))
	defer srv.Close()

	src := &fakeFileSource{filePath: "voice/file.oga", downloadURL: srv.URL}
	rec := &recordingTranscriber{}
	vt := NewVoiceTranscriber(src, srv.Client(), rec, maxBytes, nil)

	if _, err := vt.Transcribe(context.Background(), "file-id"); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if len(rec.gotBytes) > maxBytes {
		t.Errorf("transcriber saw %d bytes, want at most %d", len(rec.gotBytes), maxBytes)
	}
}
