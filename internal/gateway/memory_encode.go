package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	bs "github.com/rasimio/blueship/core"
)

// memoryEncoder runs the Recall → Compare → React cycle after each user message.
// Extracts claims, checks novelty against memory, saves/updates/skips automatically.
type memoryEncoder struct {
	llm      bs.CompletionProvider
	embedder func(ctx context.Context, text string) ([]float32, error)
	db       *sqlx.DB
	prompts  bs.PromptStore
	logger   *slog.Logger
	model    string // extraction model (Flash)
}

type claim struct {
	Content    string  `json:"content"`
	Importance float64 `json:"importance"`
}

// extractPromptKey is the system_prompts DB key for the memory extraction prompt.
const extractPromptKey = "memory-extract"

// encode runs the full Recall → Compare → React cycle for a user message.
func (e *memoryEncoder) encode(ctx context.Context, userID, message string) {
	if e.llm == nil || e.embedder == nil || e.db == nil {
		return
	}

	// 1. EXTRACT claims from user message
	claims := e.extractClaims(ctx, message)
	if len(claims) == 0 {
		return
	}

	for _, c := range claims {
		e.processOneClaim(ctx, userID, c)
	}
}

func (e *memoryEncoder) extractClaims(ctx context.Context, message string) []claim {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Load extraction prompt from DB, prepend preamble+soul for persona context.
	var parts []string
	for _, key := range []string{"preamble", "soul", extractPromptKey} {
		p, err := e.prompts.Get(ctx, key)
		if err != nil {
			if key == extractPromptKey {
				e.logger.Warn("memory_encode: prompt not found", "key", key, "error", err)
				return nil
			}
			continue // preamble/soul optional
		}
		parts = append(parts, p)
	}
	systemPrompt := strings.Join(parts, "\n\n")

	resp, err := e.llm.Complete(ctx, bs.CompletionRequest{
		Model:     e.model,
		System:    systemPrompt,
		MaxTokens: 512,
		Messages:  []bs.Message{{Role: "user", Content: bs.NormalizeContent(message)}},
	})
	if err != nil {
		e.logger.Warn("memory_encode: extract failed", "error", err)
		return nil
	}

	text := bs.ExtractText(resp.Content)
	text = strings.TrimSpace(text)

	// Strip markdown fences
	if strings.HasPrefix(text, "```") {
		if i := strings.Index(text, "\n"); i != -1 {
			text = text[i+1:]
		}
		if i := strings.LastIndex(text, "```"); i != -1 {
			text = text[:i]
		}
		text = strings.TrimSpace(text)
	}

	var claims []claim
	if err := json.Unmarshal([]byte(text), &claims); err != nil {
		e.logger.Warn("memory_encode: parse claims failed", "error", err, "text", text)
		return nil
	}
	return claims
}

func (e *memoryEncoder) processOneClaim(ctx context.Context, userID string, c claim) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if strings.TrimSpace(c.Content) == "" {
		return
	}
	if c.Importance == 0 {
		c.Importance = 0.5
	}

	// 2. RECALL — embed claim and search memory
	vec, err := e.embedder(ctx, c.Content)
	if err != nil {
		e.logger.Warn("memory_encode: embed failed", "error", err)
		return
	}

	// Search for similar facts in memory
	var matchID string
	var matchContent string
	var similarity float64

	vecStr := fmt.Sprintf("[%s]", joinFloats(vec))
	_ = e.db.QueryRowxContext(ctx, `
		SELECT e.source_id, m.content, 1 - (e.vector <=> $1::vector) as sim
		FROM embeddings e
		JOIN memory m ON m.id::text = e.source_id
		WHERE m.active = true AND m.kind = 'fact' AND m.user_id = $2
		ORDER BY e.vector <=> $1::vector ASC
		LIMIT 1`,
		vecStr, userID).Scan(&matchID, &matchContent, &similarity)

	// 3. COMPARE + REACT
	switch {
	case similarity < 0.5 || matchID == "":
		// NOVEL — save new fact with boosted importance
		novelImportance := c.Importance
		if novelImportance < 0.6 {
			novelImportance = 0.6 // novelty bonus
		}
		e.saveFact(ctx, userID, c.Content, novelImportance, vec)
		e.logger.Info("memory_encode: NEW fact",
			"content", truncateLog(c.Content, 80),
			"importance", fmt.Sprintf("%.2f", novelImportance),
		)

	case similarity >= 0.5 && similarity < 0.85:
		// SIMILAR — enrich existing
		e.enrichFact(ctx, matchID, c.Content)
		e.logger.Info("memory_encode: ENRICHED",
			"existing", truncateLog(matchContent, 60),
			"new", truncateLog(c.Content, 60),
			"sim", fmt.Sprintf("%.2f", similarity),
		)

	case similarity >= 0.85:
		// KNOWN or CONTRADICTION
		if isContradiction(matchContent, c.Content) {
			e.updateFact(ctx, matchID, c.Content)
			e.logger.Info("memory_encode: CORRECTED",
				"old", truncateLog(matchContent, 60),
				"new", truncateLog(c.Content, 60),
				"sim", fmt.Sprintf("%.2f", similarity),
			)
		} else {
			// Reinforcement — bump access
			e.db.ExecContext(ctx, `UPDATE memory SET access_count = access_count + 1, last_accessed = NOW() WHERE id = $1`, matchID)
		}
	}
}

func (e *memoryEncoder) saveFact(ctx context.Context, userID, content string, importance float64, vec []float32) {
	var id string
	if err := e.db.QueryRowxContext(ctx, `
		INSERT INTO memory (user_id, content, kind, importance, source, active)
		VALUES ($1, $2, 'fact', $3, 'auto-encode', true)
		RETURNING id`,
		userID, content, importance).Scan(&id); err != nil {
		e.logger.Warn("memory_encode: save failed", "error", err)
		return
	}

	// Store embedding
	vecStr := fmt.Sprintf("[%s]", joinFloats(vec))
	e.db.ExecContext(ctx, `
		INSERT INTO embeddings (source_type, source_id, user_id, model, vector)
		VALUES ('fact', $1, $2, 'auto', $3::vector)`,
		id, userID, vecStr)
}

func (e *memoryEncoder) enrichFact(ctx context.Context, matchID, newContent string) {
	e.db.ExecContext(ctx, `
		UPDATE memory SET content = content || E'\n' || $2, access_count = access_count + 1,
		last_accessed = NOW(), updated_at = NOW()
		WHERE id = $1`,
		matchID, newContent)
}

func (e *memoryEncoder) updateFact(ctx context.Context, matchID, newContent string) {
	e.db.ExecContext(ctx, `
		UPDATE memory SET content = $2, access_count = access_count + 1,
		last_accessed = NOW(), updated_at = NOW()
		WHERE id = $1`,
		matchID, newContent)
}

// isContradiction does a simple heuristic check — if contents are very similar
// in embedding space but text differs significantly, it's likely a correction.
func isContradiction(existing, new string) bool {
	// If strings share <50% words, it's probably a correction not a duplicate
	existingWords := strings.Fields(strings.ToLower(existing))
	newWords := strings.Fields(strings.ToLower(new))
	if len(existingWords) == 0 || len(newWords) == 0 {
		return false
	}

	wordSet := make(map[string]bool, len(existingWords))
	for _, w := range existingWords {
		wordSet[w] = true
	}

	overlap := 0
	for _, w := range newWords {
		if wordSet[w] {
			overlap++
		}
	}

	overlapRatio := float64(overlap) / float64(len(newWords))
	return overlapRatio < 0.5 // less than 50% word overlap = likely contradiction
}

func joinFloats(v []float32) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = fmt.Sprintf("%g", f)
	}
	return strings.Join(parts, ",")
}

func truncateLog(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
