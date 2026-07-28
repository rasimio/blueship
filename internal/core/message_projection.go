package core

import (
	"bytes"
	"encoding/json"
	"strings"
)

// MessageProjectionStatus describes whether a chat_messages row has safe,
// user-visible text for transcript retrieval.
type MessageProjectionStatus string

const (
	ProjectionProjected           MessageProjectionStatus = "projected"
	ProjectionNonDialogue         MessageProjectionStatus = "non_dialogue"
	ProjectionUnprojectableLegacy MessageProjectionStatus = "unprojectable_legacy"

	// CanonicalMessageProjectorVersion is written when the runtime knows the
	// exact transport-visible user text or final assistant text.
	CanonicalMessageProjectorVersion = "canonical-v1"
	// LegacyMessageProjectorVersion identifies conservative projection from a
	// historical content payload whose original transport envelope is absent.
	LegacyMessageProjectorVersion = "legacy-v1"
)

// MessageProjection is the durable, text-only view of one raw chat message.
// VisibleText is non-nil only when Status is ProjectionProjected. An empty
// string is valid: attachment-only dialogue has no searchable text.
type MessageProjection struct {
	VisibleText      *string                 `json:"visible_text,omitempty"`
	Status           MessageProjectionStatus `json:"projection_status"`
	Reason           string                  `json:"projection_reason,omitempty"`
	ProjectorVersion string                  `json:"projector_version"`
}

// ProjectLegacyMessage conservatively derives visible text from one historical
// chat_messages.content payload. It never rewrites the payload and refuses
// provider-expanded reply/file wrappers whose original transport text cannot be
// separated safely.
func ProjectLegacyMessage(role string, content json.RawMessage) MessageProjection {
	role = strings.TrimSpace(role)
	if role != "user" && role != "assistant" {
		return nonDialogueProjection("role_not_dialogue", LegacyMessageProjectorVersion)
	}

	content = bytes.TrimSpace(content)
	if len(content) == 0 || bytes.Equal(content, []byte("null")) {
		return unprojectableLegacy("empty_content")
	}

	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		if containsProviderExpansion(text) {
			return unprojectableLegacy("provider_expanded_text")
		}
		return projectedText(text, LegacyMessageProjectorVersion)
	}

	var blocks []ContentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return unprojectableLegacy("malformed_content")
	}
	return projectLegacyBlocks(blocks)
}

// ProjectMessageForWrite resolves the durable projection stored alongside a
// newly appended raw message. Callers must pass the normalized blocks that are
// written to content in the same transaction.
func ProjectMessageForWrite(msg Message, blocks []ContentBlock) MessageProjection {
	if msg.Role != "user" && msg.Role != "assistant" {
		return nonDialogueProjection("role_not_dialogue", CanonicalMessageProjectorVersion)
	}
	if isToolTranscript(blocks) {
		return nonDialogueProjection("tool_transcript", CanonicalMessageProjectorVersion)
	}
	if msg.VisibleText != nil {
		text := *msg.VisibleText
		if msg.Role == "assistant" {
			text = ProjectAssistantVisibleText(text)
		}
		return projectedText(text, CanonicalMessageProjectorVersion)
	}

	// A final assistant payload is generated inside this runtime, so its text
	// blocks are the visible response rather than a provider-side expansion.
	if msg.Role == "assistant" {
		if text, ok := textOnlyProjection(blocks, true); ok {
			return projectedText(ProjectAssistantVisibleText(text), CanonicalMessageProjectorVersion)
		}
	}

	raw, err := json.Marshal(blocks)
	if err != nil {
		return unprojectableLegacy("marshal_content")
	}
	return ProjectLegacyMessage(msg.Role, raw)
}

// ProjectAssistantVisibleText derives the transport-neutral assistant text
// stored in chat_messages.visible_text. It removes provider control/tool-call
// leakage and attachment dispatch sentinels without mutating the raw content
// payload. Clean text is returned byte-for-byte so authorial whitespace is
// preserved when no transport artifact was present.
func ProjectAssistantVisibleText(text string) string {
	cleaned, artifactsRemoved := stripLeakedAssistantArtifacts(text)
	if attachMarkerRE.MatchString(cleaned) {
		cleaned = attachMarkerRE.ReplaceAllString(cleaned, "")
		return collapseAssistantVisibleLines(cleaned)
	}
	if artifactsRemoved {
		return strings.TrimSpace(cleaned)
	}
	return text
}

// SanitizeLeakedAssistantText preserves the gateway's existing delivery
// sanitizer semantics while sharing the artifact grammar with the durable
// assistant projection.
func SanitizeLeakedAssistantText(text string) string {
	cleaned, _ := stripLeakedAssistantArtifacts(text)
	return strings.TrimSpace(cleaned)
}

func stripLeakedAssistantArtifacts(text string) (string, bool) {
	original := text

	// Some providers occasionally emit a structured call as plain text.
	for {
		idx := strings.Index(text, "call:")
		if idx == -1 {
			break
		}
		end := strings.Index(text[idx:], "}")
		if end == -1 {
			break
		}
		endAbs := idx + end + 1
		for endAbs < len(text) &&
			(text[endAbs] == '|' || text[endAbs] == '>' ||
				text[endAbs] == '<' || text[endAbs] == ' ' ||
				text[endAbs] == '\n') {
			endAbs++
		}
		text = text[:idx] + text[endAbs:]
	}

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
			continue
		}
		text = text[:start] + text[start+end+len("</tool_call>"):]
	}

	for _, tag := range []string{
		"<br>", "<br/>", "<br />",
		"</html>", "<html>", "</body>", "<body>",
	} {
		text = strings.ReplaceAll(text, tag, "")
	}
	for _, token := range []string{
		"<channel|>", "</channel>", "\nthought\n", "\n\nthought\n\n",
	} {
		text = strings.ReplaceAll(text, token, "")
	}
	text = strings.TrimPrefix(text, "thought\n")
	text = strings.TrimPrefix(text, "thought")

	return text, text != original
}

func collapseAssistantVisibleLines(text string) string {
	lines := strings.Split(text, "\n")
	out := lines[:0]
	blank := 0
	for _, line := range lines {
		line = strings.TrimRight(line, " \t\r")
		if line == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func projectLegacyBlocks(blocks []ContentBlock) MessageProjection {
	if len(blocks) == 0 {
		return unprojectableLegacy("empty_content")
	}
	if isToolTranscript(blocks) {
		return nonDialogueProjection("tool_transcript", LegacyMessageProjectorVersion)
	}

	text, ok := textOnlyProjection(blocks, false)
	if !ok {
		return unprojectableLegacy("mixed_or_unknown_content")
	}
	if containsProviderExpansion(text) {
		return unprojectableLegacy("provider_expanded_text")
	}
	return projectedText(text, LegacyMessageProjectorVersion)
}

func textOnlyProjection(blocks []ContentBlock, allowToolUse bool) (string, bool) {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			parts = append(parts, block.Text)
		case "image":
			// Images are represented by attachment references elsewhere. They
			// contribute no text and must never leak base64 into the projection.
		case "tool_use":
			if !allowToolUse {
				return "", false
			}
		case "tool_result":
			return "", false
		default:
			return "", false
		}
	}
	return strings.Join(parts, "\n\n"), true
}

func isToolTranscript(blocks []ContentBlock) bool {
	if len(blocks) == 0 {
		return false
	}
	hasToolUse := false
	hasText := false
	hasImage := false
	for _, block := range blocks {
		switch block.Type {
		case "tool_result":
			// Tool results use role=user on provider wires, but they are never
			// human dialogue even when a provider adapter adds text blocks.
			return true
		case "tool_use":
			hasToolUse = true
		case "text":
			hasText = true
		case "image":
			hasImage = true
		}
	}
	return hasToolUse && !hasText && !hasImage
}

func containsProviderExpansion(text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range []string{
		"[reply to:",
		"[reply-attached:",
		"[ref:",
		"[pdf:",
		"[xlsx:",
		"[docx:",
		"[file:",
		"[video:",
		"[attached:",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func projectedText(text, version string) MessageProjection {
	textCopy := text
	return MessageProjection{
		VisibleText:      &textCopy,
		Status:           ProjectionProjected,
		ProjectorVersion: version,
	}
}

func nonDialogueProjection(reason, version string) MessageProjection {
	return MessageProjection{
		Status:           ProjectionNonDialogue,
		Reason:           reason,
		ProjectorVersion: version,
	}
}

func unprojectableLegacy(reason string) MessageProjection {
	return MessageProjection{
		Status:           ProjectionUnprojectableLegacy,
		Reason:           reason,
		ProjectorVersion: LegacyMessageProjectorVersion,
	}
}
