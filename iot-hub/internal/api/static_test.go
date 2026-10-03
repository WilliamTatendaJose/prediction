package api

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestStaticAssets(t *testing.T) {
	js := strings.Repeat("console.log('hello');\n", 200)
	assets, err := loadAssets(fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html><title>x</title>")},
		"app.js":               {Data: []byte(js)},
		"manifest.webmanifest": {Data: []byte(`{"name":"x"}`)},
		"icon-192.png":         {Data: []byte("\x89PNG....")},
		"embed.go":             {Data: []byte("package web")},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := staticHandler(assets)
	get := func(path string, hdr ...string) *http.Response {
		req := httptest.NewRequest("GET", path, nil)
		for i := 0; i < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		h(w, req)
		return w.Result()
	}

	r := get("/app.js", "Accept-Encoding", "gzip, deflate, br")
	zr, err := gzip.NewReader(r.Body)
	if err != nil || r.Header.Get("Content-Encoding") != "gzip" || r.Header.Get("Vary") != "Accept-Encoding" {
		t.Fatalf("gzip: %v %v", err, r.Header)
	}
	if b, _ := io.ReadAll(zr); string(b) != js {
		t.Fatal("gzip body differs")
	}
	gzTag := r.Header.Get("ETag")

	r = get("/app.js")
	if b, _ := io.ReadAll(r.Body); string(b) != js || r.Header.Get("Content-Encoding") != "" || r.Header.Get("ETag") == gzTag {
		t.Fatalf("identity: %v", r.Header)
	}
	if r = get("/app.js", "If-None-Match", r.Header.Get("ETag")); r.StatusCode != 304 {
		t.Fatalf("revalidation: %d", r.StatusCode)
	}
	if r = get("/app.js", "Accept-Encoding", "gzip;q=0"); r.Header.Get("Content-Encoding") != "" {
		t.Error("gzip;q=0 must not get gzip")
	}
	if r = get("/manifest.webmanifest"); r.Header.Get("Content-Type") != "application/manifest+json" {
		t.Errorf("manifest type %q", r.Header.Get("Content-Type"))
	}
	if r = get("/icon-192.png", "Accept-Encoding", "gzip"); r.Header.Get("Content-Encoding") != "" || r.Header.Get("Content-Type") != "image/png" {
		t.Errorf("png: %v", r.Header)
	}
	if r = get("/"); r.Header.Get("Content-Type") != "text/html; charset=utf-8" || r.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("index: %v", r.Header)
	}
	for _, p := range []string{"/embed.go", "/nope.js", "/../app.js"} {
		if r = get(p); r.StatusCode != 404 {
			t.Errorf("%s: %d", p, r.StatusCode)
		}
	}
}
