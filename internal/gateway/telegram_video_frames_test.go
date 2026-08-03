package gateway

import (
	"context"
	"encoding/base64"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

func TestVideoFrameCountSpreadsABudgetOverTheRecording(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		want     int
	}{
		{"unknown duration samples nothing", 0, 0},
		{"a few seconds still shows movement", 4 * time.Second, videoFrameMinCount},
		{"half a minute", 30 * time.Second, 6},
		{"a full video note hits the cap", 60 * time.Second, videoFrameMaxCount},
		{"an hour is capped, not sampled hourly", time.Hour, videoFrameMaxCount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := videoFrameCount(tt.duration); got != tt.want {
				t.Fatalf("videoFrameCount(%v) = %d, want %d", tt.duration, got, tt.want)
			}
		})
	}
}

func TestFormatFrameOffset(t *testing.T) {
	tests := []struct {
		offset time.Duration
		want   string
	}{
		{0, "[00:00]"},
		{7 * time.Second, "[00:07]"},
		{65 * time.Second, "[01:05]"},
		{7500 * time.Millisecond, "[00:08]"},
	}
	for _, tt := range tests {
		if got := formatFrameOffset(tt.offset); got != tt.want {
			t.Fatalf("formatFrameOffset(%v) = %q, want %q", tt.offset, got, tt.want)
		}
	}
}

// The reader can only talk about when something happened if every still is
// introduced by its own timestamp, so the block order is load-bearing.
func TestVideoFramePromptOrdersFramesBehindTimestamps(t *testing.T) {
	frames := []videoFrame{
		{offset: 0, jpeg: []byte("first")},
		{offset: 5 * time.Second, jpeg: []byte("second")},
	}
	blocks := videoFramePrompt(frames, "и вот тут смотри", "что я показываю?")

	if len(blocks) != 3+len(frames)*2 {
		t.Fatalf("blocks = %d, want context + header + one text/image pair per frame: %#v", len(blocks), blocks)
	}
	if !strings.Contains(blocks[0].Text, "что я показываю?") {
		t.Fatalf("the user's question must come first, got %#v", blocks[0])
	}
	if !strings.Contains(blocks[1].Text, "и вот тут смотри") {
		t.Fatalf("the speech must reach the reader, got %#v", blocks[1])
	}
	if !strings.Contains(blocks[2].Text, "2 frames") {
		t.Fatalf("the reader must be told how many frames follow, got %#v", blocks[2])
	}

	for i, frame := range frames {
		label, image := blocks[3+i*2], blocks[4+i*2]
		if label.Type != "text" || label.Text != formatFrameOffset(frame.offset) {
			t.Fatalf("frame %d label = %#v, want its timestamp", i, label)
		}
		if image.Type != "image" || image.Source == nil {
			t.Fatalf("frame %d = %#v, want an image block", i, image)
		}
		if image.Source.MediaType != "image/jpeg" {
			t.Fatalf("frame %d media type = %q, want image/jpeg", i, image.Source.MediaType)
		}
		if want := base64.StdEncoding.EncodeToString(frame.jpeg); image.Source.Data != want {
			t.Fatalf("frame %d data = %q, want %q", i, image.Source.Data, want)
		}
	}
}

// A video note with no caption and no speech is the common case; it must still
// produce a usable prompt rather than empty context blocks.
func TestVideoFramePromptOmitsMissingContext(t *testing.T) {
	blocks := videoFramePrompt([]videoFrame{{jpeg: []byte("only")}}, "  ", "")
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want header + one pair: %#v", len(blocks), blocks)
	}
	if blocks[0].Type != "text" || !strings.Contains(blocks[0].Text, "1 frames") {
		t.Fatalf("first block should be the frame header, got %#v", blocks[0])
	}
}

// Frames go to the same model row as images but with the video prompt: the
// reader is told these are one scene over time, not an album.
func TestDescribeVideoFramesUsesTheVideoPrompt(t *testing.T) {
	reader := &stubReader{reply: "Человек за столом показывает в камеру блокнот."}
	g := visionGateway(visionRefs(), reader)

	description, err := g.describeVideoFrames(
		context.Background(),
		[]videoFrame{{offset: 0, jpeg: []byte("a")}, {offset: 5 * time.Second, jpeg: []byte("b")}},
		"смотри что нашёл",
		"что там у меня?",
	)
	if err != nil {
		t.Fatalf("describeVideoFrames() error = %v", err)
	}
	if description != reader.reply {
		t.Fatalf("description = %q, want the reader's words", description)
	}
	if len(reader.requests) != 1 {
		t.Fatalf("want a single reader call, got %d", len(reader.requests))
	}

	req := reader.requests[0]
	if req.System != videoSystemPrompt {
		t.Fatalf("the reader must be told it is watching a video, got system prompt %q", req.System)
	}
	if req.Model != "anthropic-oauth:claude-haiku-4-5-20251001" {
		t.Fatalf("reader model = %q, want the vision row's model", req.Model)
	}
	images := 0
	for _, b := range bs.NormalizeContent(req.Messages[0].Content) {
		if b.Type == "image" {
			images++
		}
	}
	if images != 2 {
		t.Fatalf("images sent = %d, want every sampled frame", images)
	}
}

// Without a vision row there is no one to read the frames. The turn keeps its
// transcript instead of failing — the behaviour before video had a picture.
func TestDescribeVideoFramesWithoutVisionRowStaysSilent(t *testing.T) {
	reader := &stubReader{reply: "should not be called"}
	refs := map[string]bs.ModelRef{"cortex": {Provider: "deepseek", Name: "deepseek-v4-flash"}}
	g := visionGateway(refs, reader)

	description, err := g.describeVideoFrames(context.Background(), []videoFrame{{jpeg: []byte("a")}}, "", "")
	if err != nil {
		t.Fatalf("describeVideoFrames() error = %v", err)
	}
	if description != "" {
		t.Fatalf("description = %q, want empty without a reader", description)
	}
	if len(reader.requests) != 0 {
		t.Fatalf("no vision row must mean no call, got %d", len(reader.requests))
	}
}

func TestDescribeVideoFramesWithoutFramesStaysSilent(t *testing.T) {
	reader := &stubReader{reply: "should not be called"}
	g := visionGateway(visionRefs(), reader)

	description, err := g.describeVideoFrames(context.Background(), nil, "речь есть, картинки нет", "")
	if err != nil {
		t.Fatalf("describeVideoFrames() error = %v", err)
	}
	if description != "" || len(reader.requests) != 0 {
		t.Fatalf("no frames must mean no call, got %q after %d calls", description, len(reader.requests))
	}
}

// Sampling blind would either miss the end of a recording or decode every
// frame of it, so an unknown duration is an error rather than a guess.
func TestExtractVideoFramesRequiresDuration(t *testing.T) {
	if _, err := extractVideoFrames(context.Background(), "fixture.mp4", 0); err == nil {
		t.Fatal("extractVideoFrames() with an unknown duration should fail")
	}
}

func TestExtractVideoFramesSamplesAcrossTheRecording(t *testing.T) {
	ffmpeg, err := findFFmpeg()
	if err != nil {
		t.Skipf("ffmpeg unavailable: %v", err)
	}

	const duration = 20 * time.Second
	path := filepath.Join(t.TempDir(), "fixture.mp4")
	cmd := exec.Command(
		ffmpeg,
		"-hide_banner",
		"-loglevel", "error",
		"-y",
		"-f", "lavfi",
		"-i", "testsrc=size=320x240:rate=10:duration=20",
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		path,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("create video fixture: %v: %s", err, output)
	}

	frames, err := extractVideoFrames(context.Background(), path, duration)
	if err != nil {
		t.Fatalf("extractVideoFrames() error = %v", err)
	}
	want := videoFrameCount(duration)
	if len(frames) != want {
		t.Fatalf("frames = %d, want %d", len(frames), want)
	}

	interval := duration / time.Duration(want)
	for i, frame := range frames {
		if got := frame.offset; got != time.Duration(i)*interval {
			t.Fatalf("frame %d offset = %v, want %v", i, got, time.Duration(i)*interval)
		}
		if len(frame.jpeg) < 2 || frame.jpeg[0] != 0xFF || frame.jpeg[1] != 0xD8 {
			t.Fatalf("frame %d is not a JPEG (%d bytes)", i, len(frame.jpeg))
		}
	}
	// testsrc animates a counter, so identical first and last frames would
	// mean every still came from the same moment.
	if string(frames[0].jpeg) == string(frames[len(frames)-1].jpeg) {
		t.Fatal("first and last frame are identical — sampling did not move through the recording")
	}
}

// Speech and picture must arrive as one artifact. Shipped as two sibling
// blocks, the model read the transcript as the user's words and skipped the
// description as metadata — a recording whose point was on screen came back
// answered from the soundtrack alone.
func TestVideoTurnBlockJoinsSpeechAndPicture(t *testing.T) {
	got := videoTurnBlock("привет", "Человек машет рукой.")
	if !strings.HasPrefix(got, videoBlockOpen) || !strings.HasSuffix(got, videoBlockClose) {
		t.Fatalf("the recording must be bracketed as one block, got %q", got)
	}
	if strings.Count(got, videoBlockOpen) != 1 {
		t.Fatalf("speech and picture must share a single block, got %q", got)
	}
	said, seen := strings.Index(got, "said: привет"), strings.Index(got, "seen: Человек машет рукой.")
	if said < 0 || seen < 0 {
		t.Fatalf("both halves must be present and named, got %q", got)
	}
	if said > seen {
		t.Fatalf("speech should come before the picture, got %q", got)
	}
}

// Each half stands alone: a silent recording still has a picture worth
// describing, and a video the reader cannot see still has its speech.
func TestVideoTurnBlockHandlesAHalfOnItsOwn(t *testing.T) {
	if got := videoTurnBlock("", "Человек машет рукой."); strings.Contains(got, "said:") {
		t.Fatalf("a silent recording must not claim speech, got %q", got)
	} else if !strings.Contains(got, "seen: Человек машет рукой.") {
		t.Fatalf("the picture must survive on its own, got %q", got)
	}
	if got := videoTurnBlock("только речь", "  "); strings.Contains(got, "seen:") {
		t.Fatalf("an unread picture must not claim a description, got %q", got)
	} else if !strings.Contains(got, "said: только речь") {
		t.Fatalf("the speech must survive on its own, got %q", got)
	}
	if got := videoTurnBlock(" ", ""); got != "" {
		t.Fatalf("nothing read means no block at all, got %q", got)
	}
}
