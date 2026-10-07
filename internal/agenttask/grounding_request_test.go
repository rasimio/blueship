package agenttask

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rasimio/blueship/internal/core"
)

func groundingMessageText(message core.Message) string {
	return core.ExtractText(core.NormalizeContent(message.Content))
}

func TestGroundingSourceMessagePreservesEvidenceAndFullReport(t *testing.T) {
	deps := evaluatorTestDeps(&cachedGateProvider{}, nil)
	report := "A claim https://example.test/source\nA qualification elsewhere. 日本語\n"
	doc := fetchRow("https://example.test/source", "Document", "SOURCE_EVIDENCE", 0, 15)
	doc.Metadata["observed_at"] = "2026-09-22T22:01:17.123+03:00"
	doc.Metadata["from_cache"] = true
	docs := []ToolOutput{doc}
	legacy, stats := buildGroundingUserMessage(report, docs)
	sources := strings.TrimPrefix(legacy, "[report]\n"+report+"\n\n")
	for _, version := range []int{1, 2} {
		for _, target := range []string{"", "A claim https://example.test/source\n"} {
			req, actualStats := groundingRequest(deps, core.AgentTask{ExecutorVersion: version}, report, docs, target)
			if actualStats != stats {
				t.Fatal("evidence selection changed", actualStats, stats)
			}
			wantReport := "[report]\n" + report
			if version == 1 {
				wantReport = legacy
				if len(req.Messages) != 1 {
					t.Fatal("legacy message layout changed")
				}
			} else if len(req.Messages) != 2 || groundingMessageText(req.Messages[0]) != sources {
				t.Fatal("source content changed at cache boundary")
			}
			if target != "" {
				wantReport += "\n\n[audit_target]\n" + target
			}
			if groundingMessageText(req.Messages[len(req.Messages)-1]) != wantReport {
				t.Fatal("auditor lost full report or exact target")
			}
			for _, message := range req.Messages {
				if message.Role != "user" {
					t.Fatal("source content elevated to instructions")
				}
			}
		}
	}
	if !strings.Contains(sources, "[runtime_observation] observed_at=2026-09-22T19:01:17.123Z\nSOURCE_EVIDENCE") {
		t.Fatal("original observation metadata omitted", sources)
	}
}

func TestGroundingSourceCacheBoundarySurvivesOnlyUnchangedCorpus(t *testing.T) {
	deps := evaluatorTestDeps(&cachedGateProvider{}, nil)
	task := core.AgentTask{ExecutorVersion: 2}
	docs := []ToolOutput{
		fetchRow("https://example.test/a", "A", "Source A", 0, 8),
		fetchRow("https://example.test/b", "B", "Source B", 0, 8),
	}
	report := "Claim https://example.test/a"
	original, _ := groundingRequest(deps, task, report, docs, "Claim")
	repaired, _ := groundingRequest(deps, task, report+"; corrected qualification", docs, "qualification")
	if !reflect.DeepEqual(original.Messages[0], repaired.Messages[0]) || reflect.DeepEqual(original.Messages[1], repaired.Messages[1]) {
		t.Fatal("report-only repair changed corpus or failed to change audit scope")
	}
	changedCitation, _ := groundingRequest(deps, task, "Claim https://example.test/b", docs, "")
	if reflect.DeepEqual(original.Messages[0], changedCitation.Messages[0]) {
		t.Fatal("report-dependent evidence selection did not update corpus")
	}
	docs[0].Output += " changed"
	changedSource, _ := groundingRequest(deps, task, report, docs, "")
	if reflect.DeepEqual(original.Messages[0], changedSource.Messages[0]) {
		t.Fatal("changed evidence reused source prefix")
	}
	if graphGroundingInputHash(deps, task, report, docs) == graphGroundingInputHash(deps, core.AgentTask{ExecutorVersion: 1}, report, docs) {
		t.Fatal("request layout and policy absent from persisted verdict identity")
	}
}

func TestGroundingReplayPartitionsPreserveBothCapturedLayouts(t *testing.T) {
	deps := evaluatorTestDeps(&cachedGateProvider{}, nil)
	report := "A" + strings.Repeat("a", 2000) + "\nB" + strings.Repeat("b", 2000)
	docs := []ToolOutput{fetchRow("https://example.test/a", "A", "SOURCE_EVIDENCE\n[audit_target] literal source text", 0, 47)}
	for _, version := range []int{1, 2} {
		for _, target := range append([]string{""}, partitionGroundingReport(report)...) {
			capture, _ := groundingRequest(deps, core.AgentTask{ExecutorVersion: version}, report, docs, target)
			want, _ := groundingRequest(deps, core.AgentTask{ExecutorVersion: version}, report, docs, "")
			before := groundingMessageText(capture.Messages[len(capture.Messages)-1])
			actualReport, base, err := groundingReplayPartitionBase(capture)
			if err != nil || actualReport != report || !reflect.DeepEqual(base, want) {
				t.Fatal("replay changed captured source/report or retained target", version, target, err)
			}
			if groundingMessageText(capture.Messages[len(capture.Messages)-1]) != before {
				t.Fatal("replay modified captured request")
			}
		}
	}
}
