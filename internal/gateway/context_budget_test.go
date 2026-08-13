package gateway

import "testing"

// One switch for the whole session-summary feature: the writer and the
// prompt reader both consult it, so a negative threshold silences the
// summariser AND stops the last stored summary from riding every prompt.
// Production's freshest "summary" was a verbatim echo of one old reply,
// presented as the memory of 569 messages — writer-only gating would have
// kept serving it indefinitely.
func TestSummariesEnabledIsTheSingleOffSwitch(t *testing.T) {
	if summariesEnabled(-1) {
		t.Fatal("negative threshold must mean OFF — it is the operator's kill switch")
	}
	if summariesEnabled(0) {
		t.Fatal("zero reaches blueship only if the host skipped its defaulting; it must not enable the feature")
	}
	if !summariesEnabled(80000) {
		t.Fatal("a real threshold must keep the feature on")
	}
}
