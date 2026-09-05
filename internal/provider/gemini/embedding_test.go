package gemini

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Documents and queries are sent with their own task types, the
// configured width is requested, and a truncated vector comes back
// normalised to unit length.
func TestEmbedSendsTaskTypesAndNormalises(t *testing.T) {
	var seen []embedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		seen = append(seen, req)
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": map[string]any{"values": []float32{3, 4, 0}}})
	}))
	defer srv.Close()
	p := NewEmbeddingProvider("key", "gemini-embedding-001", 3, time.Second).WithBaseURL(srv.URL + "/%s?key=%s")
	doc, err := p.Embed(context.Background(), "премия сто тысяч")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if _, err := p.EmbedQuery(context.Background(), "сколько была премия?"); err != nil {
		t.Fatalf("embed query: %v", err)
	}
	if len(seen) != 2 || seen[0].TaskType != taskDocument || seen[1].TaskType != taskQuery {
		t.Fatalf("task types: %+v", seen)
	}
	if seen[0].OutputDimensionality != 3 || seen[0].Model != "models/gemini-embedding-001" {
		t.Fatalf("request shape: %+v", seen[0])
	}
	var n float64
	for _, x := range doc {
		n += float64(x) * float64(x)
	}
	if math.Abs(math.Sqrt(n)-1) > 1e-6 || math.Abs(float64(doc[0])-0.6) > 1e-6 {
		t.Fatalf("not normalised: %v", doc)
	}
}

// A vector of the wrong width is an error, not a silently mis-sized row;
// a 429 is retried, a 400 is not.
func TestEmbedRejectsWrongWidthAndRetriesTransientErrors(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch {
		case calls == 1:
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "slow down"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"embedding": map[string]any{"values": []float32{1, 0}}})
		}
	}))
	defer srv.Close()
	p := NewEmbeddingProvider("key", "m", 3, time.Second).WithBaseURL(srv.URL + "/%s?key=%s")
	p.backoffs = []time.Duration{time.Millisecond}
	if _, err := p.Embed(context.Background(), "x"); err == nil {
		t.Fatal("two values for a three-wide provider must be an error")
	}
	if calls != 2 {
		t.Fatalf("the 429 must be retried once: %d calls", calls)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "bad"}})
	}))
	defer bad.Close()
	calls = 0
	p = NewEmbeddingProvider("key", "m", 0, time.Second).WithBaseURL(bad.URL + "/%s?key=%s")
	if _, err := p.Embed(context.Background(), "x"); err == nil || calls != 1 {
		t.Fatalf("a 400 fails once: err=%v calls=%d", err, calls)
	}
}
