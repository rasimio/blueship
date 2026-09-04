package blueship

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// CascadeRoute is one independent inference endpoint. Routes are tried in order.
// Each model's context window must cover the host's request budget: a failover
// must not silently discard completed tool results or instructions.
type CascadeRoute struct {
	Name      string
	Model     string
	Provider  CompletionProvider
	Timeout   time.Duration
	Cooldown  time.Duration
	Effort    string
	MaxTokens int
}

type CascadeRouteStatus struct {
	Name     string    `json:"name"`
	Model    string    `json:"model"`
	Failures int       `json:"failures"`
	RetryAt  time.Time `json:"retry_at,omitempty"`
	Probing  bool      `json:"probing"`
}

type CascadeStatus struct {
	LastSuccessfulRoute string               `json:"last_successful_route"`
	Routes              []CascadeRouteStatus `json:"routes"`
}

// CascadeProvider provides bounded, cancellation-aware failover with endpoint
// cooldowns. It does not execute tools. Stream attempts cannot be replayed after
// any callback has been delivered, even when the final response is lost.
type CascadeProvider struct {
	routes      []CascadeRoute
	mu          sync.Mutex
	state       []CascadeRouteStatus
	lastSuccess string
	now         func() time.Time
}

func NewCascadeProvider(routes []CascadeRoute) (*CascadeProvider, error) {
	if len(routes) == 0 {
		return nil, errors.New("cascade: at least one route is required")
	}
	c := &CascadeProvider{routes: append([]CascadeRoute(nil), routes...), now: time.Now}
	seen := map[string]bool{}
	for i := range c.routes {
		r := &c.routes[i]
		if r.Name == "" || r.Model == "" || r.Provider == nil || seen[r.Name] {
			return nil, fmt.Errorf("cascade: route %d needs a unique name, model and provider", i)
		}
		seen[r.Name] = true
		if r.Timeout <= 0 {
			r.Timeout = 120 * time.Second
		}
		if r.Cooldown <= 0 {
			r.Cooldown = 30 * time.Second
		}
		c.state = append(c.state, CascadeRouteStatus{Name: r.Name, Model: r.Model})
	}
	return c, nil
}

func (c *CascadeProvider) Status() CascadeStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CascadeStatus{LastSuccessfulRoute: c.lastSuccess, Routes: append([]CascadeRouteStatus(nil), c.state...)}
}

func (c *CascadeProvider) acquire(i int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &c.state[i]
	if s.Failures == 0 {
		return true
	}
	if s.Probing || c.now().Before(s.RetryAt) {
		return false
	}
	s.Probing = true // only one caller probes an unhealthy endpoint
	return true
}

func (c *CascadeProvider) finish(i int, err error, cancelled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &c.state[i]
	s.Probing = false
	if cancelled {
		return
	} // cancellation says nothing about provider health
	if err == nil {
		s.Failures = 0
		s.RetryAt = time.Time{}
		c.lastSuccess = s.Name
		return
	}
	s.Failures++
	s.RetryAt = c.now().Add(c.routes[i].Cooldown)
}

func (c *CascadeProvider) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	return c.complete(ctx, req, nil, false)
}

func (c *CascadeProvider) StreamComplete(ctx context.Context, req CompletionRequest, cb *StreamCallbacks) (*CompletionResponse, error) {
	return c.complete(ctx, req, cb, true)
}

func (c *CascadeProvider) complete(ctx context.Context, req CompletionRequest, cb *StreamCallbacks, stream bool) (*CompletionResponse, error) {
	var failures []error
	selection, selected := ctx.Value(cascadeSelectionKey{}).(CascadeSelection)
	preferred := 0
	if selection.Route != "" {
		preferred = -1
		for i, route := range c.routes {
			if route.Name == selection.Route {
				preferred = i
				break
			}
		}
		if preferred < 0 {
			return nil, fmt.Errorf("cascade: unknown route %q", selection.Route)
		}
	}
	order := []int{preferred}
	if !selection.Only {
		for i := range c.routes {
			if i != preferred {
				order = append(order, i)
			}
		}
	}
	observer, _ := ctx.Value(cascadeObserverKey{}).(func(CascadeAttempt))
	for _, i := range order {
		route := c.routes[i]
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !c.acquire(i) {
			continue
		}
		attempt := req // don't mutate the caller's model or generation parameters
		attempt.Model = route.Model
		if route.Effort != "" {
			attempt.Effort = route.Effort
		}
		if selected {
			attempt.Effort = route.Effort
			if i == preferred {
				if selection.Model != "" {
					attempt.Model = selection.Model
				}
				if selection.Effort != nil {
					attempt.Effort = *selection.Effort
				}
			}
		}
		if route.MaxTokens > 0 && (attempt.MaxTokens <= 0 || attempt.MaxTokens > route.MaxTokens) {
			attempt.MaxTokens = route.MaxTokens
		}
		attemptCtx, cancel := context.WithTimeout(ctx, route.Timeout)
		observed := CascadeAttempt{Route: route.Name, Model: attempt.Model, Effort: attempt.Effort, Phase: "started"}
		if observer != nil {
			observer(observed)
		}
		var response *CompletionResponse
		var err error
		var emitted atomic.Bool
		callbacks := guardedCallbacks(cb, &emitted)
		if sp, ok := route.Provider.(StreamCompletionProvider); stream && ok {
			response, err = sp.StreamComplete(attemptCtx, attempt, callbacks)
		} else {
			response, err = route.Provider.Complete(attemptCtx, attempt)
			if err == nil && response != nil && stream && callbacks != nil {
				for _, b := range response.Content {
					if b.Type == "text" && callbacks.OnText != nil {
						callbacks.OnText(b.Text)
					}
					if b.Type == "tool_use" && callbacks.OnToolUse != nil {
						callbacks.OnToolUse(b.ID, b.Name, b.Input)
					}
				}
			}
		}
		if err == nil && attemptCtx.Err() != nil {
			err = attemptCtx.Err()
		}
		cancel()
		if err == nil && response == nil {
			err = errors.New("provider returned a nil response")
		}
		c.finish(i, err, ctx.Err() != nil)
		observed.Phase = "succeeded"
		if err != nil {
			observed.Phase = "failed"
		}
		if ctx.Err() != nil {
			observed.Phase = "cancelled"
		}
		if response != nil {
			observed.Usage = response.Usage
		}
		if observer != nil {
			observer(observed)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err == nil {
			return response, nil
		}
		wrapped := fmt.Errorf("cascade route %s: %w", route.Name, err)
		if emitted.Load() {
			return nil, wrapped
		}
		failures = append(failures, wrapped)
	}
	if len(failures) == 0 {
		return nil, errors.New("cascade: all routes are cooling down")
	}
	return nil, errors.Join(failures...)
}
