package core

import "context"

// ImageResult is a generated picture and what a host needs to store it.
type ImageResult struct {
	Data []byte
	MIME string
	// Width and Height are read off the encoded bytes when the format
	// makes that cheap (PNG header). Zero means unknown rather than
	// zero-sized — hosts should treat it as "did not measure".
	Width  int
	Height int
}

// ImageGenerator produces an image from a text prompt.
//
// Deliberately narrow: no size, style or count. Those belong to whichever
// surface implements this, and every provider spells them differently. A
// host that needs them can type-assert to the concrete provider.
//
// Not every CompletionProvider implements this. Hosts obtain it by asserting
// on the provider they already hold:
//
//	if gen, ok := deps.LLM.(ImageGenerator); ok { … }
type ImageGenerator interface {
	GenerateImage(ctx context.Context, prompt string) (ImageResult, error)
}
