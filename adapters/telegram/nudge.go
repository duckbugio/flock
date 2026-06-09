package telegram

import (
	"strings"

	"github.com/go-telegram/bot/models"
)

// starCallbackPrefix is the inline-button callback_data prefix for the star
// nudge's confirm button. It mirrors stopCallbackPrefix: a press routes to the
// Service via the bot's callback handler, which calls StarPress.
const starCallbackPrefix = "star:"

// starCallbackData is the full callback_data carried by the star button. The
// nudge is a single global account action (no per-run id), so a fixed token is
// enough — unlike the Stop button which embeds a run id.
const starCallbackData = starCallbackPrefix + "confirm"

// starButtonText labels the confirm button. Pressing it triggers the server-side
// star (a callback button, not a URL button).
const starButtonText = "⭐ Поставить звезду"

// starMarkup returns the inline keyboard with the single confirm button used on a
// nudge message.
func starMarkup() models.ReplyMarkup {
	return &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{{
			{Text: starButtonText, CallbackData: starCallbackData},
		}},
	}
}

// IsStarCallback reports whether callback data is a star-confirm callback the
// Service should handle. Used as the prefix for the bot's callback handler, the
// same way CallbackMatch drives the Stop handler.
func IsStarCallback(data string) bool {
	return strings.HasPrefix(data, starCallbackPrefix)
}

// StarCallbackPrefix is the callback_data prefix the cmd registers the star
// handler against (bot.MatchTypePrefix), mirroring CallbackMatch for Stop.
func StarCallbackPrefix() string { return starCallbackPrefix }
