package agenttask

import "github.com/rasimio/blueship/internal/core"

// Every v2 auditor still receives the complete report and the same selected
// evidence. A separate source message lets provider prompt caching reuse that
// corpus across targets and repairs; it never reuses a verdict for changed text.
func groundingRequest(deps core.AgentDeps, task core.AgentTask, report string, docs []ToolOutput, target string) (core.CompletionRequest, groundingDocStats) {
	var messages []core.Message
	var stats groundingDocStats
	var user string
	if task.ExecutorVersion == 2 {
		var sources string
		sources, stats = buildGroundingSourceMessage(report, docs)
		messages = append(messages, core.Message{Role: "user", Content: core.NormalizeContent(sources)})
		user = "[report]\n" + report
	} else {
		user, stats = buildGroundingUserMessage(report, docs)
	}
	if target != "" {
		user += "\n\n[audit_target]\n" + target
	}
	messages = append(messages, core.Message{Role: "user", Content: core.NormalizeContent(user)})
	model := pickGroundingModel(deps)
	return core.CompletionRequest{
		Model:        model.ForRouter(),
		System:       groundingPrompt(task),
		Messages:     messages,
		MaxTokens:    groundingMaxOutputToks,
		Temperature:  0.2,
		Effort:       model.Effort,
		ThinkingMode: model.ThinkingMode,
	}, stats
}
