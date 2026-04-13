package tool

import (
	"context"
	"encoding/json"
	"time"

	bs "github.com/rasimio/blueship/core"
)

// RegisterBuiltinTools registers runtime-level tools: current_time, web_search.
// Descriptions are loaded from DB (tool_descriptions table), not hardcoded here.
func RegisterBuiltinTools(r *bs.ToolRegistry, d *bs.Deps) {
	tz, err := time.LoadLocation(d.Config.Timezone)
	if err != nil {
		tz = time.UTC
	}

	r.Register("current_time", "",
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
		r.Register("web_search", "",
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
}
