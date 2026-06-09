package telegram

import (
	"fmt"
	"os"
	"strings"

	"github.com/duckbugio/flock/core/claude"
)

// DocumentPrompt builds the prompt text for an uploaded document saved at path,
// including the user's caption when present. A caption-less upload gets a
// sensible default instruction so the run still has a clear task. The text is a
// professional engineering artifact (no duck flavor).
func DocumentPrompt(savedPath, caption string) string {
	caption = strings.TrimSpace(caption)
	if caption == "" {
		return fmt.Sprintf("The user uploaded a file: %s. Please read it and respond.", savedPath)
	}
	return fmt.Sprintf("The user uploaded a file: %s\n\n%s", savedPath, caption)
}

// PhotoPrompt builds the prompt text for an uploaded photo saved at path,
// including the user's caption when present and a sensible default otherwise. The
// saved path is always included so the run has an on-disk reference even when the
// image is also attached as a vision content block.
func PhotoPrompt(savedPath, caption string) string {
	caption = strings.TrimSpace(caption)
	if caption == "" {
		return fmt.Sprintf("The user sent an image (saved at %s).", savedPath)
	}
	return fmt.Sprintf("The user sent an image (saved at %s)\n\n%s", savedPath, caption)
}

// LoadPhotoImage reads the saved photo at savedPath and returns it as a single
// claude.ImageInput for the vision content block, with the media type derived
// from the file extension. The caller attaches the result to the run so Claude
// can see the image; on a read error the caller falls back to a path-only run.
func LoadPhotoImage(savedPath string) ([]claude.ImageInput, error) {
	data, err := os.ReadFile(savedPath) //nolint:gosec // G304: path from a workspace upload we wrote, not raw user input.
	if err != nil {
		return nil, fmt.Errorf("read photo for vision: %w", err)
	}
	return []claude.ImageInput{{
		MediaType: photoMediaType(savedPath),
		Data:      data,
	}}, nil
}

// photoMediaType maps a saved photo's extension to an image MIME type for the
// vision content block. Telegram delivers photos as JPEG, so jpeg is the default
// for an unknown/extension-less name.
func photoMediaType(savedPath string) string {
	lower := strings.ToLower(savedPath)
	switch {
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	default:
		return "image/jpeg"
	}
}
