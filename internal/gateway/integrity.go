package gateway

import (
	"context"
	"regexp"
	"strings"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/runtime/agent"
)

// announceRE catches commitment language: the reply CLAIMS a stateful
// action («ставлю на 15:51», «поставила ноту», «создала задачу»). Three
// live incidents in one day had the companion announce a scheduled
// action with zero tool calls in the turn — the user discovers the lie
// when the reminder never fires. Conservative on purpose: verbs of
// creation next to schedulable objects, or the «ставлю на HH:MM» idiom.
var announceRE = regexp.MustCompile(`(?i)(ставлю|поставила|создала|создаю|завела|запустила|запускаю)\s+(на\s+\d{1,2}[:.]\d{2}|нот[ууы]|заметк|задач|напоминани|таймер|agent[_ ]task)|ставлю на \d{1,2}[:.]\d{2}`)

// runCortexIntegrity wraps a cortex RunStream with a deterministic
// post-check: announcement language + zero successful tool calls in the
// turn = the action did NOT happen. One corrective continuation is
// forced — same session, same tools — telling the model to either make
// the call right now or walk the claim back honestly. The corrective
// reply streams to the user through the same callbacks (reads as a
// follow-up message), so the user sees «поставила ✓» or an honest
// retraction instead of silence.
func (g *Gateway) runCortexIntegrity(
	ctx context.Context,
	loop *agent.Loop,
	cfg agent.RunConfig,
	content any,
	cb *bs.StreamCallbacks,
) (string, []agent.ToolTrace, error) {
	reply, traces, err := loop.RunStream(ctx, cfg, content, cb)
	if err != nil || !announceRE.MatchString(reply) {
		return reply, traces, err
	}
	for _, t := range traces {
		if !t.Error {
			return reply, traces, err // some tool actually ran — trust the turn
		}
	}
	claim := announceRE.FindString(reply)
	g.logger.Warn("integrity: action announced without any tool call — forcing correction",
		"session_id", cfg.SessionID, "claim", claim)

	fix := cfg
	fix.SkipUserAppend = true // the nudge is machinery, not conversation
	nudge := "[integrity check] Ты только что написала «" + strings.TrimSpace(claim) +
		"», но не вызвала ни одного инструмента в этом ходу — действие НЕ выполнено, " +
		"пользователь получит пустое обещание. Прямо сейчас выполни его нужным tool call " +
		"(note_create / agent_task_create / …). Если выполнить нельзя — скажи пользователю " +
		"прямо и коротко, что пока не сделала и почему. Не извиняйся многословно."
	fixReply, fixTraces, fixErr := loop.RunStream(ctx, fix, nudge, cb)
	if fixErr != nil {
		g.logger.Warn("integrity: corrective turn failed", "error", fixErr)
		return reply, traces, err
	}
	return reply + "\n" + fixReply, append(traces, fixTraces...), err
}
