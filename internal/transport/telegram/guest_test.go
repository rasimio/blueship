package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

type recordedCall struct {
	method  string
	payload map[string]any
}

// recordingClient answers every call with the body respond picks for it and
// keeps what was sent, so the assertions read as "what Telegram was told".
func recordingClient(t *testing.T, respond func(n int, method string) (int, string)) (*Client, *[]recordedCall) {
	t.Helper()
	var calls []recordedCall
	c := testClient(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		method := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		calls = append(calls, recordedCall{method: method, payload: payload})
		status, resp := respond(len(calls), method)
		return jsonResponse(status, resp), nil
	})
	return c, &calls
}

func TestAnswerGuestQuerySendsOneArticleAndReturnsInlineID(t *testing.T) {
	c, calls := recordingClient(t, func(int, string) (int, string) {
		return http.StatusOK, `{"ok":true,"result":{"inline_message_id":"AAQ-guest"}}`
	})
	id, err := c.AnswerGuestQuery(context.Background(), "gq-1", "**hi**")
	if err != nil {
		t.Fatal(err)
	}
	if id != "AAQ-guest" {
		t.Fatalf("inline id = %q", id)
	}
	if len(*calls) != 1 || (*calls)[0].method != "answerGuestQuery" {
		t.Fatalf("calls = %+v", *calls)
	}
	p := (*calls)[0].payload
	if p["guest_query_id"] != "gq-1" {
		t.Fatalf("guest_query_id = %v", p["guest_query_id"])
	}
	result := p["result"].(map[string]any)
	if result["type"] != "article" {
		t.Fatalf("result type = %v", result["type"])
	}
	content := result["input_message_content"].(map[string]any)
	if content["parse_mode"] != "HTML" || content["message_text"] != "<b>hi</b>" {
		t.Fatalf("content = %v", content)
	}
}

// Without an inline id every later edit has nowhere to go; that has to be
// an error at the answer, not a silent stream into nothing.
func TestAnswerGuestQueryWithoutInlineIDIsAnError(t *testing.T) {
	c, _ := recordingClient(t, func(int, string) (int, string) {
		return http.StatusOK, `{"ok":true,"result":{}}`
	})
	if _, err := c.AnswerGuestQuery(context.Background(), "gq-1", "hi"); err == nil {
		t.Fatal("want an error when Telegram reports no inline message")
	}
}

func TestAnswerGuestQueryResendsLiteralTextWhenMarkupIsRejected(t *testing.T) {
	c, calls := recordingClient(t, func(n int, _ string) (int, string) {
		if n == 1 {
			return http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`
		}
		return http.StatusOK, `{"ok":true,"result":{"inline_message_id":"AAQ"}}`
	})
	if _, err := c.AnswerGuestQuery(context.Background(), "gq", "a_b"); err != nil {
		t.Fatal(err)
	}
	content := (*calls)[1].payload["result"].(map[string]any)["input_message_content"].(map[string]any)
	if content["message_text"] != "a_b" || content["parse_mode"] != nil {
		t.Fatalf("fallback content = %v", content)
	}
}

func TestEditInlineMessageTextTargetsTheInlineMessage(t *testing.T) {
	c, calls := recordingClient(t, func(int, string) (int, string) {
		return http.StatusOK, `{"ok":true,"result":true}`
	})
	if err := c.EditInlineMessageText(context.Background(), "AAQ", "text"); err != nil {
		t.Fatal(err)
	}
	p := (*calls)[0].payload
	if (*calls)[0].method != "editMessageText" || p["inline_message_id"] != "AAQ" {
		t.Fatalf("call = %+v", (*calls)[0])
	}
	if _, ok := p["chat_id"]; ok {
		t.Fatal("an inline edit must not name a chat")
	}
}

func TestEditInlineMessageTextIgnoresNotModified(t *testing.T) {
	c, _ := recordingClient(t, func(int, string) (int, string) {
		return http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`
	})
	if err := c.EditInlineMessageText(context.Background(), "AAQ", "same"); err != nil {
		t.Fatalf("not-modified is not a failure: %v", err)
	}
}

func TestFinalizeInlineResponseKeepsShortPlainAnswersPlain(t *testing.T) {
	c, calls := recordingClient(t, func(int, string) (int, string) {
		return http.StatusOK, `{"ok":true,"result":true}`
	})
	if err := c.FinalizeInlineResponse(context.Background(), "AAQ", "short answer"); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %d", len(*calls))
	}
	if _, ok := (*calls)[0].payload["rich_message"]; ok {
		t.Fatal("a short plain answer must stay copyable text")
	}
}

// A guest answer is one message. What does not fit is cut, and the cut
// still has to be something Telegram accepts.
func TestFinalizeInlineResponseCutsWhatRichAndPlainCannotHold(t *testing.T) {
	c, calls := recordingClient(t, func(int, string) (int, string) {
		return http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse rich message"}`
	})
	long := strings.Repeat("я", maxTelegramMessageLength+100)
	_ = c.FinalizeInlineResponse(context.Background(), "AAQ", long)
	var plain string
	for _, call := range *calls {
		if text, ok := call.payload["text"].(string); ok {
			plain = text
		}
	}
	if plain == "" {
		t.Fatal("no plain fallback was attempted")
	}
	if n := utf8.RuneCountInString(plain); n > maxTelegramMessageLength {
		t.Fatalf("plain fallback is %d runes, over the %d limit", n, maxTelegramMessageLength)
	}
	if !strings.HasSuffix(plain, "…") {
		t.Fatal("the cut is not marked")
	}
}

func TestUploadForInlineSendsSilentlyAndReturnsTheLargestPhoto(t *testing.T) {
	var method string
	var fields map[string]string
	c := testClient(func(req *http.Request) (*http.Response, error) {
		method = req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		if err := req.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		fields = map[string]string{}
		for k, v := range req.MultipartForm.Value {
			fields[k] = v[0]
		}
		return jsonResponse(http.StatusOK, `{"ok":true,"result":{"message_id":55,
			"photo":[{"file_id":"small","width":90},{"file_id":"large","width":1280}]}}`), nil
	})
	media, staged, err := c.UploadForInline(context.Background(), 4242, "photo", "art.png", "image/png", []byte("png"))
	if err != nil {
		t.Fatal(err)
	}
	if method != "sendPhoto" || fields["chat_id"] != "4242" || fields["disable_notification"] != "true" {
		t.Fatalf("method = %s fields = %v", method, fields)
	}
	if media != (InlineMedia{Kind: "photo", FileID: "large"}) || staged != 55 {
		t.Fatalf("media = %+v staged = %d", media, staged)
	}
}

func TestUploadForInlineDocument(t *testing.T) {
	c := testClient(func(req *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(req.URL.Path, "/sendDocument") {
			t.Fatalf("path = %s", req.URL.Path)
		}
		return jsonResponse(http.StatusOK, `{"ok":true,"result":{"message_id":56,"document":{"file_id":"doc"}}}`), nil
	})
	media, staged, err := c.UploadForInline(context.Background(), 1, "document", "r.md", "text/markdown", []byte("# r"))
	if err != nil || media != (InlineMedia{Kind: "document", FileID: "doc"}) || staged != 56 {
		t.Fatalf("media = %+v staged = %d err = %v", media, staged, err)
	}
}

// One picture under a short answer is a photo with a caption — the way
// Telegram shows a picture with words, and the caption stays copyable.
func TestFinalizeInlineWithMediaShortAnswerIsAPhotoWithCaption(t *testing.T) {
	c, calls := recordingClient(t, func(int, string) (int, string) {
		return http.StatusOK, `{"ok":true,"result":true}`
	})
	photo := InlineMedia{Kind: "photo", FileID: "large"}
	if err := c.FinalizeInlineWithMedia(context.Background(), "AAQ", "**look**", []InlineMedia{photo}); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || (*calls)[0].method != "editMessageMedia" {
		t.Fatalf("calls = %+v", *calls)
	}
	p := (*calls)[0].payload
	media := p["media"].(map[string]any)
	if p["inline_message_id"] != "AAQ" || media["type"] != "photo" || media["media"] != "large" || media["caption"] != "<b>look</b>" {
		t.Fatalf("payload = %v", p)
	}
}

// Past a caption's length the answer and the picture go in together as a
// rich message, the picture referenced by the file id it was uploaded as.
func TestFinalizeInlineWithMediaLongAnswerIsRichWithTheFileEmbedded(t *testing.T) {
	c, calls := recordingClient(t, func(int, string) (int, string) {
		return http.StatusOK, `{"ok":true,"result":true}`
	})
	long := strings.Repeat("слово ", 300)
	if err := c.FinalizeInlineWithMedia(context.Background(), "AAQ", long, []InlineMedia{{Kind: "photo", FileID: "large"}}); err != nil {
		t.Fatal(err)
	}
	if (*calls)[0].method != "editMessageText" {
		t.Fatalf("calls = %+v", *calls)
	}
	rich := (*calls)[0].payload["rich_message"].(map[string]any)
	if !strings.Contains(rich["markdown"].(string), "![](tg://photo?id=m1)") {
		t.Fatalf("markdown does not embed the photo: %q", rich["markdown"])
	}
	items := rich["media"].([]any)
	item := items[0].(map[string]any)
	if item["id"] != "m1" || item["media"].(map[string]any)["media"] != "large" {
		t.Fatalf("media = %v", items)
	}
}

// Refused rich still gets the picture there: it is what was asked for, and
// the full text is in the history.
func TestFinalizeInlineWithMediaFallsBackToThePictureWithACutCaption(t *testing.T) {
	c, calls := recordingClient(t, func(n int, method string) (int, string) {
		if method == "editMessageText" {
			return http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse rich message"}`
		}
		return http.StatusOK, `{"ok":true,"result":true}`
	})
	long := strings.Repeat("я", 3000)
	if err := c.FinalizeInlineWithMedia(context.Background(), "AAQ", long, []InlineMedia{{Kind: "photo", FileID: "large"}}); err != nil {
		t.Fatal(err)
	}
	last := (*calls)[len(*calls)-1]
	if last.method != "editMessageMedia" {
		t.Fatalf("last call = %s", last.method)
	}
	caption := last.payload["media"].(map[string]any)["caption"].(string)
	if n := utf8.RuneCountInString(caption); n > maxCaptionLength {
		t.Fatalf("caption is %d runes, over %d", n, maxCaptionLength)
	}
}
