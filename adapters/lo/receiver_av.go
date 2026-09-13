package lo

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/duckbugio/flock/internal/fsutil"
)

func (r *Receiver) downloadAV(ctx context.Context, msg *Message, chatID string) (saved, notice string) {
	kind, raw := "audio", msg.Audio
	if !present(raw) {
		kind, raw = "video", msg.Video
	}
	var attachment Attachment
	if json.Unmarshal(raw, &attachment) != nil || strings.TrimSpace(attachment.FileID) == "" {
		return "", "I could not read that " + kind + ". Please send it again."
	}
	name := attachment.FileName
	if sanitized := fsutil.SanitizeUploadName(name); sanitized == fsutil.DefaultUploadName ||
		strings.Trim(sanitized, `./\ `) == "" {
		name = strings.Replace(documentFileName(msg.ID, attachment.MimeType), "document_", kind+"_", 1)
	}
	return r.download(ctx, chatID, attachment.FileID, name, attachmentKind{
		noun: kind, advice: "Describe the content or provide a transcript.",
	})
}
