package agenttask

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rasimio/blueship/internal/core"
)

func TestGroundingPartitionPreservesEveryByte(t *testing.T) {
	for _, report := range []string{"", "short\n", strings.Repeat("строка 日本語\n", 1000), strings.Repeat("я", 6000) + "\nend", strings.Repeat("a\n", 1250) + "last"} {
		parts := partitionGroundingReport(report)
		if strings.Join(parts, "") != report {
			t.Fatal("report bytes lost")
		}
		for _, part := range parts {
			if !utf8.ValidString(part) || part == "" {
				t.Fatal("broken partition")
			}
			if utf8.RuneCountInString(part) > 2500 && strings.Count(strings.TrimSuffix(part, "\n"), "\n") > 0 {
				t.Fatal("oversized multi-line partition")
			}
		}
	}
}

type partitionAuditProvider struct {
	active, peak, started atomic.Int32
	ready                 chan struct{}
	mutex                 sync.Mutex
	targets               []string
	report, mode          string
}

func (p *partitionAuditProvider) Complete(ctx context.Context, req core.CompletionRequest) (*core.CompletionResponse, error) {
	active := p.active.Add(1)
	defer p.active.Add(-1)
	for {
		old := p.peak.Load()
		if active <= old || p.peak.CompareAndSwap(old, active) {
			break
		}
	}
	n := p.started.Add(1)
	if n == 3 {
		close(p.ready)
	}
	select {
	case <-p.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if len(req.Messages) != 2 {
		return nil, context.Canceled
	}
	sources := core.ExtractText(core.NormalizeContent(req.Messages[0].Content))
	text := core.ExtractText(core.NormalizeContent(req.Messages[1].Content))
	_, target, ok := strings.Cut(text, "\n\n[audit_target]\n")
	if !ok || !strings.HasPrefix(text, "[report]\n"+p.report) || !strings.Contains(sources, "SOURCE_EVIDENCE") {
		return nil, context.Canceled
	}
	p.mutex.Lock()
	p.targets = append(p.targets, target)
	p.mutex.Unlock()
	if n == 2 && p.mode == "truncated" {
		return &core.CompletionResponse{StopReason: "max_tokens"}, nil
	}
	status := "grounded"
	if n == 2 && p.mode == "unsupported" {
		status = "partial"
	}
	body, _ := json.Marshal(map[string]any{"claims": []ClaimGrounding{{Claim: target[:1], ClaimType: "numerical", Status: status}}})
	return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: string(body)}}}, nil
}
func TestGroundingPartitionsBoundConcurrencyAndRequireEveryAudit(t *testing.T) {
	report := ""
	for _, id := range []string{"A", "B", "C", "D", "E", "F"} {
		report += id + strings.Repeat("я", 2000) + "\n"
	}
	for _, mode := range []string{"accepted", "unsupported", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			p := &partitionAuditProvider{report: report, mode: mode, ready: make(chan struct{})}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result := evaluateGrounding(ctx, evaluatorTestDeps(p, nil), core.AgentTask{ExecutorVersion: 2}, report, []ToolOutput{{Output: "SOURCE_EVIDENCE"}})
			if p.peak.Load() != 3 || p.started.Load() != 6 {
				t.Fatal("incorrect concurrency or coverage", p.peak.Load(), p.started.Load())
			}
			seen := map[string]int{}
			for _, target := range p.targets {
				seen[target]++
			}
			for _, part := range partitionGroundingReport(report) {
				if seen[part] != 1 {
					t.Fatal("target missing or repeated")
				}
			}
			if result.Met != (mode == "accepted") || result.Unavailable != (mode == "truncated") {
				t.Fatal("invalid aggregate", result)
			}
			if p.active.Load() != 0 {
				t.Fatal("audit leaked")
			}
		})
	}
}

func TestGroundingPartitionsCancelQueuedWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	results := runGroundingParts(ctx, []string{"one", "two", "three", "four"}, func(_ int, _ string) GroundingVerdict {
		calls.Add(1)
		return GroundingVerdict{Met: true}
	})
	if calls.Load() != 0 || len(results) != 4 {
		t.Fatal("canceled work reached provider", calls.Load(), len(results))
	}
	for _, result := range results {
		if !result.Unavailable {
			t.Fatal("canceled partition silently passed")
		}
	}
	if result := combineGroundingParts(results); result.Met || !result.Unavailable {
		t.Fatal("canceled aggregate accepted", result)
	}
}
