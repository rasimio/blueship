package handler

import (
	"encoding/json"
	"fmt"
	"strings"
)

func evalPrecondition(p *PlanPrecondition, ctx map[string]any) string {
	if p == nil {
		return ""
	}
	if len(p.StatusIn) > 0 {
		status, _ := ctx["status"].(string)
		if !stringInSlice(status, p.StatusIn) {
			return fmt.Sprintf("status=%q not in %v", status, p.StatusIn)
		}
	}
	if len(p.AnyPresent) > 0 {
		anyPresent := false
		for _, k := range p.AnyPresent {
			if v, ok := ctx[k]; ok && isPresent(v) {
				anyPresent = true
				break
			}
		}
		if !anyPresent {
			return fmt.Sprintf("none of %v present", p.AnyPresent)
		}
	}
	return ""
}

func stringInSlice(s string, xs []string) bool {
	for _, x := range xs {
		if s == x {
			return true
		}
	}
	return false
}

// isPresent reports whether a JSON-decoded value counts as "present" for
// a precondition check. Empty strings, zero numbers, false bools, nil,
// empty slices/maps all count as absent.
func isPresent(v any) bool {
	switch vv := v.(type) {
	case nil:
		return false
	case string:
		return vv != ""
	case bool:
		return vv
	case float64:
		return vv != 0
	case int:
		return vv != 0
	case []any:
		return len(vv) > 0
	case map[string]any:
		return len(vv) > 0
	}
	return true
}

// formatMetadata renders the top-level scalar fields of a JSON object as a
// compact "Context metadata:\n- key: value\n…" block for the decide prompt.
// Any tool's status payload gets its salient signals surfaced up top, while
// large strings are summarised (not dumped) to keep the truncation budget
// available for the full raw JSON underneath.
//
// Rules for rendering:
//   - strings ≤ 120 chars: printed quoted.
//   - strings > 120 chars: printed as `<key> (string, N chars)`.
//   - bools / numbers / small slices / small maps: printed as-is.
//   - nil values: skipped.
//   - nested objects / large slices: printed as `<key>: <type>`.
func formatMetadata(obj map[string]any) string {
	if len(obj) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Context metadata:\n")
	for k, v := range obj {
		switch vv := v.(type) {
		case nil:
			// skip
		case string:
			if len(vv) == 0 {
				fmt.Fprintf(&b, "- %s: \"\"\n", k)
			} else if len(vv) <= 120 {
				fmt.Fprintf(&b, "- %s: %q\n", k, vv)
			} else {
				fmt.Fprintf(&b, "- %s: (string, %d chars)\n", k, len(vv))
			}
		case bool:
			fmt.Fprintf(&b, "- %s: %t\n", k, vv)
		case float64:
			fmt.Fprintf(&b, "- %s: %v\n", k, vv)
		case int:
			fmt.Fprintf(&b, "- %s: %d\n", k, vv)
		case []any:
			fmt.Fprintf(&b, "- %s: array[%d]\n", k, len(vv))
		case map[string]any:
			fmt.Fprintf(&b, "- %s: object{%d keys}\n", k, len(vv))
		default:
			fmt.Fprintf(&b, "- %s: <%T>\n", k, v)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// substituteVars replaces $peer_task_id and $result.X in input JSON.
func substituteVars(input json.RawMessage, progress *goalPlanProgress) json.RawMessage {
	if len(input) == 0 {
		return input
	}
	s := string(input)
	s = strings.ReplaceAll(s, "$peer_task_id", progress.PeerTaskID)

	// Substitute repo_path variants — LLM writes $result.repo_path, $result.path, etc.
	if progress.RepoPath != "" {
		s = strings.ReplaceAll(s, "$result.repo_path", progress.RepoPath)
		s = strings.ReplaceAll(s, "$result.path", progress.RepoPath)
	}

	// Substitute $result.field references.
	if len(progress.LastResult) > 0 {
		var lastResult map[string]any
		if json.Unmarshal(progress.LastResult, &lastResult) == nil {
			// First pass: exact field match.
			for k, v := range lastResult {
				placeholder := fmt.Sprintf("$result.%s", k)
				if str, ok := v.(string); ok {
					s = strings.ReplaceAll(s, placeholder, str)
				}
			}
			// Second pass: if any $result.* remains unresolved, try to match
			// by suffix (e.g. $result.path → repo_path, $result.id → task_id).
			if strings.Contains(s, "$result.") {
				for k, v := range lastResult {
					str, ok := v.(string)
					if !ok || str == "" {
						continue
					}
					// Check if any unresolved placeholder ends with this key's suffix.
					// E.g. "repo_path" matches "$result.path", "task_id" matches "$result.id".
					for suffix := k; strings.Contains(suffix, "_"); {
						parts := strings.SplitN(suffix, "_", 2)
						suffix = parts[1]
						placeholder := fmt.Sprintf("$result.%s", suffix)
						if strings.Contains(s, placeholder) {
							s = strings.ReplaceAll(s, placeholder, str)
						}
					}
				}
			}
		}
	}

	return json.RawMessage(s)
}

// parsePlan extracts a JSON array of PlanStep from LLM output.
func parsePlan(reply string) ([]PlanStep, error) {
	reply = strings.TrimSpace(reply)

	// Strip markdown code fences.
	if strings.HasPrefix(reply, "```") {
		lines := strings.Split(reply, "\n")
		if len(lines) > 2 {
			reply = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}

	// Try direct parse.
	var steps []PlanStep
	if err := json.Unmarshal([]byte(reply), &steps); err == nil && len(steps) > 0 {
		return steps, nil
	}

	// Try to find JSON array in the reply.
	start := strings.Index(reply, "[")
	end := strings.LastIndex(reply, "]")
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(reply[start:end+1]), &steps); err == nil && len(steps) > 0 {
			return steps, nil
		}
	}

	return nil, fmt.Errorf("no valid JSON plan found in reply (%d chars)", len(reply))
}
