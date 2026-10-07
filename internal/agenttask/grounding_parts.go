package agenttask

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/rasimio/blueship/internal/core"
)

// Keep complete lines (including table rows) and every original byte. The full
// report is still supplied to every auditor, so cross-section qualifiers and
// calculation inputs remain available. An oversized line is not cut mid-claim.
func partitionGroundingReport(report string) []string {
	const targetRunes = 2500
	var parts []string
	var current strings.Builder
	size := 0
	for _, line := range strings.SplitAfter(report, "\n") {
		n := utf8.RuneCountInString(line)
		if size > 0 && size+n > targetRunes {
			parts = append(parts, current.String())
			current.Reset()
			size = 0
		}
		current.WriteString(line)
		size += n
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

func evaluateGroundingParts(ctx context.Context, deps core.AgentDeps, task core.AgentTask, report string, docs []ToolOutput, parts []string, prior *GroundingVerdict) GroundingVerdict {
	completed := map[string]GroundingVerdict{}
	if prior != nil {
		for _, part := range prior.AuditedParts {
			verdict := strictGraphGrounding(scoreGroundingVerdict(GroundingVerdict{Claims: part.Claims}))
			if !verdict.Unavailable {
				completed[part.TargetHash] = verdict
			}
		}
	}
	results := runGroundingParts(ctx, parts, func(_ int, target string) GroundingVerdict {
		if verdict, ok := completed[groundingTargetHash(target)]; ok {
			return verdict
		}
		return evaluateGroundingPart(ctx, deps, task, report, docs, target)
	})
	combined := combineGroundingParts(results)
	for i, result := range results {
		if !result.Unavailable {
			combined.AuditedParts = append(combined.AuditedParts, GroundingPartAudit{TargetHash: groundingTargetHash(parts[i]), Claims: result.Claims})
		}
	}
	return combined
}

func groundingTargetHash(target string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(target)))
}

func runGroundingParts(ctx context.Context, parts []string, audit func(int, string) GroundingVerdict) []GroundingVerdict {
	results := make([]GroundingVerdict, len(parts))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(3, len(parts)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					results[i] = GroundingVerdict{Unavailable: true, Reason: "partitioned grounding canceled"}
					continue
				}
				results[i] = audit(i, parts[i])
			}
		}()
	}
	for i := range parts {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
	return results
}

func combineGroundingParts(results []GroundingVerdict) GroundingVerdict {
	var combined GroundingVerdict
	for _, result := range results {
		combined.Claims = append(combined.Claims, result.Claims...)
		if result.Unavailable {
			combined.Unavailable = true
			combined.Reason = "partitioned grounding incomplete: " + result.Reason
			combined.RetryDelay = max(combined.RetryDelay, result.RetryDelay)
		}
	}
	if combined.Unavailable {
		return combined
	}
	return strictGraphGrounding(scoreGroundingVerdict(combined))
}
