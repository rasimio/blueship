// urls.go — shared URL extraction for chat text.
//
// Both the user's pasted message and the assistant reply funnel
// through ExtractURLs so the gateway can persist link attachments
// (vaelum.chat_attachments kind='link') symmetrically. We deliberately
// keep this stupid-simple — a single regex over the raw text — rather
// than a full URL parser, because the input is natural-language chat
// from either side. False positives are cheap (the OG worker stamps
// og_fetched_at on failure and moves on) and false negatives are
// invisible (no chip rendered), so optimising for parsing precision
// would just add complexity nobody can debug at 3 am.
package attachment

import (
	"regexp"
	"strings"
)

// urlRE matches an http/https URL run greedily up to the first piece of
// chat punctuation. Whitespace, angle brackets, parentheses, square
// brackets and the three string quotes are the natural terminators —
// they cover markdown link syntax `[text](https://x.com)`, parenthetical
// asides "(see https://x.com)", quoted strings, and the assistant's
// own [attached: UUID] markers. Anything else (commas, periods,
// semicolons) gets trimmed in the post-pass below so a sentence-final
// "https://x.com." doesn't ship a trailing dot to the OG worker.
var urlRE = regexp.MustCompile(`https?://[^\s<>()\[\]"']+`)

// trailingNoise is the punctuation we strip off the tail of each
// match. Sentences end with periods / commas / exclamation marks; the
// markdown closer for `[txt](url)` was already excluded by urlRE but a
// stray `)]>}` from prose still needs to come off. Repeated until
// stable so "https://x.com.).," collapses cleanly to "https://x.com".
const trailingNoise = ".,!?:;)]}>"

// ExtractURLs returns the canonical http/https URLs found in s, in
// order of appearance and de-duplicated. URLs inside markdown link
// syntax ([txt](url)), inside parentheses, or with trailing prose
// punctuation are normalised by trimming the noise around them.
//
// The filter is intentionally permissive: a string survives if (a) it
// starts http:// or https://, (b) it is at least 10 chars after
// trimming (so "http://x" gets rejected before it reaches the OG
// fetch worker), and (c) the host part contains a dot (so localhost
// pastes and protocol-only fragments don't enqueue).
func ExtractURLs(s string) []string {
	if s == "" {
		return nil
	}
	matches := urlRE.FindAllString(s, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(matches))
	out := make([]string, 0, len(matches))
	for _, raw := range matches {
		u := strings.TrimRight(raw, trailingNoise)
		if len(u) < 10 {
			continue
		}
		// Reject anything that doesn't have at least one dot in the
		// host portion. Strip the scheme, then look up to the first
		// path / query / fragment / port separator. Cheap host probe
		// without dragging in net/url just for this one check.
		host := u[strings.Index(u, "://")+3:]
		for i, c := range host {
			if c == '/' || c == '?' || c == '#' || c == ':' {
				host = host[:i]
				break
			}
		}
		if !strings.Contains(host, ".") {
			continue
		}
		if seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}
