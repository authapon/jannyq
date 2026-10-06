package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/authapon/jannyq/internal/channel"
)

// fileServer fakes the Bot API with files and update batches that are
// delivered one per getUpdates call, after a delay.
type fileServer struct {
	t       *testing.T
	mu      sync.Mutex
	batches []batch
	files   map[string][]byte // file_id -> content
	sizes   map[string]int64  // size reported by getFile (default: len)
	failGet bool              // downloads answer 500
	calls   atomic.Int32      // getFile and download requests
}

type batch struct {
	after   time.Duration
	updates string
}

func (f *fileServer) handler(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/file/bot"+token+"/"):
		f.calls.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failGet {
			w.WriteHeader(500)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/file/bot"+token+"/files/")
		w.Write(f.files[id])
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		io.WriteString(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"Janny","username":"JannyBot"}}`)
	case strings.HasSuffix(r.URL.Path, "/getUpdates"):
		f.mu.Lock()
		var b *batch
		if len(f.batches) > 0 {
			b = &f.batches[0]
			f.batches = f.batches[1:]
		}
		f.mu.Unlock()
		if b == nil {
			select {
			case <-r.Context().Done():
			case <-time.After(20 * time.Millisecond):
			}
			io.WriteString(w, `{"ok":true,"result":[]}`)
			return
		}
		time.Sleep(b.after)
		io.WriteString(w, `{"ok":true,"result":[`+b.updates+`]}`)
	case strings.HasSuffix(r.URL.Path, "/getFile"):
		f.calls.Add(1)
		var body struct {
			FileID string `json:"file_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		data, ok := f.files[body.FileID]
		if !ok {
			io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: invalid file_id"}`)
			return
		}
		size := int64(len(data))
		if s, ok := f.sizes[body.FileID]; ok {
			size = s
		}
		fmt.Fprintf(w, `{"ok":true,"result":{"file_id":%q,"file_size":%d,"file_path":"files/%s"}}`, body.FileID, size, body.FileID)
	case strings.HasSuffix(r.URL.Path, "/sendChatAction"), strings.HasSuffix(r.URL.Path, "/sendMessage"):
		io.WriteString(w, `{"ok":true,"result":true}`)
	default:
		f.t.Errorf("unexpected request %s", r.URL.Path)
	}
}

func (f *fileServer) start(t *testing.T) *Channel {
	f.t = t
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return New(Config{Token: token, APIBase: srv.URL, PollTimeout: time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func photoUpdate(id int, chat int64, group, caption string, fileIDs ...string) string {
	var sizes []string
	for i, fid := range fileIDs {
		sizes = append(sizes, fmt.Sprintf(`{"file_id":%q,"width":%d,"height":%d,"file_size":%d}`, fid, 90*(i+1), 90*(i+1), 1000*(i+1)))
	}
	g := ""
	if group != "" {
		g = `,"media_group_id":"` + group + `"`
	}
	c := ""
	if caption != "" {
		c = `,"caption":"` + caption + `"`
	}
	return fmt.Sprintf(`{"update_id":%d,"message":{"message_id":%d,"date":%d,"from":{"id":5,"first_name":"Ann"},"chat":{"id":%d,"type":"private"},"photo":[%s]%s%s}}`,
		id, id, 1700000000+id, chat, strings.Join(sizes, ","), g, c)
}

func runChannel(t *testing.T, c *Channel, want int) []channel.Incoming {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var got []channel.Incoming
	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, func(_ context.Context, in channel.Incoming) {
			in.Accepted()
			mu.Lock()
			got = append(got, in)
			mu.Unlock()
		})
	}()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= want {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond) // anything extra would show up
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]channel.Incoming(nil), got...)
}

func TestPhotoAndDocumentAreFetched(t *testing.T) {
	f := &fileServer{files: map[string][]byte{"small": []byte("tiny"), "large": []byte("the big picture"), "doc1": []byte("%PDF-1.4 hello")}}
	c := f.start(t)
	f.batches = []batch{{updates: photoUpdate(1, 5, "", "look at this", "small", "large") + `,` +
		`{"update_id":2,"message":{"message_id":2,"date":1700000002,"from":{"id":5,"first_name":"Ann"},"chat":{"id":5,"type":"private"},` +
		`"document":{"file_id":"doc1","file_name":"report.pdf","mime_type":"application/pdf","file_size":14}}}`}}
	got := runChannel(t, c, 2)
	if len(got) != 2 {
		t.Fatalf("got %d messages", len(got))
	}
	p := got[0]
	if p.Text != "look at this" || len(p.Attachments) != 1 || p.HasAttachment || p.Attachments[0].MIME != "image/jpeg" ||
		!strings.HasSuffix(p.Attachments[0].Name, ".jpg") {
		t.Fatalf("photo message = %+v", p)
	}
	data, err := p.Attachments[0].Fetch(context.Background(), 1<<20)
	if err != nil || string(data) != "the big picture" {
		t.Errorf("the largest size must be fetched: %q %v", data, err)
	}
	d := got[1]
	if len(d.Attachments) != 1 || d.Attachments[0].Name != "report.pdf" || d.Attachments[0].MIME != "application/pdf" || d.Attachments[0].Size != 14 {
		t.Fatalf("document message = %+v", d)
	}
	if data, err := d.Attachments[0].Fetch(context.Background(), 1<<20); err != nil || string(data) != "%PDF-1.4 hello" {
		t.Errorf("document: %q %v", data, err)
	}
}

func TestFileSizeLimits(t *testing.T) {
	f := &fileServer{files: map[string][]byte{"big": bytes.Repeat([]byte("x"), 5000), "liar": bytes.Repeat([]byte("y"), 5000)},
		sizes: map[string]int64{"liar": 10}} // getFile claims 10 bytes, the download is 5000
	c := f.start(t)
	ctx := context.Background()

	// declared size over the limit: nothing is requested at all
	if _, err := c.fetcher("big", 5000)(ctx, 1000); !errors.Is(err, channel.ErrTooLarge) || f.calls.Load() != 0 {
		t.Errorf("declared: %v, %d requests", err, f.calls.Load())
	}
	// getFile reports a size over the limit: not downloaded
	if _, err := c.fetcher("big", 0)(ctx, 1000); !errors.Is(err, channel.ErrTooLarge) || f.calls.Load() != 1 {
		t.Errorf("getFile size: %v, %d requests", err, f.calls.Load())
	}
	// a server that understates the size is still cut off
	if _, err := c.fetcher("liar", 0)(ctx, 1000); !errors.Is(err, channel.ErrTooLarge) {
		t.Errorf("understated size: %v", err)
	}
	// within the limit
	if data, err := c.fetcher("big", 0)(ctx, 5000); err != nil || len(data) != 5000 {
		t.Errorf("exact limit: %d %v", len(data), err)
	}
}

func TestDownloadErrorsDoNotLeakTheToken(t *testing.T) {
	f := &fileServer{files: map[string][]byte{"a": []byte("x")}, failGet: true}
	c := f.start(t)
	_, err := c.fetcher("a", 0)(context.Background(), 100)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("err = %v", err)
	}
	// a connection error carries the URL, which holds the token
	c2 := New(Config{Token: token, APIBase: "http://127.0.0.1:1"}, nil)
	_, err = c2.fetcher("a", 0)(context.Background(), 100)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("err = %v", err)
	}
	if _, err := c.fetcher("missing", 0)(context.Background(), 100); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("unknown file: %v", err)
	}
}

func TestAnAlbumBecomesOneMessage(t *testing.T) {
	f := &fileServer{files: map[string][]byte{"p1": []byte("one"), "p2": []byte("two"), "p3": []byte("three")}}
	c := f.start(t)
	// the pictures arrive in separate getUpdates calls; the caption is on the second;
	// a text message follows, and another album of the same chat comes after it
	f.batches = []batch{
		{updates: photoUpdate(1, 5, "A", "", "p1")},
		{after: 100 * time.Millisecond, updates: photoUpdate(2, 5, "A", "three pictures", "p2")},
		{after: 100 * time.Millisecond, updates: photoUpdate(3, 5, "A", "", "p3")},
		{after: 100 * time.Millisecond, updates: `{"update_id":4,"message":{"message_id":4,"date":1700000004,"from":{"id":5,"first_name":"Ann"},"chat":{"id":5,"type":"private"},"text":"after"}}`},
		{updates: photoUpdate(5, 5, "B", "", "p1")},
	}
	got := runChannel(t, c, 3)
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3", len(got))
	}
	a := got[0]
	if len(a.Attachments) != 3 || a.Text != "three pictures" || !a.Addressed {
		t.Fatalf("album = %d attachments, text %q", len(a.Attachments), a.Text)
	}
	var contents []string
	for _, at := range a.Attachments {
		d, err := at.Fetch(context.Background(), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		contents = append(contents, string(d))
	}
	if strings.Join(contents, ",") != "one,two,three" {
		t.Errorf("album order = %v", contents)
	}
	if got[1].Text != "after" || len(got[2].Attachments) != 1 {
		t.Errorf("messages after the album: %+v / %+v", got[1].Text, got[2].Attachments)
	}
}

func TestAlbumsOfDifferentChatsStaySeparate(t *testing.T) {
	f := &fileServer{files: map[string][]byte{"p1": []byte("one"), "p2": []byte("two")}}
	c := f.start(t)
	f.batches = []batch{{updates: photoUpdate(1, 5, "A", "", "p1") + "," + photoUpdate(2, 6, "A", "", "p2")}}
	got := runChannel(t, c, 2)
	if len(got) != 2 || len(got[0].Attachments) != 1 || len(got[1].Attachments) != 1 || got[0].ChatID == got[1].ChatID {
		t.Fatalf("got %+v", got)
	}
}

func TestUnsupportedMediaIsFlagged(t *testing.T) {
	c := newChannel("http://unused")
	user := &tgUser{ID: 5, FirstName: "Ann"}
	in, _ := c.convert(&tgMessage{From: user, Chat: tgChat{ID: 5, Type: "private"}, Video: json.RawMessage(`{"file_id":"v"}`)})
	if !in.HasAttachment || len(in.Attachments) != 0 {
		t.Errorf("video: %+v", in)
	}
	in, _ = c.convert(&tgMessage{From: user, Chat: tgChat{ID: 5, Type: "private"}, Document: &tgDocument{FileID: "d", FileName: "a.pdf"}})
	if in.HasAttachment || len(in.Attachments) != 1 {
		t.Errorf("document: %+v", in)
	}
}
