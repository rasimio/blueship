package core

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// taskReportMarker ends a report notification; the transport replaces it with
// an "open report" button. It never reaches the user or chat history.
const taskReportMarker = "[task_report:"

var taskReportMarkerRE = regexp.MustCompile(`\n?\[task_report:([0-9a-f-]{36})\]\s*$`)

// TaskReportNotification is the single user-facing notice for a finished
// report: a heading, a short summary and the button marker.
func TaskReportNotification(readyFmt, title, body string, id uuid.UUID) string {
	text := fmt.Sprintf(readyFmt, title)
	if summary := taskReportSummary(body); summary != "" {
		text += "\n\n" + summary
	}
	return text + "\n" + taskReportMarker + id.String() + "]"
}

// SplitTaskReportMarker removes the button marker and returns its task.
func SplitTaskReportMarker(text string) (string, uuid.UUID, bool) {
	m := taskReportMarkerRE.FindStringSubmatchIndex(text)
	if m == nil {
		return text, uuid.Nil, false
	}
	id, err := uuid.Parse(text[m[2]:m[3]])
	if err != nil {
		return text, uuid.Nil, false
	}
	return strings.TrimRight(text[:m[0]], " \t\n"), id, true
}

// taskReportSummary keeps the report's opening prose: leading headings (the
// title, "Summary"), tables, code and markers are skipped; the next heading or
// a finished paragraph of reasonable length ends it.
func taskReportSummary(body string) string {
	const limit = 700
	var out []string
	size := 0
	inCode := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "```"):
			inCode = !inCode
			continue
		case inCode, strings.HasPrefix(trimmed, "|"), strings.HasPrefix(trimmed, "[unverified_step_result]"):
			continue
		case strings.HasPrefix(trimmed, "#"):
			if size > 0 {
				return finishSummary(out)
			}
			continue
		case trimmed == "":
			if size >= 200 {
				return finishSummary(out)
			}
			if len(out) > 0 && out[len(out)-1] != "" {
				out = append(out, "")
			}
			continue
		}
		if runes := []rune(trimmed); size+len(runes) > limit {
			if size == 0 {
				out = append(out, strings.TrimSpace(string(runes[:limit]))+"…")
			} else {
				out = append(out, "…")
			}
			return finishSummary(out)
		}
		out = append(out, trimmed)
		size += len([]rune(trimmed))
	}
	return finishSummary(out)
}

func finishSummary(lines []string) string {
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
