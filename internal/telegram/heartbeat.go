package telegram

import (
	"context"
	"time"

	"github.com/rasimio/blueship/agent"
	bs "github.com/rasimio/blueship/core"
	"github.com/rasimio/blueship/internal/user"
	"github.com/rasimio/blueship/tool"
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
		tz:      gw.Timezone(),
	}
}

// Run executes one heartbeat iteration for all registered users.
func (h *HeartbeatJob) Run(ctx context.Context) error {
	hour := time.Now().In(h.tz).Hour()
	if hour < 8 {
		return nil
	}

	coreDB, err := h.gateway.deps.DB("ship")
	if err != nil {
		return err
	}

	chatIDs, err := user.ListTelegramChatIDs(ctx, coreDB)
	if err != nil {
		return err
	}

	for _, chatID := range chatIDs {
		us := h.gateway.GetUser(chatID)
		if us == nil || us.LoopBusy {
			continue
		}

		go h.runForUser(ctx, us)
	}
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
	loop := agent.NewLoop(h.gateway.Provider(), h.gateway.SessionStore(), us.Registry, cfg, h.gateway.logger)
	loop.SetCompactor(h.gateway.CompactorInstance())

	reply, err := loop.Run(ctx, agent.RunConfig{
		SessionID:    sess.ID,
		SystemPrompt: h.gateway.SystemPromptHeartbeat(),
		Model:        cfg.Models.Primary,
		MaxTokens:    cfg.Limits.MaxOutputTokens,
		MaxTurns:     cfg.Gateway.MaxTurns,
	}, "heartbeat")
	if err != nil {
		h.gateway.logger.Error("heartbeat agent loop error",
			"chat_id", us.ChatID,
			"error", err,
		)
		return
	}

	if reply != "" && len(reply) > 10 && reply != "[no-op]" {
		if err := h.gateway.TG().SendLong(ctx, us.ChatID, reply); err != nil {
			h.gateway.logger.Error("heartbeat send error",
				"chat_id", us.ChatID,
				"error", err,
			)
		}
	}
}

// RegisterBuiltinTools is a convenience function for external use.
func RegisterBuiltinTools(r *bs.ToolRegistry, d *bs.Deps) {
	tool.RegisterBuiltinTools(r, d)
}
