package gateway

import (
	"context"

	"github.com/rasimio/blueship/agent"
)

// ThinkingJob runs autonomous thinking cycles for the owner user.
type ThinkingJob struct {
	gateway *Gateway
}

// NewThinkingJob creates a thinking job.
func NewThinkingJob(gw *Gateway) *ThinkingJob {
	return &ThinkingJob{gateway: gw}
}

// Run executes one thinking iteration for the owner.
func (t *ThinkingJob) Run(ctx context.Context) error {
	us := t.findOwner()
	if us == nil || us.LoopBusy {
		return nil
	}

	go t.runForOwner(ctx, us)
	return nil
}

func (t *ThinkingJob) findOwner() *UserState {
	t.gateway.mu.Lock()
	defer t.gateway.mu.Unlock()

	for _, us := range t.gateway.users {
		if us.IsOwner {
			return us
		}
	}
	return nil
}

func (t *ThinkingJob) runForOwner(ctx context.Context, us *UserState) {
	us.Mu.Lock()
	defer us.Mu.Unlock()
	us.LoopBusy = true
	defer func() { us.LoopBusy = false }()

	sess, err := t.gateway.GetOrCreateSession(ctx, us)
	if err != nil {
		t.gateway.logger.Error("thinking session error",
			"chat_id", us.ChatID,
			"error", err,
		)
		return
	}

	cfg := t.gateway.deps.Config
	loop := agent.NewLoop(t.gateway.provider, t.gateway.store, us.Registry, cfg, t.gateway.logger)
	loop.SetCompactor(t.gateway.compactor)

	reply, err := loop.Run(ctx, agent.RunConfig{
		SessionID:      sess.ID,
		SystemPrompt:   t.gateway.SystemPromptThinking(),
		CompactSummary: derefString(sess.CompactSummary),
		Model:          cfg.Models.Primary,
		MaxTokens:      cfg.Limits.MaxOutputTokens,
		MaxTurns:       cfg.Gateway.MaxTurns,
	}, "[SYSTEM: autonomous thinking cycle — это НЕ сообщение от пользователя. Пользователь тебе НЕ писал. Следуй инструкциям THINKING.]")
	if err != nil {
		t.gateway.logger.Error("thinking agent loop error",
			"chat_id", us.ChatID,
			"error", err,
		)
		return
	}

	if reply != "" {
		if err := t.gateway.tg.SendLong(ctx, us.ChatID, reply); err != nil {
			t.gateway.logger.Error("thinking send error",
				"chat_id", us.ChatID,
				"error", err,
			)
		}
	}
}
