package gateway

import (
	"context"
	"fmt"

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
	us, err := t.ensureOwner(ctx)
	if err != nil {
		t.gateway.logger.Warn("thinking: cannot resolve owner", "error", err)
		return nil
	}
	if us == nil {
		return nil
	}

	us.Mu.Lock()
	busy := us.LoopBusy
	us.Mu.Unlock()
	if busy {
		return nil
	}

	go t.runForOwner(ctx, us)
	return nil
}

// ensureOwner returns the owner UserState, initializing from DB if needed.
func (t *ThinkingJob) ensureOwner(ctx context.Context) (*UserState, error) {
	// Fast path: already in memory
	t.gateway.mu.Lock()
	for _, us := range t.gateway.users {
		if us.IsOwner {
			t.gateway.mu.Unlock()
			return us, nil
		}
	}
	t.gateway.mu.Unlock()

	// Slow path: resolve from user_profiles
	coreDB, err := t.gateway.deps.DB("ship")
	if err != nil {
		return nil, fmt.Errorf("core DB: %w", err)
	}

	// Find owner's chat_id
	var chatIDStr string
	err = coreDB.GetContext(ctx, &chatIDStr,
		`SELECT chat_id FROM user_profiles WHERE is_owner = true AND chat_id LIKE 'telegram:%' LIMIT 1`)
	if err != nil {
		return nil, fmt.Errorf("find owner chat_id: %w", err)
	}

	var chatID int64
	if _, err := fmt.Sscanf(chatIDStr, "telegram:%d", &chatID); err != nil {
		return nil, fmt.Errorf("parse chat_id %s: %w", chatIDStr, err)
	}

	// Initialize through gateway — resolves user internally.
	us, err := t.gateway.getOrInitUser(ctx, chatID)
	if err != nil {
		return nil, fmt.Errorf("init owner user %d: %w", chatID, err)
	}

	return us, nil
}

func (t *ThinkingJob) runForOwner(ctx context.Context, us *UserState) {
	// Briefly lock to set up cancellation and mark busy.
	us.Mu.Lock()
	if us.LoopBusy {
		us.Mu.Unlock()
		return
	}
	thinkCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	us.thinkingCancel = cancel
	us.thinkingDone = done
	us.LoopBusy = true
	us.Mu.Unlock()

	defer func() {
		cancel()
		us.Mu.Lock()
		us.LoopBusy = false
		us.thinkingCancel = nil
		us.thinkingDone = nil
		us.Mu.Unlock()
		close(done) // signal waiters (processMessages) that thinking is done
	}()

	sess, err := t.gateway.GetOrCreateSession(thinkCtx, us)
	if err != nil {
		if thinkCtx.Err() != nil {
			return // cancelled by user message — not an error
		}
		t.gateway.logger.Error("thinking session error",
			"chat_id", us.ChatID,
			"error", err,
		)
		t.gateway.notifyOwnerError(ctx, "thinking/session", err)
		return
	}

	cfg := t.gateway.deps.Config
	loop := agent.NewLoop(t.gateway.provider, t.gateway.store, us.Registry, cfg, t.gateway.logger)
	loop.SetCompactor(t.gateway.compactor)

	reply, err := loop.Run(thinkCtx, agent.RunConfig{
		SessionID:      sess.ID,
		SystemPrompt:   t.gateway.SystemPromptThinking(),
		CompactSummary: derefString(sess.CompactSummary),
		Model:          t.gateway.primaryModel(),
		MaxTokens:      cfg.Limits.MaxOutputTokens,
		MaxTurns:       cfg.Gateway.MaxTurns,
	}, "[SYSTEM: autonomous thinking cycle — это НЕ сообщение от пользователя. Пользователь тебе НЕ писал. Следуй инструкциям THINKING.]")
	if err != nil {
		if thinkCtx.Err() != nil {
			return // cancelled by user message — not an error
		}
		t.gateway.logger.Error("thinking agent loop error",
			"chat_id", us.ChatID,
			"error", err,
		)
		t.gateway.notifyOwnerError(ctx, "thinking/agent", err)
		return
	}

	// Don't send reply if thinking was cancelled mid-flight
	if thinkCtx.Err() != nil {
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
