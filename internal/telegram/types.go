package telegram

// Update represents a Telegram Bot API update.
type Update struct {
	UpdateID      int            `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
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
	MessageID      int          `json:"message_id"`
	From           *User        `json:"from,omitempty"`
	Chat           Chat         `json:"chat"`
	Text           string       `json:"text"`
	Caption        string       `json:"caption,omitempty"`
	Document       *Document    `json:"document,omitempty"`
	Voice          *Voice       `json:"voice,omitempty"`
	Photo          []PhotoSize  `json:"photo,omitempty"`
	ReplyToMessage *Message     `json:"reply_to_message,omitempty"`
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

// User represents a Telegram user.
type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username,omitempty"`
}

// Chat represents a Telegram chat.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
