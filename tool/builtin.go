package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	bs "github.com/rasimio/blueship/core"
)

// RegisterBuiltinTools registers framework-level tools that any agent
// running on BlueShip can use. Descriptions live inline next to the
// handler — code is the source of truth.
func RegisterBuiltinTools(r *bs.ToolRegistry, d *bs.Deps) {
	tz, err := time.LoadLocation(d.Config.Timezone)
	if err != nil {
		tz = time.UTC
	}

	r.Register("current_time",
		"Returns the current local datetime, weekday, and configured timezone. Use to ground time-sensitive reasoning ('today is X', 'it is now N o'clock') or to compare against stored timestamps.",
		json.RawMessage(`{"type":"object","properties":{}}`),
		func(ctx context.Context, input json.RawMessage) (any, error) {
			now := time.Now().In(tz)
			return map[string]string{
				"datetime": now.Format("2006-01-02 15:04:05"),
				"timezone": tz.String(),
				"weekday":  now.Weekday().String(),
			}, nil
		},
	)

	if d.Config.Search != nil {
		search := d.Config.Search
		r.Register("web_search",
			"Run a web search and return ranked title+URL+snippet results. The handler wraps whatever SearchEngine the ship was configured with (Serper, Google CSE, etc.). Use for factual lookups, current events, or finding canonical URLs to fetch.",
			json.RawMessage(`{"type":"object","properties":{
				"query":{"type":"string","description":"Search query"},
				"limit":{"type":"integer","default":5,"description":"Max results (1-20)"}
			},"required":["query"]}`),
			func(ctx context.Context, input json.RawMessage) (any, error) {
				var p struct {
					Query string `json:"query"`
					Limit int    `json:"limit"`
				}
				if err := json.Unmarshal(input, &p); err != nil {
					return nil, err
				}
				if p.Limit <= 0 {
					p.Limit = 5
				}
				return search.Search(ctx, p.Query, p.Limit)
			},
		)
	}

	if d.Config.Sender != nil {
		// message_send routes through the configured MessageSender. For
		// channel-style targets (no leading digit, '-', or '@') prepend '@'
		// so providers like Telegram resolve them correctly.
		sender := d.Config.Sender
		r.Register("message_send",
			"Send a message to a chat or channel through the agent's configured transport (Telegram, etc.). Use 'to' for a chat id and set is_channel=true for public channel handles.",
			json.RawMessage(`{"type":"object","properties":{
				"to":{"type":"string","description":"Recipient chat ID or channel name"},
				"text":{"type":"string"},
				"is_channel":{"type":"boolean","default":false}
			},"required":["to","text"]}`),
			func(ctx context.Context, input json.RawMessage) (any, error) {
				var p struct {
					To        string `json:"to"`
					Text      string `json:"text"`
					IsChannel bool   `json:"is_channel"`
				}
				if err := json.Unmarshal(input, &p); err != nil {
					return nil, err
				}
				target := p.To
				if p.IsChannel && len(target) > 0 && target[0] != '@' && target[0] != '-' {
					target = "@" + target
				}
				msgID, err := sender.SendMessage(ctx, target, p.Text)
				if err != nil {
					return nil, fmt.Errorf("message_send: %w", err)
				}
				return map[string]any{
					"to":         p.To,
					"text":       p.Text,
					"sent":       true,
					"message_id": msgID,
				}, nil
			},
		)
	}
}
