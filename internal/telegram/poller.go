package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Poller fetches updates from the Telegram Bot API using long polling.
type Poller struct {
	token      string
	httpClient *http.Client
	offset     int
}

// NewPoller creates a new long-polling Telegram update fetcher.
func NewPoller(token string, pollTimeout time.Duration) *Poller {
	return &Poller{
		token: token,
		httpClient: &http.Client{
			Timeout: pollTimeout,
		},
	}
}

type getUpdatesResponse struct {
	OK     bool     `json:"ok"`
	Result []Update `json:"result"`
}

// Poll makes a single getUpdates call and returns updates.
func (p *Poller) Poll(ctx context.Context) ([]Update, error) {
	url := fmt.Sprintf(
		"https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=30&allowed_updates=[\"message\",\"callback_query\"]",
		p.token, p.offset,
	)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram API status %d: %s", resp.StatusCode, body)
	}

	var result getUpdatesResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if !result.OK {
		return nil, fmt.Errorf("telegram API returned ok=false")
	}

	for _, u := range result.Result {
		if u.UpdateID >= p.offset {
			p.offset = u.UpdateID + 1
		}
	}

	return result.Result, nil
}

// Run polls for updates in a loop until ctx is done, sending each update to ch.
func (p *Poller) Run(ctx context.Context, ch chan<- Update) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		updates, err := p.Poll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		for _, u := range updates {
			select {
			case <-ctx.Done():
				return
			case ch <- u:
			}
		}
	}
}
