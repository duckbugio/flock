package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

// defaultHTTPTimeout bounds a hosted transcription request when the caller does
// not inject its own client.
const defaultHTTPTimeout = 60 * time.Second

// httpClient returns the injected client or a fresh one with the default
// timeout, so providers never share an unbounded http.DefaultClient.
func httpClient(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: defaultHTTPTimeout}
}

// httpMultipartTranscriber transcribes via a hosted multipart upload API. The
// mistral and openai providers differ only by endpoint, model and bearer key,
// so they share this implementation.
type httpMultipartTranscriber struct {
	endpoint string
	model    string
	bearer   string
	client   *http.Client
}

// transcriptResponse is the shared JSON shape returned by both hosted providers.
type transcriptResponse struct {
	Text string `json:"text"`
}

// Transcribe uploads audio as a multipart form (fields "model" + "file") with a
// bearer Authorization header and returns the parsed transcript. On a provider
// status >= 400 it returns an error including the status but never the response
// body or the Authorization header (which would leak the API key).
func (t *httpMultipartTranscriber) Transcribe(ctx context.Context, audio io.Reader, filename string) (string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	if err := w.WriteField("model", t.model); err != nil {
		return "", fmt.Errorf("voice: write model field: %w", err)
	}
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return "", fmt.Errorf("voice: create file part: %w", err)
	}
	if _, err := io.Copy(part, audio); err != nil {
		return "", fmt.Errorf("voice: copy audio: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("voice: close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, &body)
	if err != nil {
		return "", fmt.Errorf("voice: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.bearer)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("voice: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		// Drain to allow connection reuse; never surface the body (it may echo
		// request data) or the auth header.
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("voice: transcription failed with status %d", resp.StatusCode)
	}

	var parsed transcriptResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("voice: decode response: %w", err)
	}
	return parsed.Text, nil
}
