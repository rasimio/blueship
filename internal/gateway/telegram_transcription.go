package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rasimio/blueship/internal/transport/telegram"
)

const (
	maxTelegramVideoSourceBytes int64 = 2000 << 20
	maxTranscriptionChunkBytes  int64 = 24 << 20
	videoAudioSegmentSeconds          = 8 * 60
)

type telegramTranscriptionInput struct {
	fileID   string
	filename string
	mimeType string
	kind     string
	// duration as Telegram reports it. Frame sampling needs it to turn a
	// frame budget into a frame rate; a video arriving as a document carries
	// no duration and gets probed from the file instead.
	duration time.Duration
}

func telegramTranscriptionInputFor(msg *telegram.Message) (telegramTranscriptionInput, bool) {
	switch {
	case msg == nil:
		return telegramTranscriptionInput{}, false
	case msg.Video != nil && msg.Video.FileID != "":
		return telegramTranscriptionInput{
			fileID:   msg.Video.FileID,
			filename: videoTranscriptionFilename(msg.Video.MimeType),
			mimeType: msg.Video.MimeType,
			kind:     "video",
			duration: time.Duration(msg.Video.Duration) * time.Second,
		}, true
	case msg.VideoNote != nil && msg.VideoNote.FileID != "":
		return telegramTranscriptionInput{
			fileID:   msg.VideoNote.FileID,
			filename: "video-note.mp4",
			mimeType: "video/mp4",
			kind:     "video",
			duration: time.Duration(msg.VideoNote.Duration) * time.Second,
		}, true
	default:
		return telegramTranscriptionInput{}, false
	}
}

func videoTranscriptionFilename(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "video/webm":
		return "video.webm"
	case "video/mpeg":
		return "video.mpeg"
	case "video/quicktime":
		return "video.mov"
	default:
		return "video.mp4"
	}
}

func appendVideoTranscript(text, transcript string) string {
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		return text
	}
	block := "[video transcript]\n" + transcript
	if strings.TrimSpace(text) == "" {
		return block
	}
	return strings.TrimSpace(text) + "\n\n" + block
}

// appendVideoDescription folds the reader's account of the frames into the
// turn. Unlike a photo, a video leaves nothing behind — the bytes are never
// stored and the frames are gone once the turn ends — so this text is the only
// record that the recording showed anything at all, and it belongs in the
// durable message rather than only in the live turn.
func appendVideoDescription(text, description string) string {
	description = strings.TrimSpace(description)
	if description == "" {
		return text
	}
	block := videoDescriptionOpen + description + videoDescriptionClose
	if strings.TrimSpace(text) == "" {
		return block
	}
	return strings.TrimSpace(text) + "\n\n" + block
}

func isTranscribableVideoDocument(name, mimeType string) bool {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(mimeType)), "video/") {
		return true
	}
	switch strings.ToLower(filepath.Ext(strings.TrimSpace(name))) {
	case ".mp4", ".mpeg", ".mpg", ".webm", ".m4v", ".mov":
		return true
	default:
		return false
	}
}

func videoDocumentTranscriptionFilename(name, mimeType string) string {
	switch strings.ToLower(filepath.Ext(strings.TrimSpace(name))) {
	case ".webm":
		return "video.webm"
	case ".mpeg", ".mpg":
		return "video.mpeg"
	case ".mp4":
		return "video.mp4"
	case ".mov":
		return "video.mov"
	default:
		return videoTranscriptionFilename(mimeType)
	}
}

// readVideoIntoTurn folds one video message into the turn: the speech goes in
// verbatim, the frames become a written description, and both land in the
// durable copy of the message as well as the live one. Nothing else keeps a
// record — video bytes are never stored, and the frames are gone once the turn
// ends — so a reading that reached only the turn would be unrecoverable by the
// next message.
//
// Every failure degrades instead of aborting: a recording the reader cannot
// see is still worth its transcript, and a silent one is still worth its
// picture.
func (g *Gateway) readVideoIntoTurn(
	ctx context.Context,
	client *telegram.Client,
	fileID string,
	kind string,
	duration time.Duration,
	text string,
	visibleText string,
) (string, string) {
	// Nothing on this deployment can read the file — no transcriber and no
	// vision row. Say so without pulling the bytes down first: a video can be
	// gigabytes, and the download was skipped here before frames existed.
	if (g.whisper == nil || !g.whisper.IsConfigured()) && !g.canReadVisual() {
		return appendDocInline(text, "[video: audio transcription unavailable]"), visibleText
	}

	// The user's own caption, captured before either copy starts accumulating
	// machine text. It is the question the reader should be answering.
	userText := visibleText

	reading, err := g.readTelegramVideo(ctx, client, fileID, duration)
	if err != nil {
		g.logger.Warn("failed to download video", "error", err, "kind", kind)
		return appendDocInline(text, "[video: could not be read]"), visibleText
	}

	switch {
	case errors.Is(reading.transcriptErr, errTranscriptionUnavailable):
		text = appendDocInline(text, "[video: audio transcription unavailable]")
	case reading.transcriptErr != nil:
		g.logger.Warn("failed to transcribe video", "error", reading.transcriptErr, "kind", kind)
		text = appendDocInline(text, "[video: audio transcription failed]")
	case strings.TrimSpace(reading.transcript) == "":
		text = appendDocInline(text, "[video: no speech detected]")
	default:
		text = appendVideoTranscript(text, reading.transcript)
		visibleText = appendVisibleTranscript(visibleText, reading.transcript)
	}

	if reading.framesErr != nil {
		g.logger.Warn("video frames unavailable, reading speech only",
			"error", reading.framesErr, "kind", kind)
		return text, visibleText
	}
	description, err := g.describeVideoFrames(ctx, reading.frames, reading.transcript, userText)
	if err != nil {
		g.logger.Error("vision: could not read video frames",
			"frames", len(reading.frames), "kind", kind, "error", err)
		return text, visibleText
	}
	if strings.TrimSpace(description) == "" {
		return text, visibleText
	}
	g.logger.Info("vision: video frames read into the turn as text",
		"frames", len(reading.frames),
		"kind", kind,
		"model", g.deps.ModelStore.ForRouter(visionRole),
	)
	return appendVideoDescription(text, description), appendVideoDescription(visibleText, description)
}

// errTranscriptionUnavailable separates "this deployment has no transcription
// provider" from "transcribing this file failed" — the first is a
// configuration fact worth stating plainly to the user, the second is a bug
// worth logging.
var errTranscriptionUnavailable = errors.New("transcription provider not configured")

// videoReading is everything one downloaded video yields: the speech in it and
// a strip of sampled frames. The two are independent — a silent clip still has
// frames, and a clip whose video stream ffmpeg cannot decode still has speech
// — so each carries its own error instead of one failure declaring the whole
// message unreadable.
type videoReading struct {
	transcript    string
	transcriptErr error
	frames        []videoFrame
	framesErr     error
}

// readTelegramVideo downloads the file once and reads both tracks from it.
// Downloading twice would double the cost of a 100 MB screen recording for
// nothing.
func (g *Gateway) readTelegramVideo(
	ctx context.Context,
	client *telegram.Client,
	fileID string,
	duration time.Duration,
) (videoReading, error) {
	sourcePath, cleanup, err := client.DownloadFilePath(ctx, fileID, maxTelegramVideoSourceBytes)
	if err != nil {
		return videoReading{}, err
	}
	defer cleanup()

	var reading videoReading
	if g.whisper != nil && g.whisper.IsConfigured() {
		reading.transcript, reading.transcriptErr = g.transcribeVideoFile(ctx, sourcePath)
	} else {
		reading.transcriptErr = errTranscriptionUnavailable
	}

	if duration <= 0 {
		duration, reading.framesErr = probeVideoDuration(ctx, sourcePath)
	}
	if reading.framesErr == nil {
		reading.frames, reading.framesErr = extractVideoFrames(ctx, sourcePath, duration)
	}
	return reading, nil
}

func (g *Gateway) transcribeVideoFile(ctx context.Context, sourcePath string) (string, error) {
	audioParts, cleanup, err := extractVideoAudioParts(ctx, sourcePath)
	if err != nil {
		return "", err
	}
	defer cleanup()

	transcripts := make([]string, 0, len(audioParts))
	for _, partPath := range audioParts {
		info, statErr := os.Stat(partPath)
		if statErr != nil {
			return "", fmt.Errorf("stat extracted audio: %w", statErr)
		}
		if info.Size() > maxTranscriptionChunkBytes {
			return "", fmt.Errorf("extracted audio chunk too large: %d bytes", info.Size())
		}
		audio, readErr := os.ReadFile(partPath)
		if readErr != nil {
			return "", fmt.Errorf("read extracted audio: %w", readErr)
		}
		transcript, transcribeErr := g.whisper.Transcribe(ctx, audio, filepath.Base(partPath))
		if transcribeErr != nil {
			return "", transcribeErr
		}
		if transcript = strings.TrimSpace(transcript); transcript != "" {
			transcripts = append(transcripts, transcript)
		}
	}
	return strings.Join(transcripts, "\n"), nil
}

// extractVideoAudioParts compresses only the first audio stream and segments
// long recordings before the transcription API sees them. Eight minutes
// stays comfortably inside gpt-4o-mini-transcribe's audio-token context and,
// at 48 kbps, produces chunks of only about 2.9 MB.
func extractVideoAudioParts(ctx context.Context, sourcePath string) ([]string, func(), error) {
	ffmpeg, err := findFFmpeg()
	if err != nil {
		return nil, func() {}, err
	}

	dir, err := os.MkdirTemp("", "blueship-video-*")
	if err != nil {
		return nil, func() {}, fmt.Errorf("create video temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	outputPattern := filepath.Join(dir, "audio-%03d.m4a")

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ffmpeg,
		"-hide_banner",
		"-loglevel", "error",
		"-y",
		"-i", sourcePath,
		"-map", "0:a:0",
		"-vn",
		"-ac", "1",
		"-ar", "16000",
		"-c:a", "aac",
		"-b:a", "48k",
		"-f", "segment",
		"-segment_time", fmt.Sprintf("%d", videoAudioSegmentSeconds),
		"-reset_timestamps", "1",
		outputPattern,
	)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("extract video audio: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	parts, err := filepath.Glob(filepath.Join(dir, "audio-*.m4a"))
	if err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("list extracted audio: %w", err)
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		cleanup()
		return nil, func() {}, fmt.Errorf("extract video audio: empty output")
	}
	return parts, cleanup, nil
}

func findFFmpeg() (string, error) {
	if path, err := exec.LookPath("ffmpeg"); err == nil {
		return path, nil
	}
	const homebrewFFmpeg = "/opt/homebrew/bin/ffmpeg"
	if info, err := os.Stat(homebrewFFmpeg); err == nil && !info.IsDir() {
		return homebrewFFmpeg, nil
	}
	return "", fmt.Errorf("ffmpeg not found")
}
