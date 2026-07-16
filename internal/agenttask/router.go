package agenttask

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	core "github.com/rasimio/blueship/internal/core"
)

// routeTaskSkill is the "third agent" between task creation and first
// dispatch: a cheap LLM look at the mission picks the execution shape.
// The default background pipeline is research-formed (plan → fetch with
// citation gates → TL;DR synthesis); pushing a courier mission
// («напиши погоду через 15 минут») through it produced six-section
// reports instead of one delivered fact. The router stamps
// config.skills (from the soul's skill catalog) and, for express
// missions, tightens max_iterations — creation stays simple, shaping
// happens server-side, once, before the first iteration.
//
// Conservative by design: any error, malformed verdict, or unknown slug
// leaves the task exactly as created (research default). Runs only for
// unshaped tasks: iteration 0, direct strategy, no explicit skills.
func (s *Scheduler) routeTaskSkill(ctx context.Context, task core.AgentTask) core.AgentTask {
	if task.Iteration != 0 || task.Strategy != core.StrategyDirect {
		return task
	}
	var cfg map[string]json.RawMessage
	if len(task.Config) > 0 {
		if json.Unmarshal(task.Config, &cfg) != nil {
			return task
		}
		if raw, ok := cfg["skills"]; ok && len(raw) > 2 { // "[]" is 2
			return task
		}
		if _, routed := cfg["routed"]; routed {
			return task
		}
	}
	if cfg == nil {
		cfg = map[string]json.RawMessage{}
	}
	gw := s.deps.Config.Gateway
	if gw.ResolveSkillCatalog == nil || s.deps.LLM == nil {
		return task
	}
	catCtx, cancel := context.WithTimeout(core.WithSoulID(ctx, task.SoulID), 5*time.Second)
	defer cancel()
	catalog, err := gw.ResolveSkillCatalog(catCtx)
	if err != nil || len(catalog) == 0 {
		return task
	}
	var sb strings.Builder
	for _, sk := range catalog {
		sb.WriteString("- ")
		sb.WriteString(sk.Slug)
		sb.WriteString(": ")
		sb.WriteString(sk.Description)
		sb.WriteString("\n")
	}
	desc := ""
	if task.Description != nil {
		desc = *task.Description
	}
	system := `You route a freshly created background task to an execution skill. Output strict JSON only: {"skill": "<slug or empty string>", "max_iterations": <int or 0>}.
Pick a skill ONLY when the mission clearly matches its description; empty string keeps the default deep-research pipeline. For quick fetch-and-deliver missions (a fact, a price, the weather, a status check) pick the express/courier-style skill when present and set max_iterations to 2-3. 0 keeps the task's own cap.`
	user := "MISSION TITLE: " + task.Title + "\nMISSION:\n" + desc + "\n\nSKILL CATALOG:\n" + sb.String()
	llmCtx, cancel2 := context.WithTimeout(ctx, 12*time.Second)
	defer cancel2()
	model := s.deps.Config.Models.Primary.ForRouter()
	if model == "" {
		return task
	}
	resp, err := s.deps.LLM.Complete(llmCtx, core.CompletionRequest{
		Model:       model,
		System:      system,
		MaxTokens:   200,
		Temperature: 0.01,
		Messages:    []core.Message{{Role: "user", Content: core.NormalizeContent(user)}},
	})
	if err != nil {
		s.logger.Warn("agent-tasks: skill router failed, keeping defaults", "task_id", task.ID, "error", err)
		return task
	}
	text := strings.TrimSpace(core.ExtractText(resp.Content))
	text = strings.TrimPrefix(text, "```json")
	text = strings.Trim(text, "` \n")
	var verdict struct {
		Skill         string `json:"skill"`
		MaxIterations int    `json:"max_iterations"`
	}
	if json.Unmarshal([]byte(text), &verdict) != nil {
		s.logger.Warn("agent-tasks: skill router malformed verdict, keeping defaults", "task_id", task.ID, "raw", text)
		return task
	}
	verdict.Skill = strings.TrimSpace(verdict.Skill)
	known := verdict.Skill == ""
	for _, sk := range catalog {
		if sk.Slug == verdict.Skill {
			known = true
			break
		}
	}
	if !known {
		s.logger.Warn("agent-tasks: skill router picked unknown slug, keeping defaults", "task_id", task.ID, "slug", verdict.Skill)
		verdict.Skill = ""
	}
	cfg["routed"], _ = json.Marshal(true)
	if verdict.Skill != "" {
		cfg["skills"], _ = json.Marshal([]string{verdict.Skill})
		// Delivery-tagged skills hand their payload to the persona layer:
		// the chat voice stays continuous («будто она непрерывна и
		// неделима») and workers stay persona-free. notifyFn reads the
		// marker the handler emits when this flag is set.
		for _, sk := range catalog {
			if sk.Slug != verdict.Skill {
				continue
			}
			for _, tag := range sk.Tags {
				if tag == "delivery" || tag == "courier" {
					cfg["voice_handoff"], _ = json.Marshal(true)
				}
			}
		}
	}
	merged, err := json.Marshal(cfg)
	if err != nil {
		return task
	}
	task.Config = merged
	if verdict.Skill != "" && verdict.MaxIterations > 0 && verdict.MaxIterations < task.MaxIterations {
		task.MaxIterations = verdict.MaxIterations
	}
	// Delivery-handoff tasks must not be judged on sending: the worker
	// returns the payload and the persona layer delivers it AFTER the
	// task completes. Without this note, creator-written criteria like
	// «сообщение отправлено» dead-lock the courier (it is forbidden to
	// message_send by its skill and failed by acceptance for not doing so).
	if _, voiced := cfg["voice_handoff"]; voiced && task.AcceptanceCriteria != nil {
		amended := *task.AcceptanceCriteria +
			"\n\n[system] Доставка сообщения пользователю выполняется системой ПОСЛЕ завершения задачи. Не требуй от задачи вызова message_send; задача выполнена, когда проверенный payload получен и возвращён результатом."
		task.AcceptanceCriteria = &amended
	}
	if err := s.store.UpdateShaping(ctx, task.ID, task.Config, task.MaxIterations, task.AcceptanceCriteria); err != nil {
		s.logger.Warn("agent-tasks: skill router persist failed", "task_id", task.ID, "error", err)
	}
	s.logger.Info("agent-tasks: routed", "task_id", task.ID, "skill", verdict.Skill, "max_iterations", task.MaxIterations)
	return task
}
