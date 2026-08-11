package telegram

// Update represents a Telegram Bot API update.
type Update struct {
	UpdateID      int            `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
	// PreCheckoutQuery must be answered within about ten seconds or
	// Telegram cancels the payment and tells the buyer it failed.
	PreCheckoutQuery *PreCheckoutQuery `json:"pre_checkout_query,omitempty"`
}

// PreCheckoutQuery is Telegram asking whether a confirmed purchase may go
// through.
type PreCheckoutQuery struct {
	ID             string `json:"id"`
	From           *User  `json:"from"`
	Currency       string `json:"currency"`
	TotalAmount    int    `json:"total_amount"`
	InvoicePayload string `json:"invoice_payload"`
}

// SuccessfulPayment arrives as a field on an otherwise empty message.
//
// The subscription fields are populated only for recurring Star
// invoices: SubscriptionExpirationDate is a Unix timestamp, IsRecurring
// marks any charge under a subscription, and IsFirstRecurring marks the
// one that opened it.
type SuccessfulPayment struct {
	Currency                   string `json:"currency"`
	TotalAmount                int    `json:"total_amount"`
	InvoicePayload             string `json:"invoice_payload"`
	TelegramPaymentChargeID    string `json:"telegram_payment_charge_id"`
	ProviderPaymentChargeID    string `json:"provider_payment_charge_id"`
	SubscriptionExpirationDate int64  `json:"subscription_expiration_date"`
	IsRecurring                bool   `json:"is_recurring"`
	IsFirstRecurring           bool   `json:"is_first_recurring"`
}

// CallbackQuery represents a Telegram inline button callback.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    *User    `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data"`
}

// Message represents a Telegram message.
type Message struct {
	MessageID      int         `json:"message_id"`
	From           *User       `json:"from,omitempty"`
	Chat           Chat        `json:"chat"`
	Text           string      `json:"text"`
	Caption        string      `json:"caption,omitempty"`
	Document       *Document   `json:"document,omitempty"`
	Voice          *Voice      `json:"voice,omitempty"`
	Video          *Video      `json:"video,omitempty"`
	VideoNote      *VideoNote  `json:"video_note,omitempty"`
	Photo          []PhotoSize `json:"photo,omitempty"`
	ReplyToMessage *Message    `json:"reply_to_message,omitempty"`
	// SuccessfulPayment comes on a message with no text and no
	// attachment, which is exactly the shape the inbound path drops.
	SuccessfulPayment *SuccessfulPayment `json:"successful_payment,omitempty"`
}

// PhotoSize represents one size variant of a Telegram photo.
type PhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size"`
}

// Document represents a Telegram document (file attachment).
type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

// Voice represents a Telegram voice message.
type Voice struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

// Video represents a Telegram video message.
type Video struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

// VideoNote represents a Telegram round video message.
type VideoNote struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"`
	FileSize int64  `json:"file_size,omitempty"`
}

// User represents a Telegram user.
type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username,omitempty"`
	// LanguageCode is the IETF tag of the sender's Telegram client
	// ("ru", "en-GB"). It is the only deterministic language signal an
	// inbound update carries — a caption may be absent and speech may be a
	// single word that looks like a language it isn't.
	LanguageCode string `json:"language_code,omitempty"`
}

// Chat represents a Telegram chat.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
