package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/runtime/agent"
)

func (g *Gateway) messageBudgetForRole(role string, ref core.ModelRef) core.MessageBudgetDecision {
	if ref.MessageBudget <= 0 && g.deps.ModelStore != nil {
		if dbRef := g.deps.ModelStore.Get(role); dbRef.MessageBudget > 0 {
			ref = dbRef
		}
	}
	return core.ResolveMessageBudget(core.MessageBudgetRequest{
		Role:     role,
		ModelRef: ref,
		Config:   g.deps.Config,
	})
}

func (g *Gateway) maybeScheduleSoftSummary(us *UserState, sessionID string) {
	limits := g.deps.Config.Limits
	if limits.SoftSummaryThreshold <= 0 || sessionID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), g.deps.Config.Timeouts.Compact)
		defer cancel()
		ctx = core.WithSoulID(ctx, us.SoulID)

		status, err := g.store.SoftSummaryStatus(ctx, sessionID, limits.SoftSummaryThreshold, limits.SoftSummaryMinNew)
		if err != nil {
			g.logger.Warn("soft summary: status failed", "session_id", sessionID, "error", err)
			return
		}
		if !status.Should {
			return
		}

		msgs, err := g.store.AllMessagesForAPI(ctx, sessionID)
		if err != nil {
			g.logger.Warn("soft summary: load messages failed", "session_id", sessionID, "error", err)
			return
		}
		compactor := agent.NewCompactor(g.provider, g.deps.Config, g.logger)
		if compactor == nil {
			return
		}
		// The summary this produces is the sole carrier of pre-window history:
		// it rides inside the cortex system block on every later turn. Without
		// the compact prompt the summarizer runs uninstructed — no length cap,
		// no preservation rules — which is how 3K+-token summaries ended up in
		// the cacheable system segment. Same degradation contract as the
		// background path (newTaskCompactor): missing file → uninstructed, but
		// loudly.
		if dir := g.deps.Config.Prompts; dir != "" {
			if data, rerr := os.ReadFile(filepath.Join(dir, "compact.md")); rerr == nil && strings.TrimSpace(string(data)) != "" {
				compactor.SetSystemPrompt(string(data))
			} else {
				g.logger.Warn("soft summary: compact.md not loaded — summarizing without instructions", "error", rerr)
			}
		}
		summary, kept, err := compactor.Compact(ctx, msgs)
		if err != nil {
			g.logger.Warn("soft summary: compact failed", "session_id", sessionID, "error", err)
			return
		}
		if strings.TrimSpace(summary) == "" {
			return
		}

		totalTokens := estimateGatewayMessagesTokens(msgs)
		keptTokens := estimateGatewayMessagesTokens(kept)
		if err := g.store.AppendSoftSummary(ctx, sessionID, status.LatestMsgID, len(msgs)-len(kept), totalTokens-keptTokens, summary); err != nil {
			g.logger.Warn("soft summary: persist failed", "session_id", sessionID, "error", err)
			return
		}
		g.logger.Info("soft summary: persisted",
			"session_id", sessionID,
			"total_tokens", status.TokenCount,
			"new_tokens", status.NewTokens,
			"summarized_msgs", len(msgs)-len(kept),
			"summarized_tokens", totalTokens-keptTokens,
		)
	}()
}

func estimateGatewayMessagesTokens(messages []core.Message) int {
	total := 0
	for _, msg := range messages {
		total += core.EstimateTokens(core.NormalizeContent(msg.Content))
	}
	return total
}
