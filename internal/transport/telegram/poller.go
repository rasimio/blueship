package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Poller fetches updates from the Telegram Bot API using long polling.
type Poller struct {
	token      string
	apiBaseURL string
	httpClient *http.Client
	offset     int
}

// NewPoller creates a new long-polling Telegram update fetcher.
func NewPoller(token string, pollTimeout time.Duration) *Poller {
	return NewPollerWithAPIURL(token, "", pollTimeout)
}

// NewPollerWithAPIURL creates a poller backed by a custom Bot API endpoint.
func NewPollerWithAPIURL(token, apiBaseURL string, pollTimeout time.Duration) *Poller {
	return &Poller{
		token:      token,
		apiBaseURL: strings.TrimRight(strings.TrimSpace(apiBaseURL), "/"),
		httpClient: &http.Client{
			Timeout: pollTimeout,
		},
	}
}

func (p *Poller) baseURL() string {
	if p.apiBaseURL != "" {
		return p.apiBaseURL
	}
	return "https://api.telegram.org"
}

// Offset is the cursor to send on the next poll. Persist it only after the
// returned updates have been durably accepted by the consumer.
func (p *Poller) Offset() int { return p.offset }

// SetOffset restores a durable cursor on startup. Call from the polling
// goroutine before Poll, never concurrently with it.
func (p *Poller) SetOffset(offset int) {
	if offset >= 0 {
		p.offset = offset
	}
}

type getUpdatesResponse struct {
	OK     bool     `json:"ok"`
	Result []Update `json:"result"`
}

// Poll makes a single getUpdates call and returns updates.
func (p *Poller) Poll(ctx context.Context) ([]Update, error) {
	url := fmt.Sprintf(
		"%s/bot%s/getUpdates?offset=%d&timeout=30&allowed_updates="+allowedUpdatesJSON(),
		p.baseURL(), p.token, p.offset,
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

// AllowedUpdates is what the bot asks Telegram to deliver.
//
// An update type missing from this list is not delivered at all, and
// there is no error anywhere — the bot simply never hears about it.
// pre_checkout_query is the expensive one: unanswered, Telegram cancels
// the payment and tells the buyer it failed, which looks like a broken
// card rather than a line missing from a query string.
var AllowedUpdates = []string{"message", "callback_query", "pre_checkout_query"}

func allowedUpdatesJSON() string {
	quoted := make([]string, len(AllowedUpdates))
	for i, u := range AllowedUpdates {
		quoted[i] = strconv.Quote(u)
	}
	return "[" + strings.Join(quoted, ",") + "]"
}
