package pdf

import "strings"

// Scanned-PDF detection + render defaults, shared by every transport
// that ingests PDFs (Telegram gateway, cabinet httpchat). A PDF whose
// extracted text is under these floors is a scan: pages are images and
// the text layer is absent or vestigial.
const (
	// DefaultScanMaxPages bounds how many pages a transport renders for
	// vision when the text layer is missing.
	DefaultScanMaxPages = 6
	// DefaultScanDPI keeps an A4 page ~1250×1750 px — crisp for
	// document text, comfortably inside vision input limits.
	DefaultScanDPI = 150

	scanMinTextPerPage = 20
	scanMinTextTotal   = 100
)

// TextLooksScanned reports whether extraction yielded too little text
// to stand in for the document. Phone scans typically yield zero
// characters; a page of real prose yields thousands.
func TextLooksScanned(text string, pages int) bool {
	n := len(strings.TrimSpace(text))
	if n >= scanMinTextTotal {
		return false
	}
	if pages <= 0 {
		pages = 1
	}
	return n < pages*scanMinTextPerPage
}
