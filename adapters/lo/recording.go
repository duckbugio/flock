package lo

import (
	"encoding/json"
	"mime"
	"path/filepath"
	"strings"
)

// recordingFilename gives the speech provider a container extension, never a user path.
func recordingFilename(msg *Message) string {
	if present(msg.Voice) {
		return "voice.ogg"
	}
	var attachment Attachment
	_ = json.Unmarshal(msg.Audio, &attachment)
	mediaType, _, _ := mime.ParseMediaType(attachment.MimeType)
	switch strings.ToLower(mediaType) {
	case "audio/mpeg":
		return "audio.mp3"
	case "audio/mp4", "audio/x-m4a":
		return "audio.m4a"
	case "audio/ogg", "application/ogg":
		return "audio.ogg"
	case "audio/wav", "audio/x-wav":
		return "audio.wav"
	case "audio/flac":
		return "audio.flac"
	case "audio/webm":
		return "audio.webm"
	}
	switch ext := strings.ToLower(filepath.Ext(attachment.FileName)); ext {
	case ".mp3", ".m4a", ".ogg", ".wav", ".flac", ".webm", ".mp4":
		return "audio" + ext
	}
	// LO catalog audio exposes source.mp3 even when its update omits descriptive metadata.
	return "audio.mp3"
}
