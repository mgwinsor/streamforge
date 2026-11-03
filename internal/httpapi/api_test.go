package httpapi

import (
	"bytes"
	"context"
	"errors"
	"github.com/mgwinsor/streamforge/internal/store"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

type fakeDB struct {
	fail   bool
	videos []store.Video
}

func (d *fakeDB) Insert(_ context.Context, v store.Video) (store.Video, error) {
	if d.fail {
		return v, errors.New("db down")
	}
	v.Status = "queued"
	d.videos = append(d.videos, v)
	return v, nil
}
func (d *fakeDB) Get(context.Context, string) (store.Video, error) {
	return store.Video{}, store.ErrNotFound
}
func (d *fakeDB) List(context.Context) ([]store.Video, error) { return d.videos, nil }
func (d *fakeDB) Ping(context.Context) error                  { return nil }
func TestUpload(t *testing.T) {
	for _, dbFail := range []bool{false, true} {
		t.Run(map[bool]string{true: "insert_failure", false: "queued"}[dbFail], func(t *testing.T) {
			d := &fakeDB{fail: dbFail}
			root := t.TempDir()
			a := &API{DB: d, Sources: root, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			var body bytes.Buffer
			m := multipart.NewWriter(&body)
			f, _ := m.CreateFormFile("file", "../../outside.mp4")
			f.Write([]byte("not yet validated"))
			m.WriteField("title", "Demo")
			m.Close()
			r := httptest.NewRequest("POST", "/api/videos", &body)
			r.Header.Set("Content-Type", m.FormDataContentType())
			w := httptest.NewRecorder()
			a.Handler().ServeHTTP(w, r)
			entries, _ := os.ReadDir(root)
			if dbFail {
				if w.Code != 500 || len(entries) != 0 {
					t.Fatalf("code=%d files=%v", w.Code, entries)
				}
				return
			}
			if w.Code != 202 || len(entries) != 1 || len(d.videos) != 1 {
				t.Fatalf("code=%d files=%v", w.Code, entries)
			}
			if strings.Contains(w.Body.String(), root) {
				t.Fatal("exposed path")
			}
			if !validID(strings.TrimSuffix(entries[0].Name(), ".mp4")) {
				t.Fatal("untrusted filename")
			}
		})
	}
}

type repeatingReader struct{}

func (repeatingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
func TestOversizeStreamCleanup(t *testing.T) {
	root := t.TempDir()
	d := &fakeDB{}
	a := &API{DB: d, Sources: root, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	// Unknown Content-Length exercises streaming enforcement, not just the header check.
	prefix := "--boundary\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.mp4\"\r\n\r\n"
	r := httptest.NewRequest(http.MethodPost, "/api/videos", io.MultiReader(strings.NewReader(prefix), io.LimitReader(repeatingReader{}, MaxUpload+1)))
	r.ContentLength = -1
	r.Header.Set("Content-Type", "multipart/form-data; boundary=boundary")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	entries, _ := os.ReadDir(root)
	if w.Code != 413 || len(entries) != 0 || len(d.videos) != 0 {
		t.Fatalf("code=%d files=%v rows=%v", w.Code, entries, d.videos)
	}
}
func TestIncompleteCleanup(t *testing.T) {
	root := t.TempDir()
	a := &API{DB: &fakeDB{}, Sources: root, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := httptest.NewRequest("POST", "/api/videos", strings.NewReader("--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a\"\r\n\r\ntruncated"))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	entries, _ := os.ReadDir(root)
	if w.Code != 400 || len(entries) != 0 {
		t.Fatalf("code=%d files=%v", w.Code, entries)
	}
}
