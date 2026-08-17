package core

import "strings"

// NormalizeDocURL reduces a document URL to the form two subsystems can
// compare it by. It exists because the same page is written three ways in
// one task: the researcher's citation inside prose (often wrapped in
// markdown punctuation), the URL the fetch tool was handed, and the URL the
// fetch resolved to after redirects and the arxiv /abs/ → /pdf/ rewrite.
//
// Shared rather than reimplemented per caller: the grounding auditor decides
// which documents get context budget by matching citations to fetched rows,
// and the fetch cache decides what may be replayed by matching a request to
// what a task already read. Two normalizers that drift apart would give the
// same URL two different identities in the same task.
func NormalizeDocURL(raw string) string {
	u := strings.TrimSpace(raw)
	// Prose and markdown wrap links: "see (https://x/y)." or "«https://x/y»".
	u = strings.TrimLeft(u, "(<[«\"'`")
	u = strings.TrimRight(u, ".,;:!?)>]»\"'`")
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	u = strings.TrimPrefix(u, "www.")
	// A fragment addresses a place inside a document, not another document.
	if i := strings.IndexByte(u, '#'); i >= 0 {
		u = u[:i]
	}
	return strings.ToLower(strings.TrimSuffix(u, "/"))
}

// SameDocURL reports whether two URLs address the same document. Empty
// never matches empty: a row with no URL is unidentified, not universal.
func SameDocURL(a, b string) bool {
	na, nb := NormalizeDocURL(a), NormalizeDocURL(b)
	return na != "" && na == nb
}
