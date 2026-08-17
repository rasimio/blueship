package core

import "testing"

// One URL, three writings: what the model typed into the tool, what the
// fetch resolved to, and what the report wrote inside a sentence. All three
// have to name the same document, or the fetch cache re-downloads a page the
// task already read and the auditor spends its budget on the wrong doc.
func TestNormalizeDocURLGivesOneIdentityPerDocument(t *testing.T) {
	same := [][2]string{
		{"https://example.test/doc", "http://example.test/doc"},
		{"https://example.test/doc/", "https://example.test/doc"},
		{"https://www.example.test/doc", "https://example.test/doc"},
		{"https://example.test/doc#results", "https://example.test/doc"},
		{"(https://example.test/doc).", "https://example.test/doc"},
		{"«https://example.test/doc»", "https://example.test/doc"},
		{"https://Example.Test/Doc", "https://example.test/doc"},
	}
	for _, pair := range same {
		if !SameDocURL(pair[0], pair[1]) {
			t.Errorf("%q and %q read as different documents (%q vs %q)",
				pair[0], pair[1], NormalizeDocURL(pair[0]), NormalizeDocURL(pair[1]))
		}
	}

	// The distinctions that must survive: a path is not another path, and a
	// query names different content on the same page.
	different := [][2]string{
		{"https://example.test/a", "https://example.test/b"},
		{"https://example.test/doc?page=2", "https://example.test/doc"},
		{"https://arxiv.org/abs/2408.03515", "https://arxiv.org/abs/2404.05291"},
	}
	for _, pair := range different {
		if SameDocURL(pair[0], pair[1]) {
			t.Errorf("%q and %q collapsed into one document", pair[0], pair[1])
		}
	}

	// An unidentified row must not match another unidentified row, or the
	// cache would answer every request with the first body it stored.
	if SameDocURL("", "") || SameDocURL("   ", "") {
		t.Error("empty URLs match each other; a row with no URL would answer for any document")
	}
}
