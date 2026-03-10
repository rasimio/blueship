package gateway

import (
	"context"
	"strings"
	"time"

	"github.com/rasimio/blueship/agent"
)

// HeartbeatJob sends periodic heartbeat prompts through the AgentLoop for all registered users.
type HeartbeatJob struct {
	gateway *Gateway
	tz      *time.Location
}

// NewHeartbeatJob creates a heartbeat job.
func NewHeartbeatJob(gw *Gateway) *HeartbeatJob {
	return &HeartbeatJob{
		gateway: gw,
		tz:      gw.tz,
	}
}

// Run executes one heartbeat iteration for the owner.
func (h *HeartbeatJob) Run(ctx context.Context) error {
	hour := time.Now().In(h.tz).Hour()
	if hour < 8 {
		return nil
	}

	us := h.gateway.GetOwnerUser()
	if us == nil {
		return nil
	}
	us.Mu.Lock()
	busy := us.LoopBusy
	us.Mu.Unlock()
	if busy {
		return nil
	}

	go h.runForUser(ctx, us)
	return nil
}

func (h *HeartbeatJob) runForUser(ctx context.Context, us *UserState) {
	us.Mu.Lock()
	defer us.Mu.Unlock()
	us.LoopBusy = true
	defer func() { us.LoopBusy = false }()

	sess, err := h.gateway.GetOrCreateSession(ctx, us)
	if err != nil {
		h.gateway.logger.Error("heartbeat session error",
			"chat_id", us.ChatID,
			"error", err,
		)
		return
	}

	cfg := h.gateway.deps.Config
	loop := agent.NewLoop(h.gateway.provider, h.gateway.store, us.Registry, cfg, h.gateway.logger)
	loop.SetCompactor(h.gateway.compactor)

	reply, err := loop.Run(ctx, agent.RunConfig{
		SessionID:      sess.ID,
		SystemPrompt:   h.gateway.systemPromptHeartbeat,
		CompactSummary: derefString(sess.CompactSummary),
			Model:        cfg.Models.Primary.Name,
		MaxTokens:      cfg.Limits.MaxOutputTokens,
		MaxTurns:       cfg.Gateway.MaxTurns,
	}, "heartbeat")
	if err != nil {
		h.gateway.logger.Error("heartbeat agent loop error",
			"chat_id", us.ChatID,
			"error", err,
		)
		return
	}

	if reply != "" && len(reply) > 10 && !strings.Contains(reply, "[no-op]") {
		if err := h.gateway.tg.SendLong(ctx, us.ChatID, reply); err != nil {
			h.gateway.logger.Error("heartbeat send error",
				"chat_id", us.ChatID,
				"error", err,
			)
		}
	}
}
