package core

// ReflexResult is the structured output of the reflex (System 1) classification phase.
type ReflexResult struct {
	// MatchedRules contains IDs of rules whose triggers match the user message.
	MatchedRules []string `json:"matched_rules"`
	// Tools lists tool names the cortex model should have access to.
	// nil = use role default; empty slice = no tools needed.
	Tools []string `json:"tools"`
	// Intent classifies the user message purpose.
	Intent string `json:"intent"`
	// Confidence is the reflex model's self-assessed confidence (0.0-1.0).
	// Below threshold → fallback to full context + all role tools.
	Confidence float64 `json:"confidence"`
}

// CandidateRule is a rule found by supplementary search, sent to reflex for classification.
type CandidateRule struct {
	ID          string  `json:"id"`
	Trigger     string  `json:"trigger"`
	Action      string  `json:"action"`
	SuccessRate float64 `json:"success_rate"`
}

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
	// Strategy from AME (warm, neutral, etc.)
	Strategy string
	// ContextTokens estimated token count
	ContextTokens int
	// Degraded indicates if emotion detection was unavailable.
	Degraded bool
}
