package telegram

import (
	"encoding/json"
	"testing"
)

func TestMessageUnmarshalVideoMedia(t *testing.T) {
	var update Update
	err := json.Unmarshal([]byte(`{
		"update_id": 1,
		"message": {
			"message_id": 2,
			"chat": {"id": 3, "type": "private"},
			"video": {"file_id": "regular", "duration": 7, "mime_type": "video/mp4", "file_size": 123},
			"video_note": {"file_id": "round", "duration": 4, "file_size": 45}
		}
	}`), &update)
	if err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if update.Message == nil || update.Message.Video == nil {
		t.Fatal("video was not unmarshaled")
	}
	if update.Message.Video.FileID != "regular" || update.Message.Video.MimeType != "video/mp4" {
		t.Fatalf("video = %#v", update.Message.Video)
	}
	if update.Message.VideoNote == nil || update.Message.VideoNote.FileID != "round" {
		t.Fatalf("video_note = %#v", update.Message.VideoNote)
	}
}
