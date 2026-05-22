package core

import "encoding/json"

// ReflexResult is the structured output of the reflex (System 1) planning phase.
type ReflexResult struct {
	// MatchedRules contains IDs of rules whose triggers match the user message.
	MatchedRules []string `json:"matched_rules"`
	// Intent classifies the user message purpose.
	Intent string `json:"intent"`
	// Confidence is the reflex model's self-assessed confidence (0.0-1.0).
	// Below threshold → fallback to full context + all role tools.
	Confidence float64 `json:"confidence"`
	// PreActions are tools to execute BEFORE cortex. Results become context.
	PreActions []ToolAction `json:"pre_actions"`
	// PostActions are actions to execute AFTER cortex response.
	PostActions []PostAction `json:"post_actions"`
	// Tools lists tool names the cortex model should have access to during generation.
	// nil = use role default; empty slice = no tools needed.
	Tools []string `json:"tools"`
	// Guidance is a free-form instruction the reflex model wants the cortex
	// to follow for this turn. It is prepended to the rule-engine guidance
	// the gateway injects into the cortex prompt.
	Guidance string `json:"guidance,omitempty"`
	// ClarificationOptions is populated when intent=clarification_needed.
	// Each option describes a plausible tool + human-readable label.
	// Gateway formats these into a numbered list for cortex to present.
	ClarificationOptions []ClarificationOption `json:"clarification_options,omitempty"`
}

// ClarificationOption is one candidate tool when reflex detects ambiguity.
type ClarificationOption struct {
	Tool  string `json:"tool"`
	Label string `json:"label"`
}

// ToolAction is a tool call planned by reflex, executed by gateway.
type ToolAction struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input"`
}

// PostAction is an action to execute after cortex generates a response.
type PostAction struct {
	// Type: "save_reflection", "save_fact"
	Type string `json:"type"`
}

// CandidateRule is a rule found by supplementary search, sent to reflex for classification.
type CandidateRule struct {
	ID          string  `json:"id"`
	Trigger     string  `json:"trigger"`
	Action      string  `json:"action"`
	SuccessRate float64 `json:"success_rate"`
}

// RuleContext carries the current situation for rule engine evaluation.
type RuleContext struct {
	UserID   string  // user identifier
	Intent   string  // from reflex (optional, for intent-scoped rules)
	Strategy string  // from AME: warm, neutral, empathetic, etc.
	Energy   float64 // user energy level (0-1)
	Stress   float64 // user stress level (0-1)
	Hour     int     // current hour (0-23)
	Message  string  // user message text
}

// ActiveRule is a rule matched by the rule engine.
type ActiveRule struct {
	ID         string       `json:"id"`
	Trigger    string       `json:"trigger"`
	Action     string       `json:"action"`
	PreActions []ToolAction `json:"pre_actions,omitempty"` // tools to run BEFORE cortex
	Tools      []string     `json:"tools,omitempty"`       // tools cortex can use
	// Silent, when true, instructs the gateway to abort the current turn:
	// no cortex call, no message sent to the transport. Used for hard
	// "do not respond" rules that cannot be enforced via prompt injection
	// (cortex routinely ignores soft instructions). The rule is still
	// recorded in audit logs so the silence is observable.
	Silent bool `json:"silent,omitempty"`
}

// InterjectionClass classifies a user utterance that arrives while the
// assistant is still mid-response (barge-in). It decides whether the
// in-flight turn should keep running or be interrupted.
type InterjectionClass string

const (
	// InterjectionBackchannel — the user is acknowledging / encouraging
	// ("ага", "понятно", "да-да"); the in-flight turn keeps running.
	InterjectionBackchannel InterjectionClass = "backchannel"
	// InterjectionInterrupt — the user is correcting or redirecting; the
	// in-flight turn is cancelled and the utterance starts a fresh turn.
	InterjectionInterrupt InterjectionClass = "interruption"
	// InterjectionUnclear — ambiguous; treated as backchannel so the
	// assistant is never cut off on a guess.
	InterjectionUnclear InterjectionClass = "unclear"
)

// ReflexContext is the structured output of the context preparation phase.
// It separates AME traces from candidate rules so the reflex can classify rules
// independently, then the gateway reassembles the final context.
type ReflexContext struct {
	// FormattedTraces contains AME traces (facts, reflections, episodes, relations)
	// formatted as [memory], [insight], [episode], [relation] lines. No rules.
	FormattedTraces string
	// CandidateRules are rules found by keyword + semantic search.
	// Sent to reflex for classification.
	CandidateRules []CandidateRule
	// FullContext is the complete formatted context (traces + rules) for fallback.
	FullContext string
	// ActiveNotes is a formatted summary of active notes/tasks for reflex classification.
	ActiveNotes string
	// Strategy from AME (warm, neutral, etc.)
	Strategy string
	// ContextTokens estimated token count
	ContextTokens int
	// Degraded indicates if emotion detection was unavailable.
	Degraded bool
}
