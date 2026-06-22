package agent

import "strings"

// appendTurnText adds one turn's user-facing text to the running reply,
// skipping a turn that merely restates text already accumulated.
//
// Some models re-emit their closing line in the terminal turn after a trailing
// tool call: a heartbeat writes the reminder prose together with
// memory_update(reacted_at), then repeats the identical reminder as the final
// turn. Without this guard the two turns concatenate (joined by a blank line)
// into one doubled message ("…совещание…\n\n…совещание…"). The check is
// substring-based, so a repeat anywhere in the accumulated text is caught, not
// only an immediate one; turns carrying genuinely new text still append.
func appendTurnText(acc *strings.Builder, turnText string) {
	trimmed := strings.TrimSpace(turnText)
	if trimmed == "" || strings.Contains(acc.String(), trimmed) {
		return
	}
	if acc.Len() > 0 {
		acc.WriteString("\n\n")
	}
	acc.WriteString(turnText)
}
