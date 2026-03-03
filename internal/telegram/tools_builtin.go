package telegram

import (
	"context"
	"encoding/json"
	"time"

	bs "github.com/rasimio/blueship/core"
	"github.com/rasimio/blueship/internal/web"
)

// registerBuiltinTools registers runtime-level tools: current_time, web_search, web_fetch.
func registerBuiltinTools(r *bs.ToolRegistry, d *bs.Deps) {
	tz, err := time.LoadLocation(d.Config.Timezone)
	if err != nil {
		tz = time.UTC
	}

	registerCurrentTimeTool(r, tz)
	registerWebTools(r, d)
}

func registerCurrentTimeTool(r *bs.ToolRegistry, tz *time.Location) {
	r.Register("current_time",
		"Returns current date, time, day of week and timezone.",
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
}

func registerWebTools(r *bs.ToolRegistry, d *bs.Deps) {
	if d.Config.Search != nil {
		search := d.Config.Search
		r.Register("web_search",
			"Search the web using Google. Returns titles, URLs, and descriptions.",
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

	// web_fetch: always available (use configured fetcher or default)
	var fetcher bs.WebFetcher
	if d.Config.Fetcher != nil {
		fetcher = d.Config.Fetcher
	} else {
		fetcher = web.NewHTTPFetcher()
	}
	r.Register("web_fetch",
		"Fetch a web page and return its text content (HTML stripped).",
		json.RawMessage(`{"type":"object","properties":{
			"url":{"type":"string","description":"URL to fetch"},
			"max_chars":{"type":"integer","default":8000,"description":"Max chars to return"}
		},"required":["url"]}`),
		func(ctx context.Context, input json.RawMessage) (any, error) {
			var p struct {
				URL      string `json:"url"`
				MaxChars int    `json:"max_chars"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return nil, err
			}
			if p.MaxChars <= 0 {
				p.MaxChars = 8000
			}
			content, err := fetcher.Fetch(ctx, p.URL, p.MaxChars)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"url":     p.URL,
				"content": content,
				"length":  len(content),
			}, nil
		},
	)
}
