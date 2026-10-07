package browser

import (
	"bytes"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// Links are navigation hints, not evidence that the destination was fetched.
// Bound both count and label size independently from the source text window.
const pageLinksJS = `(() => {
 const seen = new Set(), links = [];
 const all = Array.from(document.querySelectorAll('a[href]'));
 const content = all.filter(a => !a.closest('header,nav,footer,aside'));
 const headings = content.filter(a => a.querySelector('h1,h2,h3,h4,h5,h6') || a.closest('h1,h2,h3,h4,h5,h6'));
 for (const a of [...headings, ...content, ...all]) {
  if (links.length >= 1000) break;
  if (!a.getClientRects().length || a.closest('[hidden], [aria-hidden="true"]') || getComputedStyle(a).visibility === 'hidden') continue;
  if (!/^https?:$/.test(a.protocol) || a.href.length > 4096 || seen.has(a.href)) continue;
  const text = (a.innerText || a.getAttribute('aria-label') || '').trim();
  if (!text) continue;
  seen.add(a.href); links.push({url:a.href, text:Array.from(text).slice(0,160).join('')});
 }
 return links;
})()`

func extractPageLinks(body []byte, finalURL string) []PageLink {
	base, err := url.Parse(finalURL)
	if err != nil {
		return nil
	}
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	// HTML base applies even to anchors appearing before it in document order.
	var findBase func(*html.Node) bool
	findBase = func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == "base" {
			for _, attr := range n.Attr {
				if attr.Key == "href" {
					if ref, err := url.Parse(attr.Val); err == nil {
						base = base.ResolveReference(ref)
					}
					return true
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			if findBase(child) {
				return true
			}
		}
		return false
	}
	findBase(doc)
	var links []PageLink
	seen := map[string]bool{}
	skipNavigation := true
	headingsOnly := true
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if len(links) >= 1000 {
			return
		}
		if n.Type == html.ElementNode {
			if skipNavigation && (n.Data == "header" || n.Data == "nav" || n.Data == "footer" || n.Data == "aside") {
				return
			}
			if n.Data == "script" || n.Data == "style" || n.Data == "template" || n.Data == "noscript" {
				return
			}
			var href string
			for _, attr := range n.Attr {
				if attr.Key == "hidden" || (attr.Key == "aria-hidden" && attr.Val == "true") {
					return
				}
				if attr.Key == "style" {
					style := strings.ReplaceAll(strings.ToLower(attr.Val), " ", "")
					if strings.Contains(style, "display:none") || strings.Contains(style, "visibility:hidden") {
						return
					}
				}
				if attr.Key == "href" {
					href = attr.Val
				}
			}
			if n.Data == "a" && href != "" && (!headingsOnly || isHeadingLink(n)) {
				if ref, err := url.Parse(href); err == nil {
					resolved := base.ResolveReference(ref)
					address := resolved.String()
					label := strings.TrimSpace(staticNodeText(n))
					if (resolved.Scheme == "http" || resolved.Scheme == "https") && len(address) <= 4096 && !seen[address] && label != "" {
						seen[address] = true
						runes := []rune(label)
						links = append(links, PageLink{URL: address, Text: string(runes[:min(len(runes), 160)])})
					}
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	headingsOnly = false
	walk(doc)
	skipNavigation = false
	walk(doc)
	return links
}

// Heading anchors identify content entries even when navigation uses plain divs.
// Prioritize these before deduplication so a preceding thumbnail does not hide
// the descriptive title for the same destination. Keep other links afterward.
func isHeadingLink(n *html.Node) bool {
	isHeading := func(n *html.Node) bool {
		return n.Type == html.ElementNode && len(n.Data) == 2 && n.Data[0] == 'h' && n.Data[1] >= '1' && n.Data[1] <= '6'
	}
	for parent := n.Parent; parent != nil; parent = parent.Parent {
		if isHeading(parent) {
			return true
		}
	}
	var contains func(*html.Node) bool
	contains = func(n *html.Node) bool {
		if isHeading(n) {
			return true
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			if contains(child) {
				return true
			}
		}
		return false
	}
	return contains(n)
}
