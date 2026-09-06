package blueship

import "github.com/rasimio/blueship/internal/transport/telegram"

// Telegram's transport can also be embedded independently of Ship's database
// lifecycle, for local runtimes and recovery tools. The implementation is the
// same client and poller used by the standard gateway.
type (
	TelegramClient               = telegram.Client
	TelegramPoller               = telegram.Poller
	TelegramUpdate               = telegram.Update
	TelegramMessage              = telegram.Message
	TelegramInlineKeyboardButton = telegram.InlineKeyboardButton
	TelegramAPIError             = telegram.APIError
)

var (
	NewTelegramClient           = telegram.NewClient
	NewTelegramClientWithAPIURL = telegram.NewClientWithAPIURL
	NewTelegramPoller           = telegram.NewPoller
	NewTelegramPollerWithAPIURL = telegram.NewPollerWithAPIURL
)
