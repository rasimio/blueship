package gateway

import (
	"context"
	"fmt"

	"github.com/rasimio/blueship/agent"
	"github.com/rasimio/blueship/internal/user"
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
	if us == nil || us.LoopBusy {
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

	userID, err := user.ResolveByChatID(ctx, coreDB, chatIDStr)
	if err != nil {
		return nil, fmt.Errorf("resolve owner: %w", err)
	}

	// Initialize through gateway (same as getOrInitUser but we already have user info)
	us, err := t.gateway.getOrInitUser(ctx, chatID)
	if err != nil {
		return nil, fmt.Errorf("init owner user %d: %w", chatID, err)
	}
	_ = userID // used implicitly by getOrInitUser

	return us, nil
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
