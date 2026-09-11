package lo_test

import (
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/duckbugio/flock/adapters/lo"
)

// Off by default, and the refusal is explicit: a platform that predates sendDocument answers
// 501 for every artifact, so the flag is the operator's one place to say "mine has it".
func TestSendDocumentIsRefusedUntilEnabled(t *testing.T) {
	t.Parallel()
	var called bool
	api := client(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		reply(w, `{"ok":true,"result":{"message_id":1}}`)
	})

	err := lo.NewTransport(api, false).SendDocument(t.Context(), "77", "report.pdf", strings.NewReader("x"))
	if !errors.Is(err, lo.ErrUnsupported) {
		t.Fatalf("err=%v, want ErrUnsupported", err)
	}
	if called {
		t.Fatal("a disabled document send still reached the platform")
	}
}

// Capabilities is what the core's outbox sweep reads: with documents off the agent's files
// must never be promised to the user.
func TestCapabilitiesFollowTheDocumentFlag(t *testing.T) {
	t.Parallel()
	api := client(t, func(w http.ResponseWriter, _ *http.Request) { reply(w, `{"ok":true,"result":{}}`) })

	if lo.NewTransport(api, false).Capabilities().CanSendDocument {
		t.Error("documents are advertised while disabled")
	}
	if !lo.NewTransport(api, false).WithDocuments(true).Capabilities().CanSendDocument {
		t.Error("documents are enabled but not advertised")
	}
}

func TestSendDocumentUploadsTheFileWithItsBaseName(t *testing.T) {
	t.Parallel()
	var gotName, gotChat, gotBody string
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			t.Errorf("content type = %q, want multipart", r.Header.Get("Content-Type"))
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := reader.NextPart()
			if err != nil {
				break
			}
			data, _ := io.ReadAll(part)
			switch part.FormName() {
			case "chat_id":
				gotChat = string(data)
			case "document":
				gotName, gotBody = part.FileName(), string(data)
			}
		}
		reply(w, `{"ok":true,"result":{"message_id":5}}`)
	})

	transport := lo.NewTransport(api, false).WithDocuments(true)
	// A path is deliberately passed: only its base may travel, because the full one describes
	// this machine's filesystem to everyone in the conversation.
	if err := transport.SendDocument(t.Context(), "77", "/workspace/repo/report.pdf",
		strings.NewReader("%PDF-1.7")); err != nil {
		t.Fatalf("SendDocument: %v", err)
	}

	if gotChat != "77" {
		t.Errorf("chat_id = %q", gotChat)
	}
	if gotName != "report.pdf" {
		t.Errorf("file name = %q, want only the base", gotName)
	}
	if gotBody != "%PDF-1.7" {
		t.Errorf("body = %q", gotBody)
	}
}

// A platform that has not implemented the method answers 501, and the error must carry that
// so an operator reads "upgrade LO" rather than "the file failed to send".
func TestSendDocumentSurfacesTheNotImplementedAnswer(t *testing.T) {
	t.Parallel()
	api := client(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":501,"description":"Method not implemented: sendDocument"}`))
	})

	err := lo.NewTransport(api, false).WithDocuments(true).
		SendDocument(t.Context(), "77", "report.pdf", strings.NewReader("x"))
	if err == nil {
		t.Fatal("a 501 was reported as a successful delivery")
	}
	var apiErr *lo.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != http.StatusNotImplemented {
		t.Fatalf("err=%v, want a 501 APIError", err)
	}
}

// The bot token is part of every upload URL. It must never reach an error a log will print.
func TestSendDocumentErrorsCarryNoToken(t *testing.T) {
	t.Parallel()
	api, err := lo.NewClient("https://127.0.0.1:1", "secret-token-value", nil)
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	sendErr := lo.NewTransport(api, false).WithDocuments(true).
		SendDocument(t.Context(), "77", "a.bin", strings.NewReader("x"))
	if sendErr == nil {
		t.Fatal("a dead endpoint reported success")
	}
	if strings.Contains(sendErr.Error(), "secret-token-value") {
		t.Fatalf("the token leaked into an error: %v", sendErr)
	}
}

// The upload path shares the response envelope with every other method, so a 429 that names a
// delay must survive it. It did not before: the upload built its own APIError with a zero
// Delay, and RetryAfter then told the delivery loop to retry in one second a send the platform
// had asked it to hold for a minute.
func TestUploadRetryDelaySurvivesTheEnvelope(t *testing.T) {
	t.Parallel()
	api := client(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":60}}`))
	})

	err := lo.NewTransport(api, false).WithDocuments(true).
		SendDocument(t.Context(), "77", "report.pdf", strings.NewReader("x"))
	delay, ok := lo.RetryAfter(err)
	if !ok {
		t.Fatalf("err=%v, want a classified 429", err)
	}
	if delay != 60*time.Second {
		t.Fatalf("delay=%v, want the 60s the platform asked for", delay)
	}
}

// An oversize response is refused rather than truncated. A cut-off body parses as garbage and
// used to surface as "non-JSON upload response" — a wrong diagnosis of a size problem.
func TestUploadRefusesAnOversizeResponse(t *testing.T) {
	t.Parallel()
	api := client(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"padding":"` + strings.Repeat("x", 2<<20) + `"}`))
	})

	err := lo.NewTransport(api, false).WithDocuments(true).
		SendDocument(t.Context(), "77", "report.pdf", strings.NewReader("x"))
	if err == nil || !strings.Contains(err.Error(), "exceeds size limit") {
		t.Fatalf("err=%v, want the size limit to be named", err)
	}
}

// The document is streamed, not copied into memory first, and the request still declares its
// length so the platform receives an ordinary identity-encoded upload rather than a chunked
// one. Both halves are asserted here because either alone is the wrong trade: a buffered body
// has a known length too, and a streamed body without a length changes the wire shape.
func TestUploadStreamsTheFileWithAKnownLength(t *testing.T) {
	t.Parallel()
	var gotLength int64
	var gotEncoding []string
	var gotBody string
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		gotLength, gotEncoding = r.ContentLength, r.TransferEncoding
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("content type: %v", err)
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := reader.NextPart()
			if err != nil {
				break
			}
			if part.FormName() == "document" {
				data, _ := io.ReadAll(part)
				gotBody = string(data)
			}
		}
		reply(w, `{"ok":true,"result":{"message_id":1}}`)
	})

	file, err := os.CreateTemp(t.TempDir(), "artifact-*.txt")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	const content = "an artifact the agent produced"
	if _, err := file.WriteString(content); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })

	if err := lo.NewTransport(api, false).WithDocuments(true).
		SendDocument(t.Context(), "77", "artifact.txt", file); err != nil {
		t.Fatalf("SendDocument: %v", err)
	}
	if gotBody != content {
		t.Fatalf("body=%q, want the file's contents", gotBody)
	}
	if len(gotEncoding) != 0 {
		t.Fatalf("transfer-encoding=%v, want an identity-encoded request", gotEncoding)
	}
	if gotLength <= int64(len(content)) {
		t.Fatalf("content-length=%d, want the declared length of the whole multipart body", gotLength)
	}
}
