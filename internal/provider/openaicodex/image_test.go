package openaicodex

import (
	"encoding/base64"
	"strings"
	"testing"
)

// sse builds a stream body from raw event JSON lines.
func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: " + e + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func b64(n int, fill byte) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string(fill), n)))
}

// The backend streams progressively refined frames, and which event carries
// the final one varies. Taking the largest payload is what makes the reader
// independent of that — picking "the last event" would return a preview
// whenever a partial arrives after the result.
func TestReadImagePayloadKeepsTheLargestFrame(t *testing.T) {
	small, large := b64(2000, 'a'), b64(9000, 'b')
	body := sse(
		`{"type":"response.created"}`,
		`{"type":"response.image_generation_call.in_progress"}`,
		`{"type":"response.image_generation_call.partial_image","partial_image_b64":"`+large+`"}`,
		`{"type":"response.image_generation_call.partial_image","partial_image_b64":"`+small+`"}`,
		`{"type":"response.completed"}`,
	)
	got, err := readImagePayload(strings.NewReader(body))
	if err != nil {
		t.Fatalf("readImagePayload: %v", err)
	}
	if got != large {
		t.Fatalf("got a %d-char frame, want the %d-char one", len(got), len(large))
	}
}

// Both shapes the surface has been observed to use have to work: the frame
// event's own field, and a result nested on a completed output item.
func TestReadImagePayloadAcceptsResultShapes(t *testing.T) {
	payload := b64(3000, 'c')
	cases := map[string]string{
		"partial_image_b64": `{"type":"response.image_generation_call.partial_image","partial_image_b64":"` + payload + `"}`,
		"event result":      `{"type":"response.image_generation_call.completed","result":"` + payload + `"}`,
		"item result":       `{"type":"response.output_item.done","item":{"result":"` + payload + `"}}`,
	}
	for name, event := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := readImagePayload(strings.NewReader(sse(event)))
			if err != nil {
				t.Fatalf("readImagePayload: %v", err)
			}
			if got != payload {
				t.Fatalf("payload not recovered from %s", name)
			}
		})
	}
}

// Text deltas carry the model's prose, never pixels. Scooping them up would
// hand the caller base64 of a sentence and fail at decode — or worse, decode
// into garbage bytes that get stored as an image.
func TestReadImagePayloadIgnoresNonImageEvents(t *testing.T) {
	body := sse(
		`{"type":"response.output_text.delta","delta":"`+b64(5000, 'z')+`"}`,
		`{"type":"response.completed"}`,
	)
	got, err := readImagePayload(strings.NewReader(body))
	if err != nil {
		t.Fatalf("readImagePayload: %v", err)
	}
	if got != "" {
		t.Fatalf("text delta was mistaken for an image (%d chars)", len(got))
	}
}

// "result" is not a field unique to drawing. A function call returning a
// large blob populates the same key, and without the event-type gate its
// payload would be decoded and stored as the user's picture.
func TestReadImagePayloadIgnoresResultsFromOtherTools(t *testing.T) {
	body := sse(
		`{"type":"response.function_call.completed","result":"`+b64(7000, 'f')+`"}`,
		`{"type":"response.custom_tool_call.done","item":{"result":"`+b64(6000, 'g')+`"}}`,
		`{"type":"response.completed"}`,
	)
	got, err := readImagePayload(strings.NewReader(body))
	if err != nil {
		t.Fatalf("readImagePayload: %v", err)
	}
	if got != "" {
		t.Fatalf("a non-image tool result was taken for a picture (%d chars)", len(got))
	}
}

// A full-resolution frame is megabytes on one line. bufio's default 64 KiB
// cap would end the scan with an error and the caller would report "no
// image" for a generation that in fact succeeded.
func TestReadImagePayloadHandlesMultiMegabyteLines(t *testing.T) {
	huge := b64(3<<20, 'q')
	body := sse(`{"type":"response.image_generation_call.partial_image","partial_image_b64":"` + huge + `"}`)
	got, err := readImagePayload(strings.NewReader(body))
	if err != nil {
		t.Fatalf("readImagePayload: %v", err)
	}
	if got != huge {
		t.Fatalf("recovered %d chars of %d", len(got), len(huge))
	}
}

func TestSniffImageMIME(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"png", []byte("\x89PNG\r\n\x1a\n....."), "image/png"},
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0}, "image/jpeg"},
		{"gif", []byte("GIF89a......"), "image/gif"},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), "image/webp"},
		{"unknown", []byte("not a picture at all"), "application/octet-stream"},
		{"empty", nil, "application/octet-stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sniffImageMIME(tc.data); got != tc.want {
				t.Fatalf("sniffImageMIME = %q, want %q", got, tc.want)
			}
		})
	}
}

// Dimensions are read straight off the IHDR chunk. Zero means "not
// measured" and must not be confused with a zero-sized image.
func TestPNGDimensions(t *testing.T) {
	png := make([]byte, 24)
	copy(png, "\x89PNG\r\n\x1a\n")
	// width 1122, height 1402 — the shape production returned.
	png[16], png[17], png[18], png[19] = 0, 0, 0x04, 0x62
	png[20], png[21], png[22], png[23] = 0, 0, 0x05, 0x7A
	w, h := pngDimensions(png)
	if w != 1122 || h != 1402 {
		t.Fatalf("pngDimensions = %dx%d, want 1122x1402", w, h)
	}

	// Long enough to clear the length guard, so only the signature check
	// can reject it. A short blob would pass this test with the signature
	// check deleted, which is the mutation that has to fail here.
	jpeg := make([]byte, 64)
	jpeg[0], jpeg[1], jpeg[2] = 0xFF, 0xD8, 0xFF
	jpeg[16], jpeg[19] = 0x09, 0x99 // plausible-looking garbage at IHDR offsets
	if w, h := pngDimensions(jpeg); w != 0 || h != 0 {
		t.Fatalf("jpeg measured as %dx%d, want unknown", w, h)
	}
	if w, h := pngDimensions(nil); w != 0 || h != 0 {
		t.Fatalf("nil measured as %dx%d, want unknown", w, h)
	}
}
