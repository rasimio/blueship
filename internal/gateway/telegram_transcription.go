package gateway

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

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
		}, true
	case msg.VideoNote != nil && msg.VideoNote.FileID != "":
		return telegramTranscriptionInput{
			fileID:   msg.VideoNote.FileID,
			filename: "video-note.mp4",
			mimeType: "video/mp4",
			kind:     "video",
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

func (g *Gateway) transcribeTelegramVideo(
	ctx context.Context,
	client *telegram.Client,
	fileID string,
) (string, error) {
	sourcePath, cleanup, err := client.DownloadFilePath(ctx, fileID, maxTelegramVideoSourceBytes)
	if err != nil {
		return "", err
	}
	defer cleanup()
	return g.transcribeVideoFile(ctx, sourcePath)
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
