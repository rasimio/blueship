package gateway

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Frame sampling for the visual read of a video. A Telegram video note runs
// at most a minute of a mostly static talking head, so a still every few
// seconds carries the whole story, and the cap keeps a half-hour screen
// recording from turning into a hundred images. Frames are scaled down
// because the reader gains nothing from a 4K still and pays for every pixel.
const (
	videoFrameSecondsPerFrame = 5
	videoFrameMinCount        = 3
	videoFrameMaxCount        = 8
	// videoFrameMaxEdgePixels only ever shrinks a frame, and it is set high
	// enough to leave a video note untouched: a Telegram circle is 384-640px
	// square, and the thing people film with one is usually something held up
	// to the camera with writing on it. Downscaling those to a thumbnail cost
	// the reader the text it was being asked to read. A screen recording still
	// gets cut down from 1080p.
	videoFrameMaxEdgePixels = 1024
)

// videoFrame is one sampled still together with the position it was taken
// from, so the reader can be told what happened when instead of inferring an
// order from the block sequence.
type videoFrame struct {
	offset time.Duration
	jpeg   []byte
}

// videoFrameCount spreads a fixed budget of stills over the whole recording.
// Short clips still get enough frames to show movement; long ones get the cap
// and a coarser interval rather than more images.
func videoFrameCount(duration time.Duration) int {
	if duration <= 0 {
		return 0
	}
	count := int(math.Round(duration.Seconds() / videoFrameSecondsPerFrame))
	if count < videoFrameMinCount {
		count = videoFrameMinCount
	}
	if count > videoFrameMaxCount {
		count = videoFrameMaxCount
	}
	return count
}

// extractVideoFrames samples the first video stream into evenly spaced JPEGs.
// duration must be known: it is what turns a frame budget into a frame rate,
// and sampling blind would either miss the end of the recording or decode
// every frame of it.
func extractVideoFrames(ctx context.Context, sourcePath string, duration time.Duration) ([]videoFrame, error) {
	if duration <= 0 {
		return nil, fmt.Errorf("extract video frames: unknown duration")
	}
	ffmpeg, err := findFFmpeg()
	if err != nil {
		return nil, err
	}

	count := videoFrameCount(duration)
	dir, err := os.MkdirTemp("", "blueship-frames-*")
	if err != nil {
		return nil, fmt.Errorf("create frame temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	// `count` frames spread across `seconds` of recording. ffmpeg emits the
	// first at t=0 and the rest one interval apart, so frame i comes from
	// i*duration/count and there is no need to ask where each one came from.
	// The scale expression only ever shrinks: a 384px video note would gain
	// nothing from being blown up to the cap.
	seconds := int(math.Ceil(duration.Seconds()))
	filters := fmt.Sprintf("fps=%d/%d,scale=w='min(%d,iw)':h=-2", count, seconds, videoFrameMaxEdgePixels)

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ffmpeg,
		"-hide_banner",
		"-loglevel", "error",
		"-y",
		"-i", sourcePath,
		"-map", "0:v:0",
		"-an",
		"-vf", filters,
		"-q:v", "4",
		"-frames:v", strconv.Itoa(count),
		filepath.Join(dir, "frame-%03d.jpg"),
	)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("extract video frames: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	names, err := filepath.Glob(filepath.Join(dir, "frame-*.jpg"))
	if err != nil {
		return nil, fmt.Errorf("list extracted frames: %w", err)
	}
	// ffmpeg zero-pads the counter, so lexical order is capture order.
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("extract video frames: empty output")
	}

	interval := duration / time.Duration(count)
	frames := make([]videoFrame, 0, len(names))
	for i, name := range names {
		data, readErr := os.ReadFile(name)
		if readErr != nil {
			return nil, fmt.Errorf("read extracted frame: %w", readErr)
		}
		frames = append(frames, videoFrame{offset: time.Duration(i) * interval, jpeg: data})
	}
	return frames, nil
}

// probeVideoDuration asks ffprobe how long a recording is. Only needed for
// videos that arrive as documents — Telegram already reports the duration of
// a video note or a video message.
func probeVideoDuration(ctx context.Context, sourcePath string) (time.Duration, error) {
	ffprobe, err := findFFprobe()
	if err != nil {
		return 0, err
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ffprobe,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		sourcePath,
	)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("probe video duration: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(stdout.String()), 64)
	if err != nil {
		return 0, fmt.Errorf("probe video duration: unreadable output %q: %w", strings.TrimSpace(stdout.String()), err)
	}
	if seconds <= 0 {
		return 0, fmt.Errorf("probe video duration: reported %v seconds", seconds)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func findFFprobe() (string, error) {
	if path, err := exec.LookPath("ffprobe"); err == nil {
		return path, nil
	}
	const homebrewFFprobe = "/opt/homebrew/bin/ffprobe"
	if info, err := os.Stat(homebrewFFprobe); err == nil && !info.IsDir() {
		return homebrewFFprobe, nil
	}
	return "", fmt.Errorf("ffprobe not found")
}
