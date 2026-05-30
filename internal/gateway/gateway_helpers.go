package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/session"
)

func (g *Gateway) executePostActions(ctx context.Context, us *UserState, actions []bs.PostAction, reply string) {
	for _, pa := range actions {
		switch pa.Type {
		case "save_reflection":
			// Extract a concise insight from the cortex response via Flash.
			insight := g.extractInsight(ctx, reply, "reflection")
			if insight == "" {
				g.logger.Warn("post-action save_reflection: extraction returned empty")
				continue
			}
			input := fmt.Sprintf(`{"kind":"reflection","content":%q}`, insight)
			result, isError := us.Registry.Execute(ctx, "memory_save", json.RawMessage(input))
			if isError {
				g.logger.Warn("post-action save_reflection failed", "error", result)
			} else {
				g.logger.Info("post-action save_reflection done", "insight", truncateStr(insight, 100))
			}
		case "save_fact":
			insight := g.extractInsight(ctx, reply, "fact")
			if insight == "" {
				g.logger.Warn("post-action save_fact: extraction returned empty")
				continue
			}
			input := fmt.Sprintf(`{"fact":%q,"category":"general","source":"reflex"}`, insight)
			result, isError := us.Registry.Execute(ctx, "memory_save", json.RawMessage(input))
			if isError {
				g.logger.Warn("post-action save_fact failed", "error", result)
			} else {
				g.logger.Info("post-action save_fact done", "insight", truncateStr(insight, 100))
			}
		default:
			g.logger.Warn("unknown post-action type", "type", pa.Type)
		}
	}
}

// extractInsight calls Flash to distill a concise insight from a long cortex response.
// extractType is "reflection" or "fact".
func (g *Gateway) extractInsight(ctx context.Context, response, extractType string) string {
	model := g.reflexModel()
	if model == "" {
		return truncateStr(response, 200) // fallback
	}

	if g.extractInsightPrompt == "" {
		g.logger.Warn("extract-insight prompt not in DB, skipping")
		return ""
	}
	prompt := fmt.Sprintf(g.extractInsightPrompt, extractType, truncateStr(response, 1500))

	resp, err := g.provider.Complete(ctx, bs.CompletionRequest{
		Model:     model,
		MaxTokens: 128,
		System:    g.reflexSystemPrompt,
		Messages:  []bs.Message{{Role: "user", Content: prompt}},
	})
	if err != nil {
		g.logger.Warn("extractInsight failed", "error", err)
		return ""
	}

	text := strings.TrimSpace(bs.ExtractText(resp.Content))
	// `[skip]` is the extract-insight prompt's signal that the response was
	// only an unverified temporal claim — don't persist as reflection/fact.
	// Treating it as empty short-circuits the executePostActions write.
	if text == "[skip]" || strings.HasPrefix(text, "[skip]") {
		g.logger.Info("extractInsight skipped", "type", extractType, "reason", "unverified temporal claim")
		return ""
	}
	g.logger.Info("extractInsight done", "type", extractType, "result", truncateStr(text, 100))
	return text
}

// looksLikeSelfReflection detects cortex responses that contain self-referential
// insights or reflections worth auto-saving. Markers are loaded from
// <Config.Prompts>/self_reflection_markers.md (JSON array). Empty slice
// (file absent) makes the check a no-op.
func (g *Gateway) looksLikeSelfReflection(text string) bool {
	if len(g.selfReflectionMarkers) == 0 {
		return false
	}
	lower := strings.ToLower(text)
	hits := 0
	for _, m := range g.selfReflectionMarkers {
		if strings.Contains(lower, m) {
			hits++
		}
	}
	return hits >= 2
}

func truncateStr(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// callReflex sends a classification request to the reflex model and parses JSON.
func (g *Gateway) callReflex(ctx context.Context, prompt string) (*bs.ReflexResult, error) {
	model := g.reflexModel()
	if model == "" {
		return nil, fmt.Errorf("reflex model not configured")
	}

	g.logger.Info("calling reflex", "model", model)

	// Inject current datetime so reflex can compute dates for temporal_recall.
	now := time.Now().In(g.tz)
	reflexSystem := fmt.Sprintf("[current_datetime: %s]\n\n%s",
		now.Format("2006-01-02 15:04 MST (Monday)"), g.reflexSystemPrompt)

	resp, err := g.provider.Complete(ctx, bs.CompletionRequest{
		Model:     model,
		MaxTokens: 512,
		System:    reflexSystem,
		Messages: []bs.Message{
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("reflex LLM: %w", err)
	}

	text := bs.ExtractText(resp.Content)
	// Strip markdown fences if present.
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		lines := strings.Split(text, "\n")
		if len(lines) > 2 {
			text = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}

	// Parse with flexible tools field: Flash sometimes returns objects instead of strings.
	var raw struct {
		MatchedRules         []string                 `json:"matched_rules"`
		Intent               string                   `json:"intent"`
		Confidence           float64                  `json:"confidence"`
		PreActions           []bs.ToolAction          `json:"pre_actions"`
		PostActions          []bs.PostAction          `json:"post_actions"`
		Tools                json.RawMessage          `json:"tools"`
		Guidance             string                   `json:"guidance"`
		ClarificationOptions []bs.ClarificationOption `json:"clarification_options"`
	}
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, fmt.Errorf("parse reflex JSON %q: %w", text, err)
	}

	result := &bs.ReflexResult{
		MatchedRules:         raw.MatchedRules,
		Intent:               raw.Intent,
		Confidence:           raw.Confidence,
		PreActions:           raw.PreActions,
		PostActions:          raw.PostActions,
		Guidance:             raw.Guidance,
		ClarificationOptions: raw.ClarificationOptions,
	}

	// Try parsing tools as []string first, then as []{"tool":"name",...} objects.
	if len(raw.Tools) > 0 {
		var toolStrings []string
		if err := json.Unmarshal(raw.Tools, &toolStrings); err == nil {
			result.Tools = toolStrings
		} else {
			var toolObjects []struct {
				Tool string `json:"tool"`
			}
			if err := json.Unmarshal(raw.Tools, &toolObjects); err == nil {
				for _, t := range toolObjects {
					result.Tools = append(result.Tools, t.Tool)
				}
			}
		}
	}

	return result, nil
}

func (g *Gateway) keepTypingViaSink(ctx context.Context, sink bs.ResponseSink) {
	_ = sink.SendTyping(ctx)
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = sink.SendTyping(ctx)
		}
	}
}

// GetOrCreateSession returns the soul's single permanent chat session,
// creating it on first contact. There is no rotation: the conversation
// is one continuous thread per (user, soul). Chat history is permanent
// and decoupled from the LLM context window — the agent loop windows the
// context itself (MessagesForAPI) and AME recalls older turns
// associatively. The session is never archived; "resetting" it would
// mean destroying history.
func (g *Gateway) GetOrCreateSession(ctx context.Context, us *UserState) (*session.Session, error) {
	if g.deps.ModelStore != nil {
		_ = g.deps.ModelStore.Refresh(ctx)
	}
	return g.store.GetOrCreate(ctx, us.UserID.String(), g.cortexModelDisplay())
}

// ResetSession archives the active (user, soul) chat session and opens a
// fresh one in its place. The chat_messages rows stay on disk — history
// rendering and AME recall still see them — but the LLM-side context
// window starts blank, so the next turn begins a new thread. Mirrors
// the Telegram /reset command's behaviour for HTTP / web callers.
//
// Caller MUST pin the soul on ctx via bs.WithSoulID; without it
// GetOrCreate cross-pollinates sessions across souls.
func (g *Gateway) ResetSession(ctx context.Context, userID string) (oldID, newID string, err error) {
	if g.deps.ModelStore != nil {
		_ = g.deps.ModelStore.Refresh(ctx)
	}
	sess, err := g.store.GetOrCreate(ctx, userID, g.cortexModelDisplay())
	if err != nil {
		return "", "", fmt.Errorf("reset: get session: %w", err)
	}
	if sess == nil {
		return "", "", fmt.Errorf("reset: no session for user %s", userID)
	}
	oldID = sess.ID
	if err := g.store.Archive(ctx, sess.ID); err != nil {
		return oldID, "", fmt.Errorf("reset: archive: %w", err)
	}
	newSess, err := g.store.CreateWithPrevious(ctx, userID, g.cortexModel(), sess.ID)
	if err != nil {
		return oldID, "", fmt.Errorf("reset: create new: %w", err)
	}
	if newSess == nil {
		return oldID, "", fmt.Errorf("reset: create returned nil")
	}
	g.logger.Info("reset: archived + recreated session",
		"user_id", userID,
		"old_session_id", oldID,
		"new_session_id", newSess.ID,
		"messages_in_old", sess.MessageCount,
	)
	return oldID, newSess.ID, nil
}

// Timezone returns the configured timezone.
func (g *Gateway) Timezone() *time.Location { return g.tz }

// cortexModel returns the cortex (response generator) model in "provider:name" format.
func (g *Gateway) cortexModel() string {
	if g.deps.ModelStore != nil {
		if s := g.deps.ModelStore.ForRouter("cortex"); s != "" {
			return s
		}
	}
	// Fallback to Config.Models.Primary (backwards compat)
	p := g.deps.Config.Models.Primary
	if p.Provider != "" {
		return p.Provider + ":" + p.Name
	}
	return p.Name
}

// reflexModel returns the reflex (classifier) model in "provider:name" format.
// Returns empty string if reflex is not configured.
func (g *Gateway) reflexModel() string {
	if g.deps.ModelStore != nil {
		return g.deps.ModelStore.ForRouter("reflex")
	}
	return ""
}

// buildPriorContext pulls the last `n` chat messages from the current
// session and renders them as a compact "user: ... / assistant: ..."
// thread excerpt for AME embedding. Each message is truncated so the
// concatenated output stays small (embedding is not cheap and long
// context drowns the embed signal). Empty when the session has no
// prior messages or when the store is unavailable.
func (g *Gateway) buildPriorContext(ctx context.Context, sessionID string, n int) string {
	if g.store == nil || sessionID == "" || n <= 0 {
		return ""
	}
	msgs, err := g.store.MessagesForAPI(ctx, sessionID, 0)
	if err != nil || len(msgs) == 0 {
		return ""
	}
	if len(msgs) > n {
		msgs = msgs[len(msgs)-n:]
	}
	const perTurnCap = 280
	var sb strings.Builder
	for _, m := range msgs {
		text := stringifyMessageContent(m.Content)
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if len([]rune(text)) > perTurnCap {
			r := []rune(text)
			text = string(r[:perTurnCap]) + "…"
		}
		role := m.Role
		if role == "user" {
			role = "user"
		} else if role == "assistant" {
			role = "assistant"
		} else {
			continue // skip tool messages — they're noise for embed
		}
		fmt.Fprintf(&sb, "%s: %s\n", role, text)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// stringifyMessageContent flattens a message Content (which can be a
// string or a slice of content blocks) into a plain-text fragment for
// embedding. Tool-use / tool-result blocks are skipped — only text
// blocks contribute.
func stringifyMessageContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []bs.ContentBlock:
		var parts []string
		for _, b := range v {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, " ")
	case []any:
		var parts []string
		for _, raw := range v {
			if m, ok := raw.(map[string]any); ok {
				if t, _ := m["type"].(string); t == "text" {
					if s, _ := m["text"].(string); s != "" {
						parts = append(parts, s)
					}
				}
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func (g *Gateway) cortexModelDisplay() string {
	if g.deps.ModelStore != nil {
		if ref := g.deps.ModelStore.Get("cortex"); ref.Name != "" {
			return ref.Name
		}
	}
	return g.deps.Config.Models.Primary.Name
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// --- Debouncer ---

type pendingMsg struct {
	text      string
	images    []bs.ContentBlock
	messageID int
	// rawAttachments is the per-turn list of files we want to push
	// into the host's CDN (vaelum.chat_attachments + disk store) once
	// the session id is known. We keep the bytes here, not in the
	// images slice, because images travel base64-encoded for the
	// model and the sink needs raw bytes — decoding back would mean
	// allocating twice. Empty on text-only turns.
	rawAttachments []rawAttachment
	// replyToTGMessageID is the Telegram message id of the parent
	// when the user replied via Telegram. processMessages resolves
	// this to our chat_messages.id via the session store before
	// stamping the new row's reply_to_message_id column.
	replyToTGMessageID int
	// replyToMessageID is a directly-supplied parent uuid. Set by
	// the cabinet path (where the frontend knows the parent id
	// natively); 0 / empty for Telegram inbound. When both are set
	// the direct id wins.
	replyToMessageID string
}

// rawAttachment is one inbound file held by pendingMsg until the
// debouncer flushes and the gateway has a session id to stamp it
// with. Kind is the same lane vocabulary as elsewhere ("image" /
// "pdf" / "text").
type rawAttachment struct {
	name string
	mime string
	kind string
	data []byte
}

type debouncer struct {
	mu     sync.Mutex
	msgs   []pendingMsg
	timer  *time.Timer
	fire   func([]pendingMsg)
	window time.Duration
	cap    int
}

func newDebouncer(window time.Duration, cap int, fire func([]pendingMsg)) *debouncer {
	return &debouncer{
		window: window,
		cap:    cap,
		fire:   fire,
	}
}

func (d *debouncer) Add(msg pendingMsg) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.msgs = append(d.msgs, msg)

	if len(d.msgs) >= d.cap {
		d.fireNow()
		return
	}

	if d.timer != nil {
		d.timer.Stop()
	}
	d.timer = time.AfterFunc(d.window, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.fireNow()
	})
}

func (d *debouncer) fireNow() {
	if len(d.msgs) == 0 {
		return
	}
	msgs := d.msgs
	d.msgs = nil
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	d.fire(msgs)
}

// sanitizeLeakedToolCalls removes tool call text that Gemma sometimes
// generates as plain text instead of structured tool_calls.
// Also removes HTML artifacts (<br>, </html>, etc.).
func sanitizeLeakedToolCalls(text string) string {
	// Remove patterns like: call:tool_name{...}
	for {
		idx := strings.Index(text, "call:")
		if idx == -1 {
			break
		}
		// Find the end of the tool call (closing brace)
		end := strings.Index(text[idx:], "}")
		if end == -1 {
			break
		}
		// Also consume any trailing |> or similar tokens
		endAbs := idx + end + 1
		for endAbs < len(text) && (text[endAbs] == '|' || text[endAbs] == '>' || text[endAbs] == '<' || text[endAbs] == ' ' || text[endAbs] == '\n') {
			endAbs++
		}
		text = text[:idx] + text[endAbs:]
	}

	// Remove <tool_call>...</tool_call> blocks
	for {
		start := strings.Index(text, "<tool_call")
		if start == -1 {
			break
		}
		end := strings.Index(text[start:], "</tool_call>")
		if end == -1 {
			end = strings.Index(text[start:], ">")
			if end == -1 {
				break
			}
			text = text[:start] + text[start+end+1:]
		} else {
			text = text[:start] + text[start+end+len("</tool_call>"):]
		}
	}

	// Remove HTML artifacts
	for _, tag := range []string{"<br>", "<br/>", "<br />", "</html>", "<html>", "</body>", "<body>"} {
		text = strings.ReplaceAll(text, tag, "")
	}

	// Remove Gemma thinking/channel control tokens
	for _, tok := range []string{"<channel|>", "</channel>", "\nthought\n", "\n\nthought\n\n"} {
		text = strings.ReplaceAll(text, tok, "")
	}
	// Standalone "thought" at start of response
	text = strings.TrimPrefix(text, "thought\n")
	text = strings.TrimPrefix(text, "thought")

	return strings.TrimSpace(text)
}

// resolveDisambiguation checks if a short message resolves a pending disambiguation.
// Returns the chosen option or nil if the message doesn't match any option.
func resolveDisambiguation(msg string, options []bs.ClarificationOption) *bs.ClarificationOption {
	msg = strings.TrimSpace(strings.ToLower(msg))
	if msg == "" || len(options) == 0 {
		return nil
	}

	// Numeric choice: "1", "2", etc.
	if idx, err := strconv.Atoi(msg); err == nil && idx >= 1 && idx <= len(options) {
		return &options[idx-1]
	}

	// Keyword match against option labels.
	for i := range options {
		label := strings.ToLower(options[i].Label)
		if strings.Contains(label, msg) || strings.Contains(msg, label) {
			return &options[i]
		}
	}

	// Can't resolve programmatically — return nil.
	// Cortex will see the disambiguation in session history and decide from context.
	return nil
}
