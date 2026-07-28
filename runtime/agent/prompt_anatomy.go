package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	bs "github.com/rasimio/blueship/internal/core"
)

type promptAnatomy struct {
	rawSystemSHA256       string
	effectiveSystemSHA256 string
	rawSystemTokens       int
	compactTokens         int
	baseSystemTokens      int
	effectiveSystem       int
	toolSchemaTokens      int
	dialogTokens          int
	scratchpadTokens      int
	messageTokens         int
	turnContextTokens     int
	estimatedInput        int
	rawSystemRunes        int
	compactRunes          int
	effectiveSystemRunes  int
	turnContextRunes      int
	messageRunes          int
	messageBytes          int
	toolSchemaBytes       int
	roleCounts            string
	blockCounts           string
	maxMessageRole        string
	maxMessageIndex       int
	maxMessageTokens      int
	maxMessageRunes       int
	toolNames             string
	toolSources           string
}

func newPromptAnatomy(
	registry *bs.ToolRegistry,
	rawSystem string,
	compactSummary string,
	baseSystem string,
	effectiveSystem string,
	turnContext string,
	tools []bs.ToolDefinition,
	messages []bs.Message,
	dialogTokens int,
	scratchpadTokens int,
) promptAnatomy {
	toolSchemaBytes := 0
	if len(tools) > 0 {
		data, _ := json.Marshal(tools)
		toolSchemaBytes = len(data)
	}

	msgStats := summarizePromptMessages(messages)
	toolNames, toolSources := summarizePromptTools(registry, tools)

	return promptAnatomy{
		rawSystemSHA256:       fmt.Sprintf("%x", sha256.Sum256([]byte(rawSystem))),
		effectiveSystemSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(effectiveSystem))),
		rawSystemTokens:       estimateTextTokens(rawSystem),
		compactTokens:         estimateTextTokens(compactSummary),
		baseSystemTokens:      estimateTextTokens(baseSystem),
		effectiveSystem:       estimateTextTokens(effectiveSystem),
		toolSchemaTokens:      estimateToolSchemaTokens(tools),
		dialogTokens:          dialogTokens,
		scratchpadTokens:      scratchpadTokens,
		messageTokens:         estimateMessagesTokens(messages),
		turnContextTokens:     estimateTextTokens(turnContext),
		estimatedInput:        estimateTextTokens(effectiveSystem) + estimateToolSchemaTokens(tools) + estimateMessagesTokens(messages),
		rawSystemRunes:        len([]rune(rawSystem)),
		compactRunes:          len([]rune(compactSummary)),
		effectiveSystemRunes:  len([]rune(effectiveSystem)),
		turnContextRunes:      len([]rune(turnContext)),
		messageRunes:          msgStats.runes,
		messageBytes:          msgStats.bytes,
		toolSchemaBytes:       toolSchemaBytes,
		roleCounts:            orderedPromptCountString(msgStats.roleCounts, []string{"user", "assistant", "system", "tool"}),
		blockCounts:           orderedPromptCountString(msgStats.blockCounts, []string{"text", "tool_use", "tool_result", "image", "other"}),
		maxMessageRole:        msgStats.maxRole,
		maxMessageIndex:       msgStats.maxIndex,
		maxMessageTokens:      msgStats.maxTokens,
		maxMessageRunes:       msgStats.maxRunes,
		toolNames:             toolNames,
		toolSources:           toolSources,
	}
}

func (p promptAnatomy) logAttrs(sessionID string) []any {
	return []any{
		"session_id", sessionID,
		"prompt_raw_system_sha256", p.rawSystemSHA256,
		"prompt_effective_system_sha256", p.effectiveSystemSHA256,
		"raw_system_tokens_estimate", p.rawSystemTokens,
		"compact_summary_tokens_estimate", p.compactTokens,
		"base_system_tokens_estimate", p.baseSystemTokens,
		"system_tokens_estimate", p.effectiveSystem,
		"tool_schema_tokens_estimate", p.toolSchemaTokens,
		"dialog_message_tokens_estimate", p.dialogTokens,
		"scratchpad_tokens_estimate", p.scratchpadTokens,
		"message_tokens_estimate", p.messageTokens,
		"turn_context_tokens_estimate", p.turnContextTokens,
		"prompt_estimated_input_tokens", p.estimatedInput,
		"prompt_raw_system_runes", p.rawSystemRunes,
		"prompt_compact_summary_runes", p.compactRunes,
		"prompt_effective_system_runes", p.effectiveSystemRunes,
		"prompt_turn_context_runes", p.turnContextRunes,
		"prompt_message_runes", p.messageRunes,
		"prompt_message_bytes", p.messageBytes,
		"prompt_tool_schema_bytes", p.toolSchemaBytes,
		"prompt_message_role_counts", p.roleCounts,
		"prompt_message_block_counts", p.blockCounts,
		"prompt_max_message_role", p.maxMessageRole,
		"prompt_max_message_index", p.maxMessageIndex,
		"prompt_max_message_tokens_estimate", p.maxMessageTokens,
		"prompt_max_message_runes", p.maxMessageRunes,
		"prompt_tool_names", p.toolNames,
		"prompt_tool_sources", p.toolSources,
	}
}

func (p promptAnatomy) responseAttrs(inputTokens int) []any {
	attrs := []any{"prompt_estimated_input_tokens", p.estimatedInput}
	if inputTokens > 0 && p.estimatedInput > 0 {
		attrs = append(attrs,
			"prompt_input_to_estimate_ratio", fmt.Sprintf("%.2f", float64(inputTokens)/float64(p.estimatedInput)),
			"prompt_input_minus_estimate", inputTokens-p.estimatedInput,
		)
	}
	return attrs
}

type promptMessageStats struct {
	runes       int
	bytes       int
	roleCounts  map[string]int
	blockCounts map[string]int
	maxRole     string
	maxIndex    int
	maxTokens   int
	maxRunes    int
}

func summarizePromptMessages(messages []bs.Message) promptMessageStats {
	stats := promptMessageStats{
		roleCounts:  make(map[string]int),
		blockCounts: make(map[string]int),
		maxIndex:    -1,
	}
	for i, msg := range messages {
		role := msg.Role
		if role == "" {
			role = "unknown"
		}
		stats.roleCounts[role]++
		blocks := bs.NormalizeContent(msg.Content)
		messageTokens := bs.EstimateTokens(blocks)
		messageStats := summarizePromptBlocks(blocks)
		stats.runes += messageStats.runes
		stats.bytes += messageStats.bytes
		for key, value := range messageStats.blockCounts {
			stats.blockCounts[key] += value
		}
		if messageTokens > stats.maxTokens {
			stats.maxRole = role
			stats.maxIndex = i
			stats.maxTokens = messageTokens
			stats.maxRunes = messageStats.runes
		}
	}
	return stats
}

type promptBlockStats struct {
	runes       int
	bytes       int
	blockCounts map[string]int
}

func summarizePromptBlocks(blocks []bs.ContentBlock) promptBlockStats {
	stats := promptBlockStats{blockCounts: make(map[string]int)}
	for _, block := range blocks {
		typ := block.Type
		if typ == "" {
			typ = "other"
		}
		switch typ {
		case "text":
			stats.blockCounts["text"]++
			addPromptTextStats(&stats, block.Text)
		case "tool_use":
			stats.blockCounts["tool_use"]++
			addPromptTextStats(&stats, block.Name)
			addPromptTextStats(&stats, string(block.Input))
			addPromptTextStats(&stats, block.ThoughtSignature)
		case "tool_result":
			stats.blockCounts["tool_result"]++
			addPromptTextStats(&stats, block.Name)
			switch content := block.Content.(type) {
			case string:
				addPromptTextStats(&stats, content)
			default:
				data, _ := json.Marshal(content)
				addPromptTextStats(&stats, string(data))
			}
		case "image":
			stats.blockCounts["image"]++
			if block.Source != nil {
				stats.bytes += len(block.Source.Data)
			}
		default:
			stats.blockCounts["other"]++
			data, _ := json.Marshal(block)
			addPromptTextStats(&stats, string(data))
		}
	}
	return stats
}

func addPromptTextStats(stats *promptBlockStats, text string) {
	stats.runes += len([]rune(text))
	stats.bytes += len(text)
}

func summarizePromptTools(registry *bs.ToolRegistry, tools []bs.ToolDefinition) (names string, sources string) {
	if len(tools) == 0 {
		return "", ""
	}
	toolNames := make([]string, 0, len(tools))
	sourceCounts := make(map[string]int)
	for _, tool := range tools {
		toolNames = append(toolNames, tool.Name)
		source := "local"
		if registry != nil {
			if peer := registry.PeerForTool(tool.Name); peer != "" {
				source = "peer:" + peer
			}
		}
		sourceCounts[source]++
	}
	return joinPromptValues(toolNames, 40), orderedPromptCountString(sourceCounts, []string{"local"})
}

func joinPromptValues(values []string, limit int) string {
	if len(values) == 0 {
		return ""
	}
	if limit <= 0 || len(values) <= limit {
		return strings.Join(values, ",")
	}
	clipped := append([]string{}, values[:limit]...)
	clipped = append(clipped, fmt.Sprintf("+%d_more", len(values)-limit))
	return strings.Join(clipped, ",")
}

func orderedPromptCountString(counts map[string]int, order []string) string {
	if len(counts) == 0 {
		return ""
	}
	seen := make(map[string]bool, len(order))
	parts := make([]string, 0, len(counts))
	for _, key := range order {
		seen[key] = true
		if value := counts[key]; value > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", key, value))
		}
	}
	var extras []string
	for key, value := range counts {
		if !seen[key] && value > 0 {
			extras = append(extras, key)
		}
	}
	sort.Strings(extras)
	for _, key := range extras {
		parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
	}
	return strings.Join(parts, ",")
}
