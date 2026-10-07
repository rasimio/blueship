package agenttask

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/rasimio/blueship/internal/core"
)

// Adaptive reasoning shares the output budget with the verdict. A 4096-token
// cap exhausted reasoning twice without any verdict on a complete PC report.
const graphAcceptanceMaxTokens = 8192

// These independent gates share a deadline. A provider error or complete
// negative acceptance verdict cancels and drains the audit; passing acceptance
// still requires the completed audit before the caller can mark success.
func completeGraphReviews(ctx context.Context, deps core.AgentDeps, task core.AgentTask, report string, docs []ToolOutput, request core.CompletionRequest, prior *AcceptanceVerdict) (*core.CompletionResponse, *GroundingVerdict, string, error) {
	if len(docs) == 0 {
		response, err := deps.LLM.Complete(ctx, request)
		return response, nil, "", err
	}
	inputHash := graphGroundingInputHash(deps, task, report, docs)
	if prior != nil && prior.GroundingInputHash == inputHash && prior.Grounding != nil && !prior.Grounding.Unavailable {
		response, err := deps.LLM.Complete(ctx, request)
		return response, prior.Grounding, inputHash, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	audited := make(chan GroundingVerdict, 1)
	go func() {
		if task.ExecutorVersion == 2 && prior != nil && prior.GroundingInputHash == inputHash && prior.Grounding != nil {
			if parts := partitionGroundingReport(report); len(parts) > 1 {
				audited <- evaluateGroundingParts(ctx, deps, task, report, docs, parts, prior.Grounding)
				return
			}
		}
		audited <- evaluateGrounding(ctx, deps, task, report, docs)
	}()
	response, err := deps.LLM.Complete(ctx, request)
	review, valid := parseAcceptanceResponse(response)
	if err != nil || (valid && !review.Met) {
		cancel()
	}
	grounding := <-audited
	return response, &grounding, inputHash, err
}

// Cache only the exact audited input and model policy. A repaired report or
// updated source must be audited again, including previously rejected claims.
func graphGroundingInputHash(deps core.AgentDeps, task core.AgentTask, report string, docs []ToolOutput) string {
	// Include the actual message boundaries and every configured request field.
	// Target text is additionally checked by each persisted partition's hash.
	request, _ := groundingRequest(deps, task, report, docs, "")
	input, _ := json.Marshal(request)
	sum := sha256.Sum256(input)
	return fmt.Sprintf("%x", sum)
}
