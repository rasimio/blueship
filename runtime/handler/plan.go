package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/rasimio/blueship/internal/core"
)

// catalogSlugs returns the set of valid skill slugs (from the host catalog) a
// plan patch may reference. Empty when no catalog hook is wired.
func catalogSlugs(ctx context.Context, gw core.GatewayConfig) map[string]bool {
	set := map[string]bool{}
	if gw.ResolveSkillCatalog == nil {
		return set
	}
	cat, err := gw.ResolveSkillCatalog(ctx)
	if err != nil {
		return set
	}
	for _, c := range cat {
		set[c.Slug] = true
	}
	return set
}

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

// --- S2-b: adaptive plan patching --------------------------------------
//
// The executor proposes plan edits in a <<<PLAN_PATCH_JSON …>>> block; the
// handler validates and applies them. The model proposes, the handler owns the
// state — it rejects bad slugs, refuses to mutate done steps, and won't grow
// the plan past the remaining iteration budget.

// PlanPatch is the executor's proposed change to the remaining plan.
type PlanPatch struct {
	CompletedStepID string    `json:"completed_step_id"`
	ResultSummary   string    `json:"result_summary"`
	Operations      []PatchOp `json:"operations"`
}

// PatchOp is one edit: add a step (after another, or appended), remove a
// pending step, or update a pending step's goal/role/acceptance.
type PatchOp struct {
	Op         string    `json:"op"` // add | remove | update
	After      string    `json:"after"`
	ID         string    `json:"id"`
	Step       *RoleStep `json:"step"`
	Goal       *string   `json:"goal"`
	Skills     []string  `json:"skills"`
	Acceptance *string   `json:"acceptance"`
}

// parsePlanPatch extracts a PLAN_PATCH_JSON block from a reply. ok=false means
// the executor proposed no plan change this step.
func parsePlanPatch(reply string) (*PlanPatch, bool) {
	m := planPatchRE.FindStringSubmatch(reply)
	if m == nil {
		return nil, false
	}
	var p PlanPatch
	if err := json.Unmarshal([]byte(m[1]), &p); err != nil {
		return nil, false
	}
	return &p, true
}

func (p *RolePlan) step(id string) *RoleStep {
	for i := range p.Steps {
		if p.Steps[i].ID == id {
			return &p.Steps[i]
		}
	}
	return nil
}

// nextStepID returns an unused step_NNN id (max existing + 1).
func (p *RolePlan) nextStepID() string {
	max := 0
	for i := range p.Steps {
		var n int
		if _, err := fmt.Sscanf(p.Steps[i].ID, "step_%d", &n); err == nil && n > max {
			max = n
		}
	}
	return fmt.Sprintf("step_%03d", max+1)
}

func (p *RolePlan) insertAfter(afterID string, s RoleStep) {
	if afterID == "" {
		p.Steps = append(p.Steps, s)
		return
	}
	for i := range p.Steps {
		if p.Steps[i].ID == afterID {
			p.Steps = append(p.Steps[:i+1], append([]RoleStep{s}, p.Steps[i+1:]...)...)
			return
		}
	}
	p.Steps = append(p.Steps, s) // unknown anchor → append
}

func (p *RolePlan) removeStep(id string) {
	for i := range p.Steps {
		if p.Steps[i].ID == id {
			p.Steps = append(p.Steps[:i], p.Steps[i+1:]...)
			return
		}
	}
}

// applyPatch validates and applies a patch's operations to the remaining plan.
// validSlugs gates skill assignment; remaining caps how many pending steps the
// plan may hold (so the model can't plan past its iteration budget). Returns the
// number of operations actually applied. Invalid ops are skipped + logged, not
// fatal — a bad patch must never wedge the run.
func (p *RolePlan) applyPatch(patch *PlanPatch, validSlugs map[string]bool, remaining int, logger *slog.Logger) int {
	warn := func(msg, id string) {
		if logger != nil {
			logger.Warn("plan patch: op skipped", "reason", msg, "id", id)
		}
	}
	applied := 0
	for _, op := range patch.Operations {
		switch op.Op {
		case "add":
			if op.Step == nil {
				continue
			}
			if p.pendingCount() >= remaining {
				warn("over budget", op.Step.ID)
				continue
			}
			if len(op.Step.Skills) > 0 && !validSlugs[op.Step.Skills[0]] {
				warn("unknown skill", op.Step.Skills[0])
				continue
			}
			ns := *op.Step
			if len(ns.Skills) > 1 {
				ns.Skills = ns.Skills[:1]
			}
			ns.Status = "pending"
			if ns.ID == "" || p.step(ns.ID) != nil {
				ns.ID = p.nextStepID()
			}
			p.insertAfter(op.After, ns)
			applied++
		case "remove":
			s := p.step(op.ID)
			if s == nil || s.Status != "pending" { // never drop a done step
				warn("remove non-pending", op.ID)
				continue
			}
			p.removeStep(op.ID)
			applied++
		case "update":
			s := p.step(op.ID)
			if s == nil || s.Status != "pending" {
				warn("update non-pending", op.ID)
				continue
			}
			if op.Goal != nil {
				s.Goal = *op.Goal
			}
			if op.Acceptance != nil {
				s.Acceptance = *op.Acceptance
			}
			if len(op.Skills) > 0 {
				if validSlugs[op.Skills[0]] {
					s.Skills = op.Skills[:1]
				} else {
					warn("unknown skill", op.Skills[0])
				}
			}
			applied++
		default:
			warn("unknown op", op.Op)
		}
	}
	if applied > 0 {
		p.Rev++
		if next := p.currentStep(); next != nil {
			p.CurrentStepID = next.ID
		} else {
			p.CurrentStepID = ""
		}
	}
	return applied
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
