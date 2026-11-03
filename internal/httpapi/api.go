package httpapi

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/mgwinsor/streamforge/internal/store"
)

//go:embed index.html
var page embed.FS

const MaxUpload = 100 << 20

type Repository interface {
	Insert(context.Context, store.Video) (store.Video, error)
	Get(context.Context, string) (store.Video, error)
	List(context.Context) ([]store.Video, error)
	Ping(context.Context) error
}

type API struct {
	DB      Repository
	Sources string
	Log     *slog.Logger
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/videos", a.upload)
	mux.HandleFunc("GET /api/videos", func(w http.ResponseWriter, r *http.Request) {
		vs, err := a.DB.List(r.Context())
		if err != nil {
			a.internal(w, err)
			return
		}
		writeJSON(w, 200, vs)
	})
	mux.HandleFunc("GET /api/videos/{id}", a.get)
	mux.HandleFunc("GET /media/{id}/{asset...}", a.media)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := a.DB.Ping(ctx); err != nil {
			apiError(w, 503, "not_ready", "Database unavailable.")
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := page.ReadFile("index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { apiError(w, 404, "not_found", "Route not found.") })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func (a *API) internal(w http.ResponseWriter, err error) {
	a.Log.Error("HTTP operation failed", "error", err)
	apiError(w, 500, "internal_error", "Operation failed.")
}

func validID(id string) bool { v, err := uuid.Parse(id); return err == nil && v.String() == id }

func (a *API) lookup(w http.ResponseWriter, r *http.Request) (store.Video, bool) {
	id := r.PathValue("id")
	if !validID(id) {
		apiError(w, 400, "invalid_id", "Invalid video ID.")
		return store.Video{}, false
	}
	v, err := a.DB.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		apiError(w, 404, "not_found", "Video not found.")
		return v, false
	}
	if err != nil {
		a.internal(w, err)
		return v, false
	}
	return v, true
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	if v, ok := a.lookup(w, r); ok {
		writeJSON(w, 200, v)
	}
}

func (a *API) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxUpload)
	bad := func(err error) {
		var max *http.MaxBytesError
		if errors.As(err, &max) {
			apiError(w, 413, "upload_too_large", "Request exceeds the 100 MiB limit.")
		} else {
			apiError(w, 400, "invalid_upload", "Provide one file and an optional title as multipart form data.")
		}
	}
	if r.ContentLength > MaxUpload {
		bad(&http.MaxBytesError{Limit: MaxUpload})
		return
	}
	reader, err := r.MultipartReader()
	if err != nil {
		bad(err)
		return
	}
	if err = os.MkdirAll(a.Sources, 0o755); err != nil {
		a.internal(w, err)
		return
	}
	id := uuid.NewString()
	final := filepath.Join(a.Sources, id+".mp4")
	tmp, err := os.CreateTemp(a.Sources, ".upload-"+id+"-")
	if err != nil {
		a.internal(w, err)
		return
	}
	defer tmp.Close()
	defer os.Remove(tmp.Name())
	committed := false
	defer func() {
		if !committed {
			os.Remove(final)
		}
	}()
	title := "Untitled video"
	hasFile, hasTitle := false, false
	var size int64
	for {
		part, e := reader.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			bad(e)
			return
		}
		switch part.FormName() {
		case "file":
			if hasFile || part.FileName() == "" {
				bad(errors.New("duplicate or missing file"))
				return
			}
			hasFile = true
			size, e = io.Copy(tmp, part)
			if e != nil {
				bad(e)
				return
			}
		case "title":
			if hasTitle {
				bad(errors.New("duplicate title"))
				return
			}
			hasTitle = true
			b, e := io.ReadAll(io.LimitReader(part, 513))
			if e != nil {
				bad(e)
				return
			}
			if len(b) > 512 || !utf8.Valid(b) {
				bad(errors.New("invalid title"))
				return
			}
			if s := strings.TrimSpace(string(b)); s != "" {
				title = s
			}
		default:
			bad(errors.New("unknown field"))
			return
		}
		if e = part.Close(); e != nil {
			bad(e)
			return
		}
	}
	// Drain an optional multipart epilogue so the limit applies to the entire request.
	if _, err = io.Copy(io.Discard, r.Body); err != nil {
		bad(err)
		return
	}
	if !hasFile || size == 0 {
		bad(errors.New("empty file"))
		return
	}
	if err = tmp.Close(); err != nil {
		a.internal(w, err)
		return
	}
	if err = os.Rename(tmp.Name(), final); err != nil {
		a.internal(w, err)
		return
	}
	v, err := a.DB.Insert(r.Context(), store.Video{ID: id, Title: title, SourcePath: final})
	if err != nil {
		a.internal(w, err)
		return
	}
	committed = true
	a.Log.Info("video queued", "video_id", id, "status", v.Status, "bytes", size)
	w.Header().Set("Location", "/api/videos/"+id)
	writeJSON(w, 202, map[string]string{"id": v.ID, "status": v.Status})
}

var assetPattern = regexp.MustCompile(`^(master\.m3u8|(360p|720p)/(index\.m3u8|segment[0-9]+\.ts))$`)

func (a *API) media(w http.ResponseWriter, r *http.Request) {
	v, ok := a.lookup(w, r)
	if !ok {
		return
	}
	asset := r.PathValue("asset")
	if v.Status != "ready" || !assetPattern.MatchString(asset) {
		apiError(w, 404, "not_found", "Media not found.")
		return
	}
	f, err := os.Open(filepath.Join(v.OutputPath, filepath.FromSlash(asset)))
	if err != nil {
		apiError(w, 404, "not_found", "Media not found.")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		apiError(w, 404, "not_found", "Media not found.")
		return
	}
	if strings.HasSuffix(asset, ".m3u8") {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	} else {
		w.Header().Set("Content-Type", "video/mp2t")
	}
	http.ServeContent(w, r, asset, st.ModTime(), f)
}
