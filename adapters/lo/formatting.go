package lo

import (
	"context"
	"errors"
	"net/http"

	"github.com/duckbugio/flock/core/textformat"
)

// callFormatted retries only explicit formatting rejections, which LO returns
// before storing a message. Transport errors and ambiguous server failures must
// never trigger a second send. The retry preserves the original text and markup.
func (t *Transport) callFormatted(
	ctx context.Context, method string, body map[string]any, text string, markdown bool, result *Message,
) error {
	if markdown {
		body["text"] = textformat.MarkdownToHTML(text)
		body["parse_mode"] = "HTML"
	}
	err := t.api.call(ctx, method, body, result)
	if !markdown || !formattingRejected(err) {
		return err
	}
	body["text"] = text
	delete(body, "parse_mode")
	return t.api.call(ctx, method, body, result)
}

// formattingRejected matches the server's pre-write parser/validator errors.
// Other 400 responses may concern chat access or keyboard validation instead.
func formattingRejected(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != http.StatusBadRequest {
		return false
	}
	switch apiErr.Description {
	case "Bad Request: cannot parse HTML entities",
		"Bad Request: message text is empty",
		"Bad Request: too many entities",
		"Bad Request: invalid entity URL",
		"Bad Request: code and pre entities cannot overlap other entities",
		"Bad Request: duplicate entity range",
		"Bad Request: crossing entity ranges",
		"Bad Request: unsupported parse_mode",
		"Bad Request: parse_mode is not supported yet":
		return true
	default:
		return false
	}
}
