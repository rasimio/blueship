package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
)

// stubModelStore serves a fixed role→ModelRef map, so these cases exercise the
// vision decision without a database.
type stubModelStore struct {
	refs map[string]bs.ModelRef
}

func (s *stubModelStore) Load(context.Context) error    { return nil }
func (s *stubModelStore) Refresh(context.Context) error { return nil }
func (s *stubModelStore) Get(role string) bs.ModelRef   { return s.refs[role] }
func (s *stubModelStore) ForRouter(role string) string {
	ref, ok := s.refs[role]
	if !ok || ref.Name == "" {
		return ""
	}
	if ref.Provider == "" {
		return ref.Name
	}
	return ref.Provider + ":" + ref.Name
}
func (s *stubModelStore) Update(context.Context, string, string, string) error { return nil }
func (s *stubModelStore) Roles() []string {
	roles := make([]string, 0, len(s.refs))
	for role := range s.refs {
		roles = append(roles, role)
	}
	return roles
}

// stubReader records what the vision model was asked and replies with a canned
// description (or an error).
type stubReader struct {
	reply    string
	err      error
	requests []bs.CompletionRequest
}

func (s *stubReader) Complete(_ context.Context, req bs.CompletionRequest) (*bs.CompletionResponse, error) {
	s.requests = append(s.requests, req)
	if s.err != nil {
		return nil, s.err
	}
	return &bs.CompletionResponse{
		Content:    []bs.ContentBlock{{Type: "text", Text: s.reply}},
		StopReason: "end_turn",
	}, nil
}

func visionGateway(refs map[string]bs.ModelRef, reader bs.CompletionProvider) *Gateway {
	return &Gateway{
		deps:     &bs.Deps{ModelStore: &stubModelStore{refs: refs}, Config: &bs.Config{}},
		provider: reader,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func imageBlock(data string) bs.ContentBlock {
	return bs.ContentBlock{Type: "image", Source: &bs.ImageSource{Type: "base64", MediaType: "image/png", Data: data}}
}

func visionRefs() map[string]bs.ModelRef {
	return map[string]bs.ModelRef{
		"cortex": {Provider: "deepseek", Name: "deepseek-v4-flash"},
		"vision": {Provider: "anthropic-oauth", Name: "claude-haiku-4-5-20251001", MaxTokens: 1500, Temperature: 0.3},
	}
}

// The image is read into text and cortex answers from that text — the reply is
// never written by the vision model.
func TestDescribeImagesReplacesImagesWithText(t *testing.T) {
	reader := &stubReader{reply: "A blue square, RGB roughly (0,140,255)."}
	g := visionGateway(visionRefs(), reader)

	content := []bs.ContentBlock{{Type: "text", Text: "какого цвета квадрат?"}, imageBlock("AAAA")}
	out, ok := g.describeImages(context.Background(), content)
	if !ok {
		t.Fatal("an image turn should be read into text")
	}

	blocks, isBlocks := out.([]bs.ContentBlock)
	if !isBlocks {
		t.Fatalf("content should stay a block slice, got %T", out)
	}
	for _, b := range blocks {
		if b.Type == "image" {
			t.Fatalf("image survived into the cortex turn: %#v", blocks)
		}
	}
	if blocks[0].Text != "какого цвета квадрат?" {
		t.Fatalf("the user's own words must be preserved first, got %#v", blocks)
	}
	if !strings.Contains(blocks[1].Text, "[image_description]") || !strings.Contains(blocks[1].Text, "blue square") {
		t.Fatalf("description block missing or unwrapped: %#v", blocks[1])
	}
}

// The reader is given the user's message, so it can extract what is actually
// being asked about instead of producing a generic caption.
func TestDescribeImagesAsksTheReaderTheUsersQuestion(t *testing.T) {
	reader := &stubReader{reply: "ok"}
	g := visionGateway(visionRefs(), reader)

	content := []bs.ContentBlock{{Type: "text", Text: "что написано в правом нижнем углу?"}, imageBlock("AAAA")}
	if _, ok := g.describeImages(context.Background(), content); !ok {
		t.Fatal("an image turn should be read into text")
	}
	if len(reader.requests) != 1 {
		t.Fatalf("want a single reader call, got %d", len(reader.requests))
	}

	req := reader.requests[0]
	if req.Model != "anthropic-oauth:claude-haiku-4-5-20251001" {
		t.Fatalf("reader model = %q, want the vision row's model", req.Model)
	}
	sent := bs.NormalizeContent(req.Messages[0].Content)
	var sawQuestion, sawImage bool
	for _, b := range sent {
		if b.Type == "text" && strings.Contains(b.Text, "правом нижнем углу") {
			sawQuestion = true
		}
		if b.Type == "image" {
			sawImage = true
		}
	}
	if !sawQuestion {
		t.Fatalf("the reader must receive the user's question, got %#v", sent)
	}
	if !sawImage {
		t.Fatalf("the reader must receive the image, got %#v", sent)
	}
	// A reader that answers would speak in a voice the user never chose.
	if !strings.Contains(req.System, "describe rather than answer") {
		t.Fatalf("reader prompt should keep it describing, got %q", req.System)
	}
}

// Reasoning controls are not portable between models: inheriting them across
// tiers has broken chat before with a 400.
func TestDescribeImagesUsesTheVisionRowsOwnControls(t *testing.T) {
	reader := &stubReader{reply: "ok"}
	refs := visionRefs()
	refs["vision"] = bs.ModelRef{
		Provider: "anthropic-oauth", Name: "claude-haiku-4-5-20251001",
		MaxTokens: 1500, Temperature: 0.3, Effort: "low", ThinkingMode: "off",
	}
	g := visionGateway(refs, reader)

	if _, ok := g.describeImages(context.Background(), []bs.ContentBlock{imageBlock("AAAA")}); !ok {
		t.Fatal("an image turn should be read into text")
	}
	req := reader.requests[0]
	if req.Effort != "low" || req.ThinkingMode != "off" {
		t.Fatalf("reader effort/thinking = %q/%q, want the vision row's own", req.Effort, req.ThinkingMode)
	}
	if req.MaxTokens != 1500 || req.Temperature != 0.3 {
		t.Fatalf("reader limits = %d/%v, want the vision row's own", req.MaxTokens, req.Temperature)
	}
}

// No vision row means the deployment wants cortex to read images itself, so the
// turn must reach it untouched.
func TestDescribeImagesPassesThroughWithoutTheRole(t *testing.T) {
	reader := &stubReader{reply: "should not be called"}
	g := visionGateway(map[string]bs.ModelRef{
		"cortex": {Provider: "anthropic-oauth", Name: "claude-opus-5"},
	}, reader)

	content := []bs.ContentBlock{{Type: "text", Text: "что тут?"}, imageBlock("AAAA")}
	out, ok := g.describeImages(context.Background(), content)
	if ok {
		t.Fatal("without a vision row the turn must be left alone")
	}
	if len(reader.requests) != 0 {
		t.Fatalf("no vision row: the reader must not be called, got %d calls", len(reader.requests))
	}
	if blocks, _ := out.([]bs.ContentBlock); len(blocks) != 2 || blocks[1].Type != "image" {
		t.Fatalf("image must reach cortex untouched, got %#v", out)
	}
}

// A failed or empty reading falls through to the original content: a worse
// answer beats no answer, and a vision-capable cortex may still cope.
func TestDescribeImagesFallsThroughOnReaderFailure(t *testing.T) {
	for name, reader := range map[string]*stubReader{
		"error":       {err: errors.New("upstream exploded")},
		"empty reply": {reply: "   "},
	} {
		g := visionGateway(visionRefs(), reader)
		content := []bs.ContentBlock{{Type: "text", Text: "что тут?"}, imageBlock("AAAA")}

		out, ok := g.describeImages(context.Background(), content)
		if ok {
			t.Fatalf("%s: a failed reading must not report success", name)
		}
		if blocks, _ := out.([]bs.ContentBlock); len(blocks) != 2 || blocks[1].Type != "image" {
			t.Fatalf("%s: original content must survive, got %#v", name, out)
		}
	}
}

// Text turns are the common case: no reader call, nothing rewritten.
func TestDescribeImagesLeavesTextTurnsAlone(t *testing.T) {
	for name, content := range map[string]any{
		"plain string": "просто текст",
		"text blocks":  []bs.ContentBlock{{Type: "text", Text: "просто текст"}},
		"tool result":  []bs.ContentBlock{{Type: "tool_result", ToolUseID: "call_1", Content: "ok"}},
	} {
		reader := &stubReader{reply: "should not be called"}
		g := visionGateway(visionRefs(), reader)

		if _, ok := g.describeImages(context.Background(), content); ok {
			t.Fatalf("%s: a text turn must not be rewritten", name)
		}
		if len(reader.requests) != 0 {
			t.Fatalf("%s: the reader must not be called on a text turn", name)
		}
	}
}

// Several images are read in one call so they can be described in relation to
// each other, and collapse to a single description at the first image's spot.
func TestDescribeImagesHandlesSeveralImagesInOneCall(t *testing.T) {
	reader := &stubReader{reply: "First: a chart. Second: a receipt."}
	g := visionGateway(visionRefs(), reader)

	content := []bs.ContentBlock{
		{Type: "text", Text: "сравни"},
		imageBlock("AAAA"),
		imageBlock("BBBB"),
		{Type: "text", Text: "и что скажешь?"},
	}
	out, ok := g.describeImages(context.Background(), content)
	if !ok {
		t.Fatal("an image turn should be read into text")
	}
	if len(reader.requests) != 1 {
		t.Fatalf("want one reader call for all images, got %d", len(reader.requests))
	}

	blocks, _ := out.([]bs.ContentBlock)
	if len(blocks) != 3 {
		t.Fatalf("two images should collapse to one description block, got %#v", blocks)
	}
	if blocks[0].Text != "сравни" || blocks[2].Text != "и что скажешь?" {
		t.Fatalf("surrounding text must keep its order, got %#v", blocks)
	}
	if !strings.Contains(blocks[1].Text, "receipt") {
		t.Fatalf("description should sit where the first image was, got %#v", blocks)
	}
}
