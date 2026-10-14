package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

// SendDirect delivers a host-composed message to one chat through one of
// the gateway's bots, with exactly one Telegram request. It goes through
// the bot's own client, so it shares that bot's send allowance with every
// reply and reminder the bot sends — Bulk messages at a lower priority.
//
// Nothing about the conversation is touched: no turn starts, nothing is
// written to history, no turn lock is taken. The failure, when there is
// one, is a *bs.DirectSendError saying whether the message can exist.
func (g *Gateway) SendDirect(ctx context.Context, msg bs.DirectMessage) (bs.DirectReceipt, error) {
	bi := g.botByID(msg.BotID)
	if bi == nil || bi.client == nil || !bi.client.IsConfigured() {
		return bs.DirectReceipt{}, &bs.DirectSendError{Outcome: bs.DirectNotSent, Description: "bot is not running"}
	}
	rows, err := g.directRows(msg.Buttons)
	if err != nil {
		return bs.DirectReceipt{}, &bs.DirectSendError{Outcome: bs.DirectRejected, Description: err.Error()}
	}
	pm := telegram.PreparedMessage{ChatID: msg.ChatID, HTML: msg.HTML, Rows: rows, Silent: msg.Silent}
	if p := msg.Photo; p != nil {
		if p.FileID != "" {
			pm.PhotoFileID = p.FileID
		} else {
			pm.Photo = &telegram.PhotoUpload{Name: p.Name, MIME: p.MIME, Data: p.Data}
		}
	}
	if msg.Bulk {
		ctx = telegram.WithBulk(ctx)
	}
	res, err := bi.client.SendPrepared(ctx, pm)
	if err != nil {
		return bs.DirectReceipt{}, classifyDirectFailure(err)
	}
	return bs.DirectReceipt{MessageID: res.MessageID, PhotoFileID: res.PhotoFileID, DeliveredAt: time.Now()}, nil
}

// directRows builds the keyboard. A button that cannot be built fails the
// whole message rather than going out without it: the host composed the
// keyboard deliberately, and an unsubscribe button silently missing is
// worse than a message not sent.
func (g *Gateway) directRows(buttons [][]bs.DirectButton) ([][]telegram.InlineKeyboardButton, error) {
	var rows [][]telegram.InlineKeyboardButton
	for _, row := range buttons {
		var keys []telegram.InlineKeyboardButton
		for _, b := range row {
			if b.Text == "" {
				return nil, errors.New("button without a label")
			}
			switch {
			case b.Command != "" && b.URL != "":
				return nil, errors.New("button with both a link and a command")
			case b.Command != "":
				data := hostCallbackPrefix + b.Command + ":" + b.Args
				if len(data) > 64 {
					return nil, fmt.Errorf("button %q: callback data is %d bytes, over 64", b.Command, len(data))
				}
				if !g.isHostCallbackCommand(b.Command) {
					return nil, fmt.Errorf("button %q: not a host command", b.Command)
				}
				keys = append(keys, telegram.InlineKeyboardButton{Text: b.Text, CallbackData: data})
			case b.URL != "":
				keys = append(keys, telegram.InlineKeyboardButton{Text: b.Text, URL: b.URL})
			default:
				return nil, errors.New("button with neither a link nor a command")
			}
		}
		if len(keys) > 0 {
			rows = append(rows, keys)
		}
	}
	return rows, nil
}

func classifyDirectFailure(err error) *bs.DirectSendError {
	var apiErr *telegram.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode
		if code == 0 {
			code = apiErr.StatusCode
		}
		out := &bs.DirectSendError{Code: code, Description: apiErr.Description}
		switch {
		case code == http.StatusTooManyRequests:
			out.Outcome = bs.DirectRateLimited
			out.RetryAfter = time.Duration(apiErr.Parameters.RetryAfter) * time.Second
		case code == http.StatusRequestTimeout:
			// An intermediary may have timed out after Telegram took it.
			out.Outcome = bs.DirectUnknown
		case code >= 400 && code < 500:
			out.Outcome = bs.DirectRejected
		default:
			// A 5xx is an answer about the server, not about this message:
			// it may have been delivered before the failure.
			out.Outcome = bs.DirectUnknown
		}
		return out
	}
	var transportErr *telegram.TransportError
	if errors.As(err, &transportErr) {
		if transportErr.Written {
			return &bs.DirectSendError{Outcome: bs.DirectUnknown, Description: transportErr.Error()}
		}
		return &bs.DirectSendError{Outcome: bs.DirectNotSent, Description: transportErr.Error()}
	}
	// Composition failures (encoding the form) happen before any request.
	return &bs.DirectSendError{Outcome: bs.DirectNotSent, Description: err.Error()}
}
