package gateway

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rasimio/blueship/internal/transport/telegram"
)

const maxTelegramTranscriptionBytes int64 = 20 << 20

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

func prepareVideoForTranscription(
	ctx context.Context,
	video []byte,
	filename, mimeType string,
) ([]byte, string, error) {
	if !isMOVVideo(filename, mimeType) {
		return video, filename, nil
	}

	audio, err := extractMOVAudio(ctx, video)
	if err != nil {
		return nil, "", err
	}
	return audio, "video.m4a", nil
}

func isMOVVideo(filename, mimeType string) bool {
	return strings.EqualFold(strings.TrimSpace(mimeType), "video/quicktime") ||
		strings.EqualFold(filepath.Ext(strings.TrimSpace(filename)), ".mov")
}

func extractMOVAudio(ctx context.Context, video []byte) ([]byte, error) {
	ffmpeg, err := findFFmpeg()
	if err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp("", "blueship-video-*")
	if err != nil {
		return nil, fmt.Errorf("create video temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	inputPath := filepath.Join(dir, "input.mov")
	outputPath := filepath.Join(dir, "audio.m4a")
	if err := os.WriteFile(inputPath, video, 0o600); err != nil {
		return nil, fmt.Errorf("write video temp file: %w", err)
	}

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ffmpeg,
		"-hide_banner",
		"-loglevel", "error",
		"-y",
		"-i", inputPath,
		"-map", "0:a:0",
		"-vn",
		"-c:a", "aac",
		"-b:a", "96k",
		outputPath,
	)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("extract mov audio: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	audio, err := os.ReadFile(outputPath)
	if err != nil {
		return nil, fmt.Errorf("read extracted audio: %w", err)
	}
	if len(audio) == 0 {
		return nil, fmt.Errorf("extract mov audio: empty output")
	}
	return audio, nil
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
