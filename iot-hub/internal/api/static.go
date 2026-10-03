package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// asset is one dashboard file, prepared once at start: the embedded files
// have no modification time, so without an ETag every visit re-downloads
// them in full.
type asset struct {
	body, gz []byte
	ctype    string
	etag     string
}

var assetTypes = map[string]string{
	".webmanifest": "application/manifest+json",
	".js":          "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".html":        "text/html; charset=utf-8",
	".svg":         "image/svg+xml",
	".png":         "image/png",
}

func loadAssets(fsys fs.FS) (map[string]*asset, error) {
	out := map[string]*asset{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, ".go") {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		a := &asset{body: b, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		ext := path.Ext(p)
		if a.ctype = assetTypes[ext]; a.ctype == "" {
			a.ctype = mime.TypeByExtension(ext)
		}
		if ext != ".png" { // already compressed
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			zw.Write(b)
			zw.Close()
			if buf.Len() < len(b)*9/10 {
				a.gz = buf.Bytes()
			}
		}
		out["/"+p] = a
		return nil
	})
	return out, err
}

// staticHandler serves the dashboard: revalidated on every load (no-cache +
// ETag, so a deployed update shows at once and an unchanged file costs a
// 304), gzip when the browser accepts it.
func staticHandler(assets map[string]*asset) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/" {
			p = "/index.html"
		}
		a, ok := assets[p]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", a.ctype)
		h.Set("Cache-Control", "no-cache")
		h.Set("Vary", "Accept-Encoding")
		body, etag := a.body, a.etag
		if a.gz != nil && acceptsGzip(r) {
			body, etag = a.gz, strings.TrimSuffix(a.etag, `"`)+`-gz"`
			h.Set("Content-Encoding", "gzip")
		}
		h.Set("ETag", etag)
		if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatch(inm, etag) {
			h.Del("Content-Encoding")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if r.Method == http.MethodHead {
			return
		}
		w.Write(body)
	}
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc, params, _ := strings.Cut(part, ";")
		if !strings.EqualFold(strings.TrimSpace(enc), "gzip") {
			continue
		}
		if v, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			q, err := strconv.ParseFloat(v, 64)
			return err == nil && q > 0
		}
		return true
	}
	return false
}

func etagMatch(header, etag string) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == etag || t == "*" {
			return true
		}
	}
	return false
}

// StaticHandler serves dashboard files (shared by every tenant).
func StaticHandler(fsys fs.FS) (http.Handler, error) {
	a, err := loadAssets(fsys)
	if err != nil {
		return nil, err
	}
	return staticHandler(a), nil
}
