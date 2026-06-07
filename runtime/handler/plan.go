package handler

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/rasimio/blueship/internal/core"
)

// RolePlan is the living, role-assigned plan a multi-step task executes against
// — the runtime state the planner builds and the executor walks one step at a
// time. Steps carry STABLE ids (never array indices) so adaptive reordering in
// S2-b can't desync a step from its role. Stored in bgProgress.Plan.
type RolePlan struct {
	Rev           int        `json:"plan_rev"`
	CurrentStepID string     `json:"current_step_id"`
	Steps         []RoleStep `json:"steps"`
}

// RoleStep is one unit of work: a goal, the ONE primary role that owns it, an
// acceptance line, and (after it runs) a short result summary. v1 = one step,
// one goal, one role, one result.
type RoleStep struct {
	ID            string   `json:"id"`
	Goal          string   `json:"goal"`
	Skills        []string `json:"skills,omitempty"` // ≤1 primary skill in v1
	Status        string   `json:"status"`           // pending | done
	Acceptance    string   `json:"acceptance,omitempty"`
	ResultSummary string   `json:"result_summary,omitempty"`
}

// planJSONRE captures the planner's <<<PLAN_JSON {...} >>> block. planPatchRE
// captures the executor's adaptive patch block (consumed in S2-b; stripped from
// user-facing text now so a stray marker never leaks).
var (
	planJSONRE  = regexp.MustCompile(`(?s)<<<PLAN_JSON\s*(\{.*?\})\s*>>>`)
	planPatchRE = regexp.MustCompile(`(?s)<<<PLAN_PATCH_JSON\s*(\{.*?\})\s*>>>`)
)

// parsePlanJSON extracts and validates the planner's plan from a reply. It
// normalises the plan so the handler owns the invariants the model can't be
// trusted to keep: every step gets a stable id, status defaults to pending,
// and v1 caps each step at one skill. ok=false means no usable plan was found.
func parsePlanJSON(reply string) (*RolePlan, bool) {
	m := planJSONRE.FindStringSubmatch(reply)
	if m == nil {
		return nil, false
	}
	var p RolePlan
	if err := json.Unmarshal([]byte(m[1]), &p); err != nil {
		return nil, false
	}
	if len(p.Steps) == 0 {
		return nil, false
	}
	for i := range p.Steps {
		s := &p.Steps[i]
		if strings.TrimSpace(s.ID) == "" {
			s.ID = fmt.Sprintf("step_%03d", i+1)
		}
		if s.Status == "" {
			s.Status = "pending"
		}
		if len(s.Skills) > 1 { // v1: one primary skill per step
			s.Skills = s.Skills[:1]
		}
	}
	p.Rev = 1
	if cur := p.currentStep(); cur != nil {
		p.CurrentStepID = cur.ID
	}
	return &p, true
}

// currentStep returns the step to run now: the CurrentStepID step if it's still
// pending, otherwise the first pending step. nil = plan exhausted.
func (p *RolePlan) currentStep() *RoleStep {
	if p == nil {
		return nil
	}
	if p.CurrentStepID != "" {
		for i := range p.Steps {
			if p.Steps[i].ID == p.CurrentStepID && p.Steps[i].Status == "pending" {
				return &p.Steps[i]
			}
		}
	}
	for i := range p.Steps {
		if p.Steps[i].Status == "pending" {
			return &p.Steps[i]
		}
	}
	return nil
}

// completeStep marks a step done with a one-line result and advances
// CurrentStepID to the next pending step.
func (p *RolePlan) completeStep(id, summary string) {
	for i := range p.Steps {
		if p.Steps[i].ID == id {
			p.Steps[i].Status = "done"
			if summary != "" {
				p.Steps[i].ResultSummary = summary
			}
			break
		}
	}
	if next := p.currentStep(); next != nil {
		p.CurrentStepID = next.ID
	} else {
		p.CurrentStepID = ""
	}
}

// pendingCount reports how many steps remain — used to render budget and to
// decide when the executor is out of work (→ synthesis).
func (p *RolePlan) pendingCount() int {
	n := 0
	for i := range p.Steps {
		if p.Steps[i].Status == "pending" {
			n++
		}
	}
	return n
}

// resultLineRE pulls the executor's "RESULT: …" closing line — the one-sentence
// outcome of a step, used as its result_summary.
var resultLineRE = regexp.MustCompile(`(?im)^\s*RESULT:\s*(.+?)\s*$`)

// extractResultLine returns the step's RESULT line, or a truncated reply if the
// model didn't emit one.
func extractResultLine(reply string) string {
	if m := resultLineRE.FindStringSubmatch(reply); m != nil {
		return strings.TrimSpace(m[1])
	}
	return truncate(stripPlanMarkers(reply), 200)
}

// stripPlanMarkers removes any PLAN_JSON / PLAN_PATCH_JSON blocks from text so
// the planner's machinery never reaches the user (belt-and-suspenders alongside
// the [DONE] cleaning).
func stripPlanMarkers(s string) string {
	s = planJSONRE.ReplaceAllString(s, "")
	s = planPatchRE.ReplaceAllString(s, "")
	return s
}

// formatSkillCatalog renders the role catalog for the planning prompt — slug +
// title + a trimmed when-to-use line, no bodies.
func formatSkillCatalog(cat []core.SkillMeta) string {
	if len(cat) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Available roles (assign AT MOST ONE per step, by slug):\n")
	for _, s := range cat {
		desc := s.Description
		if len(desc) > 220 {
			desc = desc[:220] + "…"
		}
		fmt.Fprintf(&b, "- %s (%s): %s\n", s.Slug, s.Title, desc)
	}
	return b.String()
}

// formatPlanForExecutor renders the plan + the current step for an execution
// iteration so the model sees the whole arc and the one step it owns now.
func formatPlanForExecutor(p *RolePlan, cur *RoleStep) string {
	var b strings.Builder
	b.WriteString("PLAN:\n")
	for i := range p.Steps {
		s := &p.Steps[i]
		mark := "▢"
		if s.Status == "done" {
			mark = "✓"
		} else if cur != nil && s.ID == cur.ID {
			mark = "▸"
		}
		fmt.Fprintf(&b, "%s %s: %s\n", mark, s.ID, s.Goal)
	}
	if cur != nil {
		fmt.Fprintf(&b, "\nYOUR CURRENT STEP — %s: %s", cur.ID, cur.Goal)
		if cur.Acceptance != "" {
			fmt.Fprintf(&b, "\nDone when: %s", cur.Acceptance)
		}
		b.WriteString("\nDo ONLY this step. End your reply with one line — RESULT: <one-sentence outcome>.")
	}
	return b.String()
}
