package core

import "encoding/json"

const DefaultMessageBudget = 6000

type MessageBudgetRequest struct {
	Role           string
	ExplicitBudget int
	ExplicitSource string
	ModelRef       ModelRef
	Config         *Config
	SystemPrompt   string
	Tools          []ToolDefinition
}

type MessageBudgetDecision struct {
	Budget int
	Source string
}

func ResolveMessageBudget(req MessageBudgetRequest) MessageBudgetDecision {
	if req.ExplicitBudget > 0 {
		source := req.ExplicitSource
		if source == "" {
			source = "run_config.message_budget"
		}
		return MessageBudgetDecision{Budget: req.ExplicitBudget, Source: source}
	}
	if req.ModelRef.MessageBudget > 0 {
		source := "model_config.message_budget"
		if req.Role != "" {
			source = "model_config." + req.Role + ".message_budget"
		}
		return MessageBudgetDecision{Budget: req.ModelRef.MessageBudget, Source: source}
	}
	if req.Config != nil && req.Config.Limits.ChatMessageBudget > 0 {
		return MessageBudgetDecision{Budget: req.Config.Limits.ChatMessageBudget, Source: "config.limits.chat_message_budget"}
	}

	budget := calculatedMessageBudget(req)
	if budget > 0 {
		return MessageBudgetDecision{Budget: budget, Source: "calculated.context_minus_prompt"}
	}
	return MessageBudgetDecision{Budget: DefaultMessageBudget, Source: "default"}
}

func calculatedMessageBudget(req MessageBudgetRequest) int {
	maxContext := 0
	if req.Config != nil {
		maxContext = req.Config.Limits.MaxContext
	}
	if maxContext <= 0 {
		return 0
	}

	systemTokens := EstimateTextTokens(req.SystemPrompt)
	toolSchemaTokens := 0
	if len(req.Tools) > 0 {
		data, _ := json.Marshal(req.Tools)
		toolSchemaTokens = EstimateTextTokens(string(data))
	}

	budget := maxContext - systemTokens - toolSchemaTokens
	minBudget := 0
	if req.Config != nil {
		minBudget = req.Config.Limits.MinMessageBudget
	}
	if minBudget > 0 && budget < minBudget {
		return minBudget
	}
	return budget
}
