package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeBotAPI records the form of every request and answers with reply.
type fakeBotAPI struct {
	srv    *httptest.Server
	method string
	form   map[string]string
	photo  []byte
	reply  func(w http.ResponseWriter)
}

func newFakeBotAPI(t *testing.T, reply func(w http.ResponseWriter)) *fakeBotAPI {
	f := &fakeBotAPI{reply: reply}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.method = r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse form: %v", err)
		}
		f.form = map[string]string{}
		f.photo = nil
		for k, v := range r.MultipartForm.Value {
			f.form[k] = v[0]
		}
		if fh := r.MultipartForm.File["photo"]; len(fh) > 0 {
			file, _ := fh[0].Open()
			f.photo, _ = io.ReadAll(file)
		}
		f.reply(w)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func okMessage(w http.ResponseWriter) {
	_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
}

func TestSendPreparedTextIsHTMLAsGiven(t *testing.T) {
	api := newFakeBotAPI(t, okMessage)
	c := NewClientWithAPIURL("123:SECRET", api.srv.URL, 5*time.Second)
	res, err := c.SendPrepared(context.Background(), PreparedMessage{
		ChatID: 77, HTML: "<b>Привет</b>, **не markdown**", Silent: true,
		Rows: [][]InlineKeyboardButton{{{Text: "Сайт", URL: "https://vaelum.ai"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.MessageID != 42 {
		t.Fatalf("message id = %d", res.MessageID)
	}
	if api.method != "sendMessage" {
		t.Fatalf("method = %s", api.method)
	}
	if api.form["text"] != "<b>Привет</b>, **не markdown**" || api.form["parse_mode"] != "HTML" {
		t.Fatalf("text changed on the way: %q (%s)", api.form["text"], api.form["parse_mode"])
	}
	if api.form["disable_notification"] != "true" || api.form["chat_id"] != "77" {
		t.Fatalf("form = %v", api.form)
	}
	if _, paid := api.form["allow_paid_broadcast"]; paid {
		t.Fatal("paid broadcast must never be switched on")
	}
	var markup struct {
		InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
	}
	if err := json.Unmarshal([]byte(api.form["reply_markup"]), &markup); err != nil || markup.InlineKeyboard[0][0].URL != "https://vaelum.ai" {
		t.Fatalf("keyboard = %q (%v)", api.form["reply_markup"], err)
	}
}

func TestSendPreparedUploadsPhotoAndReportsItsID(t *testing.T) {
	api := newFakeBotAPI(t, func(w http.ResponseWriter) {
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":5,"photo":[{"file_id":"small"},{"file_id":"large"}]}}`)
	})
	c := NewClientWithAPIURL("123:SECRET", api.srv.URL, 5*time.Second)
	res, err := c.SendPrepared(context.Background(), PreparedMessage{
		ChatID: 1, HTML: "подпись", Photo: &PhotoUpload{Name: "a.png", MIME: "image/png", Data: []byte("PNG")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if api.method != "sendPhoto" || api.form["caption"] != "подпись" || string(api.photo) != "PNG" {
		t.Fatalf("method=%s form=%v photo=%q", api.method, api.form, api.photo)
	}
	if res.PhotoFileID != "large" {
		t.Fatalf("photo file id = %q, want the largest size", res.PhotoFileID)
	}

	if _, err := c.SendPrepared(context.Background(), PreparedMessage{ChatID: 1, HTML: "x", PhotoFileID: "large"}); err != nil {
		t.Fatal(err)
	}
	if api.form["photo"] != "large" || api.photo != nil {
		t.Fatalf("resend did not reuse the file id: %v", api.form)
	}
}

func TestSendPreparedRateLimitPausesBulk(t *testing.T) {
	api := newFakeBotAPI(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 7","parameters":{"retry_after":7}}`)
	})
	c := NewClientWithAPIURL("123:SECRET", api.srv.URL, 5*time.Second)
	_, err := c.SendPrepared(WithBulk(context.Background()), PreparedMessage{ChatID: 1, HTML: "x"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Parameters.RetryAfter != 7 {
		t.Fatalf("err = %v", err)
	}
	if d := c.limiter.take(true); d < 6*time.Second {
		t.Fatalf("bulk not paused after 429: wait %v", d)
	}
}

func TestSendPreparedNeverLeaksTheToken(t *testing.T) {
	// Nothing listens here: the request cannot be written.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	c := NewClientWithAPIURL("123:SECRET", "http://"+addr, time.Second)
	_, err = c.SendPrepared(context.Background(), PreparedMessage{ChatID: 1, HTML: "x"})
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("err = %T %v", err, err)
	}
	if te.Written {
		t.Fatal("a refused connection reported the request as written")
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("token in error: %v", err)
	}
}

func TestSendPreparedTimeoutAfterWriteIsAmbiguous(t *testing.T) {
	api := newFakeBotAPI(t, func(w http.ResponseWriter) { time.Sleep(300 * time.Millisecond); okMessage(w) })
	c := NewClientWithAPIURL("123:SECRET", api.srv.URL, 100*time.Millisecond)
	_, err := c.SendPrepared(context.Background(), PreparedMessage{ChatID: 1, HTML: "x"})
	var te *TransportError
	if !errors.As(err, &te) || !te.Written {
		t.Fatalf("err = %v, want a written transport error", err)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("token in error: %v", err)
	}
}

func TestSendPreparedRefusesTwoPhotos(t *testing.T) {
	c := NewClientWithAPIURL("123:SECRET", "http://127.0.0.1:1", time.Second)
	if _, err := c.SendPrepared(context.Background(), PreparedMessage{ChatID: 1, PhotoFileID: "a", Photo: &PhotoUpload{}}); err == nil {
		t.Fatal("accepted two photos")
	}
}
