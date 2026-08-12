package gateway

import (
	"strings"
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
)

// Passes and tool names move together. The failure this pins is quiet: a turn
// granted tools but only one pass calls something, never gets a second pass to
// speak, and the person receives nothing at all.
func TestAutonomousTurnToolConfig(t *testing.T) {
	t.Run("no tools keeps the single-pass turn", func(t *testing.T) {
		passes, override, allowed := autonomousTurnToolConfig(nil)
		if passes != 1 {
			t.Fatalf("passes = %d, want 1", passes)
		}
		if len(override) != 0 || len(allowed) != 0 {
			t.Fatalf("tool lists = %v / %v, want empty", override, allowed)
		}
	})

	t.Run("tools open the extra passes they need", func(t *testing.T) {
		passes, override, allowed := autonomousTurnToolConfig([]string{"image_generate", "attachment_include"})
		if passes < 2 {
			t.Fatalf("passes = %d — a tool call leaves nothing to answer with", passes)
		}
		if len(override) != 2 || len(allowed) != 2 {
			t.Fatalf("tool lists = %v / %v, want both names", override, allowed)
		}
	})

	t.Run("blank entries do not buy passes", func(t *testing.T) {
		passes, _, allowed := autonomousTurnToolConfig([]string{"  ", ""})
		if passes != 1 || len(allowed) != 0 {
			t.Fatalf("passes = %d, allowed = %v — whitespace was taken for a tool", passes, allowed)
		}
	})
}

// A soul that draws has to be able to hand the picture over.
//
// The ordinary agent-task notify path resolves `[attached: UUID]` into a file
// send; this one shipped the sentinel as literal text. Production showed
// exactly that: the marker in the chat, the 2.6 MB image sitting in the store,
// and a message that announced a drawing nobody received.
func TestAutonomousCommitSeparatesMarkersFromText(t *testing.T) {
	const drawn = "b97e0477-7f3b-495e-9fed-0c098de95acf"

	t.Run("marker is stripped and the id recovered", func(t *testing.T) {
		ids, cleaned, ok := bs.ParseAttachmentMarkers(
			"Вот он ты — каким я тебя знаю (^_^)\n\n[attached: " + drawn + "]")
		if !ok || len(ids) != 1 || ids[0].String() != drawn {
			t.Fatalf("ids = %v, ok = %v", ids, ok)
		}
		if strings.Contains(cleaned, "attached:") {
			t.Fatalf("marker survived into the delivered text: %q", cleaned)
		}
		if !strings.Contains(cleaned, "каким я тебя знаю") {
			t.Fatalf("stripping ate the message: %q", cleaned)
		}
	})

	t.Run("text without markers is untouched", func(t *testing.T) {
		const plain = "просто сообщение без вложений"
		ids, cleaned, ok := bs.ParseAttachmentMarkers(plain)
		if ok || len(ids) != 0 || cleaned != plain {
			t.Fatalf("plain text was rewritten: ok=%v ids=%v cleaned=%q", ok, ids, cleaned)
		}
	})

	t.Run("a message that is only a marker has nothing to say", func(t *testing.T) {
		_, cleaned, ok := bs.ParseAttachmentMarkers("[attached: " + drawn + "]")
		if !ok {
			t.Fatal("marker not recognised")
		}
		// The commit path refuses this rather than sending an empty message or
		// inventing a caption on the soul's behalf.
		if strings.TrimSpace(cleaned) != "" {
			t.Fatalf("cleaned = %q, want empty", cleaned)
		}
	})
}

// The soul's own ceiling survives a tool request.
//
// A perception naming image_generate must not hand it to a soul whose owner
// switched drawing off in the cabinet. The request narrows what the turn may
// use; it never widens it.
func TestIntersectTools(t *testing.T) {
	ceiling := []string{"image_generate", "attachment_include", "notes"}

	got := intersectTools([]string{"image_generate", "web_search"}, ceiling)
	if len(got) != 1 || got[0] != "image_generate" {
		t.Fatalf("intersect = %v — a disabled tool was granted", got)
	}

	if got := intersectTools([]string{"web_search"}, ceiling); len(got) != 0 {
		t.Fatalf("intersect = %v, want nothing the soul may not use", got)
	}

	// No policy configured is the framework-consumer case: there is nothing to
	// filter against, which is not the same as filtering everything out.
	if got := intersectTools([]string{"image_generate"}, nil); len(got) != 1 {
		t.Fatalf("intersect against a nil ceiling = %v, want the request intact", got)
	}

	if got := intersectTools(nil, ceiling); len(got) != 0 {
		t.Fatalf("intersect = %v, want nothing when nothing was asked for", got)
	}
}
