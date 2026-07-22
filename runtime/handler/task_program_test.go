package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/internal/core"
)

var (
	opaqueDeliveryTokenRE = regexp.MustCompile(`^d_[A-Za-z0-9_-]{22}$`)
	deliveryTokenJSONRE   = regexp.MustCompile(`"_delivery_ref":"(d_[A-Za-z0-9_-]{22})"`)
)

func onlyDeliveryRef(t *testing.T, refs map[string]core.TaskDeliveryRef) (string, core.TaskDeliveryRef) {
	t.Helper()
	if len(refs) != 1 {
		t.Fatalf("DeliveryRefs = %#v, want exactly one", refs)
	}
	for token, ref := range refs {
		return token, ref
	}
	return "", core.TaskDeliveryRef{}
}

func programInput(id, tool, input, onError string) core.TaskProgramInput {
	return core.TaskProgramInput{
		ID:      id,
		Tool:    tool,
		Input:   json.RawMessage(input),
		Enabled: true,
		OnError: onError,
	}
}

func selectedProgram(inputs []core.TaskProgramInput, mode string, inputIDs []string, tools []string) core.TaskProgram {
	return core.TaskProgram{
		Schema: core.TaskProgramSchemaV1,
		Inputs: inputs,
		Activation: core.TaskProgramActivation{
			Mode:     mode,
			InputIDs: inputIDs,
		},
		Decision: core.TaskProgramDecision{
			Mode:  core.TaskProgramDecisionSelected,
			Tools: tools,
		},
	}
}

type fakeTaskDeliveryLedger struct {
	delivered   map[core.TaskDeliveryRef]bool
	lookupErr   error
	lookupCalls int
	marked      []core.TaskDeliveryRef
}

func (f *fakeTaskDeliveryLedger) LookupDelivered(_ context.Context, _ uuid.UUID, refs []core.TaskDeliveryRef) (map[core.TaskDeliveryRef]bool, error) {
	f.lookupCalls++
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	out := make(map[core.TaskDeliveryRef]bool)
	for _, ref := range refs {
		if f.delivered[ref] {
			out[ref] = true
		}
	}
	return out, nil
}

func (f *fakeTaskDeliveryLedger) MarkDelivered(_ context.Context, _ uuid.UUID, refs []core.TaskDeliveryRef) error {
	f.marked = append(f.marked, refs...)
	return nil
}

func TestRunTaskProgramFiltersDeliveredCalendarItemBeforeActivation(t *testing.T) {
	taskID := uuid.New()
	ref := core.TaskDeliveryRef{InputID: "calendar", ItemKey: "calendar:event-1"}
	ledger := &fakeTaskDeliveryLedger{delivered: map[core.TaskDeliveryRef]bool{ref: true}}
	registry := core.NewToolRegistry()
	registry.Register("calendar_upcoming", "", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
		return []any{map[string]any{"item_key": ref.ItemKey, "title": "Standup"}}, nil
	})
	program := selectedProgram(
		[]core.TaskProgramInput{programInput("calendar", "calendar_upcoming", `{}`, core.TaskProgramOnErrorFail)},
		core.TaskProgramActivationAnyNonEmpty,
		[]string{"calendar"},
		nil,
	)

	ctx := core.ContextWithTaskID(context.Background(), taskID)
	execution, err := runTaskProgram(ctx, core.AgentDeps{Registry: registry, Deliveries: ledger}, program, time.Now())
	if err != nil {
		t.Fatalf("runTaskProgram: %v", err)
	}
	if execution.SkipReason != "activation:any_non_empty" {
		t.Fatalf("SkipReason = %q, want delivered item to skip activation", execution.SkipReason)
	}
	if len(execution.DeliveryRefs) != 0 || len(execution.Traces) != 1 || execution.Traces[0].Output != "[]" {
		t.Fatalf("delivered item survived filtering: %#v", execution)
	}
}

func TestRunTaskProgramNewObjectItemReachesPromptAsPending(t *testing.T) {
	taskID := uuid.New()
	ref := core.TaskDeliveryRef{InputID: "notes", ItemKey: "note:slot-1"}
	ledger := &fakeTaskDeliveryLedger{}
	registry := core.NewToolRegistry()
	registry.Register("notes_due", "", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
		return map[string]any{"item_key": ref.ItemKey, "content": "send report"}, nil
	})
	program := selectedProgram(
		[]core.TaskProgramInput{programInput("notes", "notes_due", `{}`, core.TaskProgramOnErrorFail)},
		core.TaskProgramActivationAnyNonEmpty,
		[]string{"notes"},
		nil,
	)

	ctx := core.ContextWithTaskID(context.Background(), taskID)
	execution, err := runTaskProgram(ctx, core.AgentDeps{Registry: registry, Deliveries: ledger}, program, time.Now())
	if err != nil {
		t.Fatalf("runTaskProgram: %v", err)
	}
	if execution.SkipReason != "" || !strings.Contains(execution.PromptBlock, "send report") {
		t.Fatalf("new item did not reach prompt: %#v", execution)
	}
	if strings.Contains(execution.PromptBlock, "item_key") || strings.Contains(execution.PromptBlock, ref.ItemKey) {
		t.Fatalf("runtime item_key leaked into prompt: %s", execution.PromptBlock)
	}
	token, gotRef := onlyDeliveryRef(t, execution.DeliveryRefs)
	if !strings.Contains(execution.PromptBlock, `delivery="all_items"`) ||
		!strings.Contains(execution.PromptBlock, `"_delivery_ref":"`+token+`"`) ||
		!strings.Contains(execution.PromptBlock, "[delivered_items:ref1,ref2]") {
		t.Fatalf("delivery acknowledgement contract missing from prompt: %s", execution.PromptBlock)
	}
	if !opaqueDeliveryTokenRE.MatchString(token) || gotRef != ref {
		t.Fatalf("opaque token/ref = %q/%#v, want safe token and %#v", token, gotRef, ref)
	}
	if len(ledger.marked) != 0 {
		t.Fatalf("handler marked delivery before notify: %#v", ledger.marked)
	}
}

func TestRunTaskProgramDeliveryLookupFailureFailsClosed(t *testing.T) {
	ledger := &fakeTaskDeliveryLedger{lookupErr: fmt.Errorf("database unavailable")}
	registry := core.NewToolRegistry()
	registry.Register("calendar_upcoming", "", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
		return []any{map[string]any{"item_key": "calendar:event-1"}}, nil
	})
	program := selectedProgram(
		[]core.TaskProgramInput{programInput("calendar", "calendar_upcoming", `{}`, core.TaskProgramOnErrorContinue)},
		core.TaskProgramActivationAlways,
		nil,
		nil,
	)
	ctx := core.ContextWithTaskID(context.Background(), uuid.New())

	_, err := runTaskProgram(ctx, core.AgentDeps{Registry: registry, Deliveries: ledger}, program, time.Now())
	if err == nil || !strings.Contains(err.Error(), "lookup delivered items") {
		t.Fatalf("error = %v, want fail-closed delivery lookup error", err)
	}
}

func TestRunTaskProgramDeduplicatesItemKeysWithinOutput(t *testing.T) {
	ledger := &fakeTaskDeliveryLedger{}
	registry := core.NewToolRegistry()
	registry.Register("calendar_upcoming", "", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
		return []any{
			map[string]any{"item_key": "same", "title": "first"},
			map[string]any{"item_key": "same", "title": "duplicate"},
		}, nil
	})
	program := selectedProgram(
		[]core.TaskProgramInput{programInput("calendar", "calendar_upcoming", `{}`, core.TaskProgramOnErrorFail)},
		core.TaskProgramActivationAlways,
		nil,
		nil,
	)
	ctx := core.ContextWithTaskID(context.Background(), uuid.New())

	execution, err := runTaskProgram(ctx, core.AgentDeps{Registry: registry, Deliveries: ledger}, program, time.Now())
	if err != nil {
		t.Fatalf("runTaskProgram: %v", err)
	}
	if len(execution.DeliveryRefs) != 1 || !strings.Contains(execution.PromptBlock, "first") || strings.Contains(execution.PromptBlock, "item_key") || strings.Contains(execution.PromptBlock, "duplicate") {
		t.Fatalf("duplicate keyed items were not collapsed: %#v\n%s", execution.DeliveryRefs, execution.PromptBlock)
	}
}

func TestRunTaskProgramOversizedItemKeyFailsClosed(t *testing.T) {
	ledger := &fakeTaskDeliveryLedger{}
	registry := core.NewToolRegistry()
	registry.Register("notes_due", "", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
		return map[string]any{
			"item_key": strings.Repeat("x", core.TaskDeliveryItemKeyMaxBytes+1),
			"content":  "must not bypass deduplication",
		}, nil
	})
	program := selectedProgram(
		[]core.TaskProgramInput{programInput("notes", "notes_due", `{}`, core.TaskProgramOnErrorContinue)},
		core.TaskProgramActivationAlways,
		nil,
		nil,
	)
	ctx := core.ContextWithTaskID(context.Background(), uuid.New())

	_, err := runTaskProgram(ctx, core.AgentDeps{Registry: registry, Deliveries: ledger}, program, time.Now())
	if err == nil || !strings.Contains(err.Error(), "item_key exceeds 512 bytes") {
		t.Fatalf("error = %v, want oversized item_key fail-closed", err)
	}
	if ledger.lookupCalls != 0 {
		t.Fatalf("oversized key reached ledger lookup: %d calls", ledger.lookupCalls)
	}
}

func TestFilterTaskProgramDeliveriesPreservesLegacyAndProviderNumbers(t *testing.T) {
	legacy := ` [ {"provider_id":9223372036854775807} ] `
	filtered, pending, err := filterTaskProgramDeliveries(context.Background(), nil, "legacy", legacy)
	if err != nil || filtered != legacy || len(pending) != 0 {
		t.Fatalf("unkeyed output changed: filtered=%q pending=%#v err=%v", filtered, pending, err)
	}

	ledger := &fakeTaskDeliveryLedger{}
	keyed := `[{"item_key":"event:1","provider_id":9223372036854775807,"title":"standup"}]`
	ctx := core.ContextWithTaskID(context.Background(), uuid.New())
	filtered, pending, err = filterTaskProgramDeliveries(ctx, ledger, "calendar", keyed)
	if err != nil {
		t.Fatalf("filter keyed output: %v", err)
	}
	if strings.Contains(filtered, "item_key") || !strings.Contains(filtered, "9223372036854775807") || !strings.Contains(filtered, "standup") {
		t.Fatalf("key stripping mutated provider payload: %s", filtered)
	}
	want := core.TaskDeliveryRef{InputID: "calendar", ItemKey: "event:1"}
	if len(pending) != 1 || !opaqueDeliveryTokenRE.MatchString(pending[0].Token) || pending[0].Ref != want {
		t.Fatalf("pending = %#v, want %#v", pending, want)
	}
}

func TestFilterTaskProgramDeliveriesUsesOpaqueUniqueTokens(t *testing.T) {
	ledger := &fakeTaskDeliveryLedger{}
	ctx := core.ContextWithTaskID(context.Background(), uuid.New())
	used := make(map[string]struct{})
	inputID := "sensitive_calendar_input"
	itemKey := "provider-secret-item-key"
	raw := `[{"item_key":"` + itemKey + `","title":"standup"}]`

	firstOutput, first, err := filterTaskProgramDeliveriesWithTokens(ctx, ledger, inputID, raw, used)
	if err != nil || len(first) != 1 {
		t.Fatalf("first filter output=%q pending=%#v err=%v", firstOutput, first, err)
	}
	secondOutput, second, err := filterTaskProgramDeliveriesWithTokens(ctx, ledger, inputID, raw, used)
	if err != nil || len(second) != 1 {
		t.Fatalf("second filter output=%q pending=%#v err=%v", secondOutput, second, err)
	}
	firstToken, secondToken := first[0].Token, second[0].Token
	if !opaqueDeliveryTokenRE.MatchString(firstToken) || !opaqueDeliveryTokenRE.MatchString(secondToken) {
		t.Fatalf("tokens use unsafe format: %q / %q", firstToken, secondToken)
	}
	if firstToken == secondToken {
		t.Fatalf("two delivery filters reused token %q", firstToken)
	}
	for _, token := range []string{firstToken, secondToken} {
		if strings.Contains(token, inputID) || strings.Contains(token, itemKey) {
			t.Fatalf("token %q leaks input id or provider item key", token)
		}
	}
	if !strings.Contains(firstOutput, `"_delivery_ref":"`+firstToken+`"`) ||
		!strings.Contains(secondOutput, `"_delivery_ref":"`+secondToken+`"`) {
		t.Fatalf("opaque refs missing from filtered outputs: %s / %s", firstOutput, secondOutput)
	}
	body, acknowledged, valid := consumeTaskProgramDeliveryAck(
		"Standup is soon.\n[delivered_items:"+firstToken+"]",
		map[string]core.TaskDeliveryRef{firstToken: first[0].Ref},
	)
	if !valid || body != "Standup is soon." || len(acknowledged) != 1 || acknowledged[0] != first[0].Ref {
		t.Fatalf("opaque ACK did not round-trip: valid=%v body=%q refs=%#v", valid, body, acknowledged)
	}
}

func TestFilterTaskProgramDeliveriesSupportsEnvelopeAndRejectsMalformedKey(t *testing.T) {
	ledger := &fakeTaskDeliveryLedger{}
	ctx := core.ContextWithTaskID(context.Background(), uuid.New())
	filtered, pending, err := filterTaskProgramDeliveries(ctx, ledger, "calendar", `{
		"count":2,
		"items":[
			{"item_key":"event:1","title":"one"},
			{"item_key":"event:2","title":"two"}
		]
	}`)
	if err != nil {
		t.Fatalf("filter envelope: %v", err)
	}
	if len(pending) != 2 || !strings.Contains(filtered, `"count":2`) ||
		!strings.Contains(filtered, `"_delivery_ref":"`+pending[0].Token+`"`) ||
		!strings.Contains(filtered, `"_delivery_ref":"`+pending[1].Token+`"`) ||
		pending[0].Token == pending[1].Token ||
		strings.Contains(filtered, "item_key") {
		t.Fatalf("envelope was not delivery-filtered: pending=%#v output=%s", pending, filtered)
	}

	for _, malformed := range []string{
		`[{"item_key":123,"title":"bad"}]`,
		`{"items":[{"item_key":" ","title":"bad"}]}`,
	} {
		if _, _, err := filterTaskProgramDeliveries(ctx, ledger, "calendar", malformed); err == nil ||
			!strings.Contains(err.Error(), "item_key must be a non-empty string") {
			t.Fatalf("malformed key %s did not fail closed: %v", malformed, err)
		}
	}
}

func TestFilterTaskProgramDeliveriesSelectsUniqueKeyedEnvelope(t *testing.T) {
	ledger := &fakeTaskDeliveryLedger{}
	ctx := core.ContextWithTaskID(context.Background(), uuid.New())
	filtered, pending, err := filterTaskProgramDeliveries(ctx, ledger, "calendar", `{
		"items":[],
		"results":[{"item_key":"event:1","title":"standup"}]
	}`)
	if err != nil {
		t.Fatalf("filter unique keyed envelope: %v", err)
	}
	if len(pending) != 1 || pending[0].Ref.ItemKey != "event:1" ||
		!strings.Contains(filtered, `"items":[]`) ||
		!strings.Contains(filtered, `"results":[{"_delivery_ref":"`+pending[0].Token+`","title":"standup"}]`) ||
		strings.Contains(filtered, "item_key") {
		t.Fatalf("keyed results envelope was hidden by empty items: pending=%#v output=%s", pending, filtered)
	}
}

func TestFilterTaskProgramDeliveriesRejectsAmbiguousOrNestedItemKeys(t *testing.T) {
	ledger := &fakeTaskDeliveryLedger{}
	ctx := core.ContextWithTaskID(context.Background(), uuid.New())
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "multiple supported envelopes",
			raw:  `{"items":[{"item_key":"one"}],"results":[{"item_key":"two"}]}`,
			want: "multiple delivery envelopes",
		},
		{
			name: "unsupported envelope",
			raw:  `{"items":[],"other":[{"item_key":"hidden"}]}`,
			want: "outside a supported top-level delivery envelope",
		},
		{
			name: "nested inside supported item",
			raw:  `{"items":[{"metadata":{"item_key":"hidden"}}]}`,
			want: "outside a supported top-level delivery item",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := filterTaskProgramDeliveries(ctx, ledger, "calendar", test.raw); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestFilterTaskProgramDeliveriesRejectsProviderOwnedDeliveryRef(t *testing.T) {
	for _, raw := range []string{
		`[{"item_key":"event:1","_delivery_ref":"d_calendar_1","title":"keyed"}]`,
		`[{"_delivery_ref":"d_calendar_1","title":"unkeyed"}]`,
		`{"items":[{"title":"nested","metadata":{"_delivery_ref":"d_calendar_1"}}]}`,
	} {
		if _, _, err := filterTaskProgramDeliveries(context.Background(), nil, "calendar", raw); err == nil ||
			!strings.Contains(err.Error(), "reserved field _delivery_ref") {
			t.Fatalf("provider control ref %s did not fail closed: %v", raw, err)
		}
	}
}

func TestConsumeTaskProgramDeliveryAckMarksOnlyNamedRefs(t *testing.T) {
	first := core.TaskDeliveryRef{InputID: "calendar", ItemKey: "event:1"}
	second := core.TaskDeliveryRef{InputID: "calendar", ItemKey: "event:2"}
	body, got, valid := consumeTaskProgramDeliveryAck(
		"Only the first meeting is soon.\n[delivered_items:d_calendar_1]",
		map[string]core.TaskDeliveryRef{"d_calendar_1": first, "d_calendar_2": second},
	)
	if !valid || body != "Only the first meeting is soon." || len(got) != 1 || got[0] != first {
		t.Fatalf("valid=%v body=%q refs=%#v, want only first", valid, body, got)
	}
	body, got, valid = consumeTaskProgramDeliveryAck(
		"Invalid exact set\n[delivered_items:d_calendar_1,unknown]",
		map[string]core.TaskDeliveryRef{"d_calendar_1": first},
	)
	if valid || body != "Invalid exact set" || len(got) != 0 {
		t.Fatalf("unknown token must invalidate and strip ack: valid=%v body=%q refs=%#v", valid, body, got)
	}
	body, got, valid = consumeTaskProgramDeliveryAck(
		"Empty exact set\n[delivered_items:]",
		map[string]core.TaskDeliveryRef{"d_calendar_1": first},
	)
	if valid || body != "Empty exact set" || len(got) != 0 {
		t.Fatalf("empty ack must be invalid and stripped: valid=%v body=%q refs=%#v", valid, body, got)
	}
	body, got, valid = consumeTaskProgramDeliveryAck("Plain message without control line", map[string]core.TaskDeliveryRef{"d_calendar_1": first})
	if valid || body != "Plain message without control line" || len(got) != 0 {
		t.Fatalf("missing ack must fail safe: valid=%v body=%q refs=%#v", valid, body, got)
	}
	body, got, valid = consumeTaskProgramDeliveryAck(
		"Duplicate control lines\n[delivered_items:d_calendar_1]\n[delivered_items:d_calendar_1]",
		map[string]core.TaskDeliveryRef{"d_calendar_1": first},
	)
	if valid || len(got) != 0 {
		t.Fatalf("multiple ack lines must fail safe: valid=%v body=%q refs=%#v", valid, body, got)
	}
}

func TestRunTaskProgramAnyNonEmptyAndRepeatedToolOutputs(t *testing.T) {
	registry := core.NewToolRegistry()
	registry.Register("git_status", "", json.RawMessage(`{}`), func(_ context.Context, input json.RawMessage) (any, error) {
		var request struct {
			Repo string `json:"repo"`
		}
		if err := json.Unmarshal(input, &request); err != nil {
			return nil, err
		}
		if request.Repo == "empty" {
			return []any{}, nil
		}
		return []any{map[string]any{"repo": request.Repo}}, nil
	})
	program := selectedProgram(
		[]core.TaskProgramInput{
			programInput("repo_empty", "git_status", `{"repo":"empty"}`, core.TaskProgramOnErrorFail),
			programInput("repo_changed", "git_status", `{"repo":"changed"}`, core.TaskProgramOnErrorFail),
		},
		core.TaskProgramActivationAnyNonEmpty,
		[]string{"repo_empty", "repo_changed"},
		nil,
	)

	execution, err := runTaskProgram(context.Background(), core.AgentDeps{Registry: registry}, program, time.Now())
	if err != nil {
		t.Fatalf("runTaskProgram: %v", err)
	}
	if execution.SkipReason != "" {
		t.Fatalf("SkipReason = %q, want active", execution.SkipReason)
	}
	if execution.ToolOverride == nil || len(execution.ToolOverride) != 0 {
		t.Fatalf("selected+[] must produce a non-nil empty override, got %#v", execution.ToolOverride)
	}
	if len(execution.Traces) != 2 || execution.Traces[0].BlockID != "repo_empty" || execution.Traces[1].BlockID != "repo_changed" {
		t.Fatalf("traces lost block identity: %#v", execution.Traces)
	}
	for _, want := range []string{`[input id="repo_empty" tool="git_status" status="ok"]`, `[input id="repo_changed" tool="git_status" status="ok"]`, `"repo":"changed"`} {
		if !strings.Contains(execution.PromptBlock, want) {
			t.Fatalf("prompt block missing %q:\n%s", want, execution.PromptBlock)
		}
	}
}

func TestRunTaskProgramContinueOnError(t *testing.T) {
	registry := core.NewToolRegistry()
	registry.Register("broken", "", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
		return nil, fmt.Errorf("integration unavailable")
	})
	registry.Register("healthy", "", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
		return []any{"one"}, nil
	})
	program := selectedProgram(
		[]core.TaskProgramInput{
			programInput("optional", "broken", `{}`, core.TaskProgramOnErrorContinue),
			programInput("required", "healthy", `{}`, core.TaskProgramOnErrorFail),
		},
		core.TaskProgramActivationAnyNonEmpty,
		[]string{"optional", "required"},
		nil,
	)

	execution, err := runTaskProgram(context.Background(), core.AgentDeps{Registry: registry}, program, time.Now())
	if err != nil {
		t.Fatalf("continue-on-error stopped program: %v", err)
	}
	if execution.SkipReason != "" || len(execution.Traces) != 2 || !execution.Traces[0].Error || execution.Traces[1].Error {
		t.Fatalf("unexpected execution: %#v", execution)
	}
	if !strings.Contains(execution.PromptBlock, `id="optional" tool="broken" status="error"`) {
		t.Fatalf("continued error missing from prompt block:\n%s", execution.PromptBlock)
	}
}

func TestRunTaskProgramUnavailableContinueInputDoesNotBlockHealthyInput(t *testing.T) {
	registry := core.NewToolRegistry()
	registry.Register("notes_due", "", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
		return []any{map[string]any{"id": "note-1"}}, nil
	})
	program := selectedProgram(
		[]core.TaskProgramInput{
			programInput("calendar", "calendar_upcoming", `{}`, core.TaskProgramOnErrorContinue),
			programInput("notes", "notes_due", `{}`, core.TaskProgramOnErrorFail),
		},
		core.TaskProgramActivationAnyNonEmpty,
		[]string{"calendar", "notes"},
		nil,
	)

	execution, err := runTaskProgram(context.Background(), core.AgentDeps{Registry: registry}, program, time.Now())
	if err != nil {
		t.Fatalf("optional unavailable input stopped program: %v", err)
	}
	if execution.SkipReason != "" || len(execution.Traces) != 2 {
		t.Fatalf("healthy notes did not activate decision: %#v", execution)
	}
	if !execution.Traces[0].Error || execution.Traces[0].BlockID != "calendar" || execution.Traces[1].Error || execution.Traces[1].BlockID != "notes" {
		t.Fatalf("unexpected traces: %#v", execution.Traces)
	}
	if !strings.Contains(execution.PromptBlock, `id="calendar" tool="calendar_upcoming" status="error"`) {
		t.Fatalf("optional unavailable input missing from prompt:\n%s", execution.PromptBlock)
	}
}

func TestRunTaskProgramPreflightPreventsPartialSideEffects(t *testing.T) {
	registry := core.NewToolRegistry()
	calls := 0
	registry.Register("healthy", "", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
		calls++
		return []any{"ok"}, nil
	})

	t.Run("required input unavailable", func(t *testing.T) {
		calls = 0
		program := selectedProgram(
			[]core.TaskProgramInput{
				programInput("first", "healthy", `{}`, core.TaskProgramOnErrorFail),
				programInput("required", "missing", `{}`, core.TaskProgramOnErrorFail),
			},
			core.TaskProgramActivationAlways,
			nil,
			nil,
		)
		if _, err := runTaskProgram(context.Background(), core.AgentDeps{Registry: registry}, program, time.Now()); err == nil {
			t.Fatal("missing required input should fail preflight")
		}
		if calls != 0 {
			t.Fatalf("healthy input ran before required preflight failure: %d calls", calls)
		}
	})

	t.Run("decision tool unavailable", func(t *testing.T) {
		calls = 0
		program := selectedProgram(
			[]core.TaskProgramInput{programInput("first", "healthy", `{}`, core.TaskProgramOnErrorFail)},
			core.TaskProgramActivationAlways,
			nil,
			[]string{"missing_decision"},
		)
		if _, err := runTaskProgram(context.Background(), core.AgentDeps{Registry: registry}, program, time.Now()); err == nil {
			t.Fatal("missing decision tool should fail preflight")
		}
		if calls != 0 {
			t.Fatalf("input ran before decision preflight failure: %d calls", calls)
		}
	})
}

func TestRunTaskProgramQuietHoursBeforeInputs(t *testing.T) {
	calls := 0
	registry := core.NewToolRegistry()
	registry.Register("expensive", "", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
		calls++
		return []any{"data"}, nil
	})
	program := selectedProgram(
		[]core.TaskProgramInput{programInput("expensive_check", "expensive", `{}`, core.TaskProgramOnErrorFail)},
		core.TaskProgramActivationAlways,
		nil,
		nil,
	)
	program.QuietHours = &core.TaskProgramQuietHours{FromHour: 22, ToHour: 7}

	execution, err := runTaskProgram(context.Background(), core.AgentDeps{Registry: registry}, program, time.Date(2026, 7, 22, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("runTaskProgram: %v", err)
	}
	if calls != 0 || execution.SkipReason != "quiet_hours:23" || len(execution.Traces) != 0 {
		t.Fatalf("quiet hours executed input: calls=%d execution=%#v", calls, execution)
	}
}

func TestRunTaskProgramRejectsOversizedOutputThroughOnErrorPolicy(t *testing.T) {
	registry := core.NewToolRegistry()
	registry.Register("huge", "", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
		return strings.Repeat("x", taskProgramMaxOutputBytes+1), nil
	})
	program := selectedProgram(
		[]core.TaskProgramInput{programInput("huge_result", "huge", `{}`, core.TaskProgramOnErrorContinue)},
		core.TaskProgramActivationAlways,
		nil,
		nil,
	)

	execution, err := runTaskProgram(context.Background(), core.AgentDeps{Registry: registry}, program, time.Now())
	if err != nil {
		t.Fatalf("continue policy must absorb size error: %v", err)
	}
	if len(execution.Traces) != 1 || !execution.Traces[0].Error || !strings.Contains(execution.Traces[0].Output, "exceeds task_program limit") {
		t.Fatalf("oversized output was not converted to an input error: %#v", execution.Traces)
	}
	if len(execution.PromptBlock) > 2*1024 {
		t.Fatalf("oversized tool output leaked into prompt: %d bytes", len(execution.PromptBlock))
	}
}

func TestRunTaskProgramAggregateOutputLimitFailsClosed(t *testing.T) {
	registry := core.NewToolRegistry()
	registry.Register("large_read", "", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
		return strings.Repeat("x", 60<<10), nil
	})
	inputs := make([]core.TaskProgramInput, 5)
	for i := range inputs {
		inputs[i] = programInput(fmt.Sprintf("large_%d", i), "large_read", `{}`, core.TaskProgramOnErrorContinue)
	}
	program := selectedProgram(inputs, core.TaskProgramActivationAlways, nil, nil)

	execution, err := runTaskProgram(context.Background(), core.AgentDeps{Registry: registry}, program, time.Now())
	if err == nil || !strings.Contains(err.Error(), "aggregate output exceeds") {
		t.Fatalf("error = %v, want aggregate output failure", err)
	}
	if len(execution.Traces) != len(inputs) || !execution.Traces[len(execution.Traces)-1].Error ||
		!strings.Contains(execution.Traces[len(execution.Traces)-1].Output, "aggregate output exceeds") {
		t.Fatalf("aggregate failure was not bounded/audited: %#v", execution.Traces)
	}
}

func TestRunTaskProgramUnknownConfiguredToolIsExplicit(t *testing.T) {
	program := selectedProgram(
		[]core.TaskProgramInput{programInput("missing_block", "missing_tool", `{}`, core.TaskProgramOnErrorFail)},
		core.TaskProgramActivationAlways,
		nil,
		nil,
	)
	_, err := runTaskProgram(context.Background(), core.AgentDeps{Registry: core.NewToolRegistry()}, program, time.Now())
	if err == nil || !strings.Contains(err.Error(), `input "missing_block": tool "missing_tool" is unavailable`) {
		t.Fatalf("error = %v, want explicit unavailable tool", err)
	}
}

func TestBackgroundRunTaskProgramUsesSelectedDecisionTools(t *testing.T) {
	provider := &capturingProvider{}
	provider.respond = func(request core.CompletionRequest) (*core.CompletionResponse, error) {
		for _, message := range request.Messages {
			text := core.ExtractText(core.NormalizeContent(message.Content))
			if match := deliveryTokenJSONRE.FindStringSubmatch(text); match != nil {
				return &core.CompletionResponse{
					StopReason: "end_turn",
					Content: []core.ContentBlock{{
						Type: "text",
						Text: "[DONE] handled\n[delivered_items:" + match[1] + "]",
					}},
				}, nil
			}
		}
		return nil, fmt.Errorf("task-program delivery token missing from completion request")
	}
	deps, store := backgroundTestDeps(provider, map[string]string{})
	deliveryRef := core.TaskDeliveryRef{InputID: "notes_due", ItemKey: "note:slot-1"}
	unacknowledgedRef := core.TaskDeliveryRef{InputID: "notes_due", ItemKey: "note:slot-2"}
	deps.Deliveries = &fakeTaskDeliveryLedger{}
	deps.Registry.Register("notes_list", "", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
		return []any{
			map[string]any{"id": "n1", "item_key": deliveryRef.ItemKey},
			map[string]any{"id": "n2", "item_key": unacknowledgedRef.ItemKey},
		}, nil
	})
	deps.Registry.Register("safe_lookup", "", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
		return map[string]any{"ok": true}, nil
	})
	deps.Registry.Register("dangerous", "", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
		return nil, nil
	})
	program := selectedProgram(
		[]core.TaskProgramInput{programInput("notes_due", "notes_list", `{}`, core.TaskProgramOnErrorFail)},
		core.TaskProgramActivationAnyNonEmpty,
		[]string{"notes_due"},
		[]string{"safe_lookup"},
	)
	config, _ := json.Marshal(map[string]any{
		"prompt":       "check due notes",
		"skip_reflex":  true,
		"task_program": program,
	})
	task := core.AgentTask{
		ID:            uuid.New(),
		UserID:        uuid.New(),
		Title:         "heartbeat",
		Strategy:      core.StrategyRecurring,
		Config:        config,
		MaxIterations: 1,
	}

	result, err := NewBackground(time.UTC, nil, nil, nil).Run(core.ContextWithTaskID(context.Background(), task.ID), task, deps)
	if err != nil {
		t.Fatalf("Background.Run: %v", err)
	}
	if len(result.PendingDeliveries) != 1 || result.PendingDeliveries[0] != deliveryRef {
		t.Fatalf("iteration pending deliveries = %#v, want %#v", result.PendingDeliveries, deliveryRef)
	}
	if result.Notify != "handled" || result.Output != "handled" || strings.Contains(result.Notify, "delivered_items") {
		t.Fatalf("exact ack leaked or changed notification: output=%q notify=%q", result.Output, result.Notify)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("LLM requests = %d, want 1", len(provider.requests))
	}
	tools := provider.requests[0].Tools
	if len(tools) != 1 || tools[0].Name != "safe_lookup" {
		t.Fatalf("LLM tools = %#v, want only safe_lookup", tools)
	}
	if len(store.appended) == 0 {
		t.Fatal("no program-backed user message was stored")
	}
	message, _ := store.appended[0].Content.(string)
	for _, want := range []string{"[task_program_inputs]", `id="notes_due" tool="notes_list" status="ok"`, "untrusted data"} {
		if !strings.Contains(message, want) {
			t.Fatalf("background message missing %q:\n%s", want, message)
		}
	}
}

func TestBackgroundTaskProgramDeliveryAckFallbackAndFailClosedCases(t *testing.T) {
	tests := []struct {
		name          string
		reply         string
		maxIterations int
		itemCount     int
		wantErr       string
		wantDone      bool
		wantNotify    string
		wantPending   int
	}{
		{
			name:        "sole missing ack auto-acks",
			reply:       "[DONE] Standup starts in ten minutes.",
			wantDone:    true,
			wantNotify:  "Standup starts in ten minutes.",
			wantPending: 1,
		},
		{
			name:    "unknown token",
			reply:   "[DONE] Standup starts in ten minutes.\n[delivered_items:guessed-token]",
			wantErr: "requires a valid [delivered_items:...] acknowledgment",
		},
		{
			name:    "empty explicit ack",
			reply:   "[DONE] Standup starts in ten minutes.\n[delivered_items:]",
			wantErr: "requires a valid [delivered_items:...] acknowledgment",
		},
		{
			name:    "malformed unclosed ack",
			reply:   "[DONE] Standup starts in ten minutes.\n[delivered_items:broken",
			wantErr: "requires a valid [delivered_items:...] acknowledgment",
		},
		{
			name:    "duplicate ack lines",
			reply:   "[DONE] Standup starts in ten minutes.\n[delivered_items:none]\n[delivered_items:none]",
			wantErr: "requires a valid [delivered_items:...] acknowledgment",
		},
		{
			name:    "explicit none with message",
			reply:   "[DONE] Standup starts in ten minutes.\n[delivered_items:none]",
			wantErr: "must acknowledge at least one represented item",
		},
		{
			name:      "multiple refs missing ack",
			reply:     "[DONE] Two events are soon.",
			itemCount: 2,
			wantErr:   "requires a valid [delivered_items:...] acknowledgment",
		},
		{
			name:     "no-op needs no ack",
			reply:    "[DONE] [no-op]",
			wantDone: true,
		},
		{
			name:          "non-final milestone is suppressed",
			reply:         "[MILESTONE] Standup starts in ten minutes.",
			maxIterations: 2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &capturingProvider{responses: []*core.CompletionResponse{{
				StopReason: "end_turn",
				Content:    []core.ContentBlock{{Type: "text", Text: test.reply}},
			}}}
			deps, _ := backgroundTestDeps(provider, map[string]string{})
			deps.Deliveries = &fakeTaskDeliveryLedger{}
			deps.Registry.Register("calendar_upcoming", "", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
				itemCount := test.itemCount
				if itemCount == 0 {
					itemCount = 1
				}
				items := make([]any, 0, itemCount)
				for i := 1; i <= itemCount; i++ {
					items = append(items, map[string]any{
						"item_key": fmt.Sprintf("event:%d", i),
						"title":    fmt.Sprintf("Event %d", i),
					})
				}
				return items, nil
			})
			program := selectedProgram(
				[]core.TaskProgramInput{programInput("calendar", "calendar_upcoming", `{}`, core.TaskProgramOnErrorFail)},
				core.TaskProgramActivationAnyNonEmpty,
				[]string{"calendar"},
				nil,
			)
			config, _ := json.Marshal(map[string]any{"task_program": program})
			maxIterations := test.maxIterations
			if maxIterations == 0 {
				maxIterations = 1
			}
			task := core.AgentTask{
				ID:            uuid.New(),
				UserID:        uuid.New(),
				Title:         "heartbeat",
				Strategy:      core.StrategyRecurring,
				Config:        config,
				MaxIterations: maxIterations,
			}

			result, err := NewBackground(time.UTC, nil, nil, nil).Run(
				core.ContextWithTaskID(context.Background(), task.ID), task, deps,
			)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Background.Run error = %v, want %q", err, test.wantErr)
				}
				if result.Notify != "" || len(result.PendingDeliveries) != 0 {
					t.Fatalf("invalid ACK escaped as notification: %#v", result)
				}
				return
			}
			if err != nil || result.Done != test.wantDone || result.Notify != test.wantNotify || len(result.PendingDeliveries) != test.wantPending {
				t.Fatalf("result=%#v err=%v, want done=%t notify=%q pending=%d",
					result, err, test.wantDone, test.wantNotify, test.wantPending)
			}
			if test.wantPending == 1 {
				wantRef := core.TaskDeliveryRef{InputID: "calendar", ItemKey: "event:1"}
				if result.PendingDeliveries[0] != wantRef {
					t.Fatalf("PendingDeliveries[0] = %#v, want %#v", result.PendingDeliveries[0], wantRef)
				}
			}
		})
	}
}

func TestBackgroundTaskProgramComposesTrustedInstructionAndSkipsHiddenReflexPipeline(t *testing.T) {
	provider := &capturingProvider{responses: []*core.CompletionResponse{{
		StopReason: "end_turn",
		Content:    []core.ContentBlock{{Type: "text", Text: "[DONE] visible result"}},
	}}}
	deps, store := backgroundTestDeps(provider, map[string]string{"heartbeat-base": "BASE SAFETY AND PERSONA TEMPLATE"})
	reflexCalls := 0
	contextCalls := 0
	deps.ReflexPreparer = func(context.Context, string, string, string) *core.ReflexContext {
		reflexCalls++
		return &core.ReflexContext{FullContext: "hidden AME context"}
	}
	deps.ContextInjector = func(context.Context, string, string, string) string {
		contextCalls++
		return "hidden fallback context"
	}
	deps.Registry.Register("visual_source", "", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
		return map[string]any{"payload": "UNTRUSTED PROVIDER DATA"}, nil
	})
	program := selectedProgram(
		[]core.TaskProgramInput{programInput("source", "visual_source", `{}`, core.TaskProgramOnErrorFail)},
		core.TaskProgramActivationAnyNonEmpty,
		[]string{"source"},
		nil,
	)
	program.Decision.Instruction = "TRUSTED VISUAL DECISION INSTRUCTION"
	config, _ := json.Marshal(map[string]any{
		"prompt":       "heartbeat-base",
		"skip_reflex":  false,
		"task_program": program,
	})
	task := core.AgentTask{
		ID:            uuid.New(),
		UserID:        uuid.New(),
		Title:         "hermetic program",
		Strategy:      core.StrategyRecurring,
		Config:        config,
		MaxIterations: 1,
	}

	result, err := NewBackground(time.UTC, nil, nil, nil).Run(context.Background(), task, deps)
	if err != nil {
		t.Fatalf("Background.Run: %v", err)
	}
	if reflexCalls != 0 || contextCalls != 0 {
		t.Fatalf("task_program entered hidden reflex pipeline: reflex=%d context=%d", reflexCalls, contextCalls)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("LLM requests = %d, want 1", len(provider.requests))
	}
	system := provider.requests[0].System
	for _, want := range []string{
		"BASE SAFETY AND PERSONA TEMPLATE",
		`[task_program_decision_instruction trusted="true"]`,
		taskProgramDecisionInstructionFrame,
		"TRUSTED VISUAL DECISION INSTRUCTION",
		"[/task_program_decision_instruction]",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("system prompt missing %q:\n%s", want, system)
		}
	}
	if strings.Contains(system, "UNTRUSTED PROVIDER DATA") {
		t.Fatalf("provider output leaked into trusted system prompt:\n%s", system)
	}
	if len(store.appended) == 0 {
		t.Fatal("no task-program user message was stored")
	}
	message, _ := store.appended[0].Content.(string)
	for _, want := range []string{"[task_program_inputs]", "untrusted data", "UNTRUSTED PROVIDER DATA"} {
		if !strings.Contains(message, want) {
			t.Fatalf("untrusted user block missing %q:\n%s", want, message)
		}
	}
	if strings.Contains(message, "TRUSTED VISUAL DECISION INSTRUCTION") {
		t.Fatalf("trusted instruction leaked into untrusted user block:\n%s", message)
	}
	if result.Notify != "visible result" {
		t.Fatalf("result notify = %q, want visible result", result.Notify)
	}
}

func TestBackgroundRunTaskProgramActivationSkipsLLM(t *testing.T) {
	provider := &capturingProvider{}
	deps, _ := backgroundTestDeps(provider, map[string]string{})
	deps.Registry.Register("notes_list", "", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
		return []any{}, nil
	})
	program := selectedProgram(
		[]core.TaskProgramInput{programInput("notes_due", "notes_list", `{}`, core.TaskProgramOnErrorFail)},
		core.TaskProgramActivationAnyNonEmpty,
		[]string{"notes_due"},
		nil,
	)
	config, _ := json.Marshal(map[string]any{"task_program": program})
	task := core.AgentTask{ID: uuid.New(), UserID: uuid.New(), Strategy: core.StrategyRecurring, Config: config}

	result, err := NewBackground(time.UTC, nil, nil, nil).Run(context.Background(), task, deps)
	if err != nil {
		t.Fatalf("Background.Run: %v", err)
	}
	if len(provider.requests) != 0 || !result.Done || result.Output != "" {
		t.Fatalf("activation did not skip LLM cleanly: requests=%d result=%#v", len(provider.requests), result)
	}
}

func TestBackgroundRunTaskProgramTakesPrecedenceOverLegacyPrefetch(t *testing.T) {
	provider := &capturingProvider{}
	deps, _ := backgroundTestDeps(provider, map[string]string{})
	legacyCalls := 0
	deps.Registry.Register("legacy_prefetch", "", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
		legacyCalls++
		return []any{"legacy"}, nil
	})
	program := selectedProgram(nil, core.TaskProgramActivationAlways, nil, nil)
	program.Decision.Mode = core.TaskProgramDecisionNone
	config, _ := json.Marshal(map[string]any{
		"task_program": program,
		"backend_prefetch": map[string]any{
			"tools": []map[string]any{{"name": "legacy_prefetch", "input": map[string]any{}}},
		},
	})
	task := core.AgentTask{ID: uuid.New(), UserID: uuid.New(), Strategy: core.StrategyRecurring, Config: config}

	result, err := NewBackground(time.UTC, nil, nil, nil).Run(context.Background(), task, deps)
	if err != nil {
		t.Fatalf("Background.Run: %v", err)
	}
	if legacyCalls != 0 || len(provider.requests) != 0 || !result.Done {
		t.Fatalf("legacy branch ran alongside task_program: calls=%d requests=%d result=%#v", legacyCalls, len(provider.requests), result)
	}
}

func TestBackgroundRunLegacyPrefetchStillWorksWithoutProgram(t *testing.T) {
	provider := &capturingProvider{}
	deps, _ := backgroundTestDeps(provider, map[string]string{})
	calls := 0
	deps.Registry.Register("legacy_prefetch", "", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
		calls++
		return []any{"legacy"}, nil
	})
	task := core.AgentTask{
		ID:            uuid.New(),
		UserID:        uuid.New(),
		Strategy:      core.StrategyRecurring,
		MaxIterations: 1,
		Config: json.RawMessage(`{
			"prompt":"legacy heartbeat",
			"skip_reflex":true,
			"backend_prefetch":{"tools":[{"name":"legacy_prefetch","input":{}}],"disable_llm_tools":true}
		}`),
	}

	if _, err := NewBackground(time.UTC, nil, nil, nil).Run(context.Background(), task, deps); err != nil {
		t.Fatalf("Background.Run: %v", err)
	}
	if calls != 1 || len(provider.requests) != 1 {
		t.Fatalf("legacy prefetch calls=%d LLM requests=%d, want 1/1", calls, len(provider.requests))
	}
	if len(provider.requests[0].Tools) != 0 {
		t.Fatalf("legacy disable_llm_tools ignored: %v", provider.requests[0].Tools)
	}
}
