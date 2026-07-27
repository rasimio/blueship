package gateway

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

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

func TestAppendVideoTranscriptKeepsCaption(t *testing.T) {
	got := appendVideoTranscript("Посмотри и скажи, что думаешь", "Проверка микрофона.")
	want := "Посмотри и скажи, что думаешь\n\n[video transcript]\nПроверка микрофона."
	if got != want {
		t.Fatalf("appendVideoTranscript() = %q, want %q", got, want)
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
	mov, err := os.ReadFile(movPath)
	if err != nil {
		t.Fatalf("read MOV fixture: %v", err)
	}

	audio, filename, err := prepareVideoForTranscription(
		context.Background(), mov, "iphone.mov", "video/quicktime",
	)
	if err != nil {
		t.Fatalf("prepareVideoForTranscription() error = %v", err)
	}
	if filename != "video.m4a" {
		t.Fatalf("filename = %q, want video.m4a", filename)
	}
	if len(audio) == 0 {
		t.Fatal("prepareVideoForTranscription() returned empty audio")
	}
	if bytes.Equal(audio, mov) {
		t.Fatal("prepareVideoForTranscription() returned original MOV bytes")
	}
}
