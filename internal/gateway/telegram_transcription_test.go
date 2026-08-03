package gateway

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rasimio/blueship/internal/transport/telegram"
)

func TestTelegramTranscriptionInputForVideo(t *testing.T) {
	input, ok := telegramTranscriptionInputFor(&telegram.Message{
		Video: &telegram.Video{
			FileID:   "video-file",
			MimeType: "video/webm",
		},
	})
	if !ok {
		t.Fatal("telegramTranscriptionInputFor() did not recognize video")
	}
	if input.fileID != "video-file" {
		t.Fatalf("fileID = %q, want video-file", input.fileID)
	}
	if input.filename != "video.webm" {
		t.Fatalf("filename = %q, want video.webm", input.filename)
	}
}

func TestTelegramTranscriptionInputForVideoNote(t *testing.T) {
	input, ok := telegramTranscriptionInputFor(&telegram.Message{
		VideoNote: &telegram.VideoNote{FileID: "round-video"},
	})
	if !ok {
		t.Fatal("telegramTranscriptionInputFor() did not recognize video note")
	}
	if input.fileID != "round-video" {
		t.Fatalf("fileID = %q, want round-video", input.fileID)
	}
	if input.filename != "video-note.mp4" {
		t.Fatalf("filename = %q, want video-note.mp4", input.filename)
	}
}

// Frame sampling needs the duration to pick a frame rate, and the reader needs
// the sender's language to describe the picture in it. Both ride on the same
// update and are silently zero if the mapping drops them.
func TestTelegramTranscriptionInputCarriesDurationAndLanguage(t *testing.T) {
	input, ok := telegramTranscriptionInputFor(&telegram.Message{
		VideoNote: &telegram.VideoNote{FileID: "round-video", Duration: 12},
		From:      &telegram.User{ID: 42, LanguageCode: "ru"},
	})
	if !ok {
		t.Fatal("telegramTranscriptionInputFor() did not recognize video note")
	}
	if input.duration != 12*time.Second {
		t.Fatalf("duration = %v, want 12s", input.duration)
	}
	if input.language != "ru" {
		t.Fatalf("language = %q, want ru", input.language)
	}
}

func TestTelegramTranscriptionInputForMOV(t *testing.T) {
	input, ok := telegramTranscriptionInputFor(&telegram.Message{
		Video: &telegram.Video{
			FileID:   "quicktime-video",
			MimeType: "video/quicktime",
		},
	})
	if !ok {
		t.Fatal("telegramTranscriptionInputFor() did not recognize MOV video")
	}
	if input.filename != "video.mov" {
		t.Fatalf("filename = %q, want video.mov", input.filename)
	}
}

func TestVideoTurnBlockKeepsCaption(t *testing.T) {
	got := appendDocInline("Посмотри и скажи, что думаешь", videoTurnBlock("Проверка микрофона.", ""))
	want := "Посмотри и скажи, что думаешь\n\n" + videoBlockOpen + "\nsaid: Проверка микрофона." + videoBlockClose
	if got != want {
		t.Fatalf("video block with a caption = %q, want %q", got, want)
	}
}

func TestIsTranscribableVideoDocument(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		mimeType string
		want     bool
	}{
		{name: "mime", filename: "upload", mimeType: "video/mp4", want: true},
		{name: "extension", filename: "clip.WEBM", want: true},
		{name: "mov extension", filename: "clip.MOV", want: true},
		{name: "not video", filename: "notes.txt", mimeType: "text/plain", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTranscribableVideoDocument(tt.filename, tt.mimeType); got != tt.want {
				t.Fatalf("isTranscribableVideoDocument(%q, %q) = %v, want %v", tt.filename, tt.mimeType, got, tt.want)
			}
		})
	}
}

func TestVideoDocumentTranscriptionFilename(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		mimeType string
		want     string
	}{
		{name: "mp4", filename: "holiday.MP4", want: "video.mp4"},
		{name: "webm", filename: "recording.webm", want: "video.webm"},
		{name: "mpeg alias", filename: "legacy.mpg", want: "video.mpeg"},
		{name: "mime fallback", filename: "upload", mimeType: "video/webm", want: "video.webm"},
		{name: "mov", filename: "iphone.MOV", mimeType: "video/quicktime", want: "video.mov"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := videoDocumentTranscriptionFilename(tt.filename, tt.mimeType)
			if got != tt.want {
				t.Fatalf("videoDocumentTranscriptionFilename(%q, %q) = %q, want %q", tt.filename, tt.mimeType, got, tt.want)
			}
		})
	}
}

func TestPrepareVideoForTranscriptionExtractsMOVAudio(t *testing.T) {
	ffmpeg, err := findFFmpeg()
	if err != nil {
		t.Skipf("ffmpeg unavailable: %v", err)
	}

	dir := t.TempDir()
	movPath := filepath.Join(dir, "fixture.mov")
	cmd := exec.Command(
		ffmpeg,
		"-hide_banner",
		"-loglevel", "error",
		"-y",
		"-f", "lavfi",
		"-i", "sine=frequency=440:duration=0.25",
		"-c:a", "aac",
		movPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create MOV fixture: %v: %s", err, output)
	}
	parts, cleanup, err := extractVideoAudioParts(context.Background(), movPath)
	if err != nil {
		t.Fatalf("extractVideoAudioParts() error = %v", err)
	}
	defer cleanup()
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want 1", len(parts))
	}
	audio, err := os.ReadFile(parts[0])
	if err != nil {
		t.Fatalf("read audio part: %v", err)
	}
	mov, err := os.ReadFile(movPath)
	if err != nil {
		t.Fatalf("read MOV fixture: %v", err)
	}
	if len(audio) == 0 {
		t.Fatal("extractVideoAudioParts() returned empty audio")
	}
	if bytes.Equal(audio, mov) {
		t.Fatal("extractVideoAudioParts() returned original MOV bytes")
	}
}
