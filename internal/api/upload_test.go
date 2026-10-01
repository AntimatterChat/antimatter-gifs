// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/antimatterchat/antimatter-gifs/internal/importer"
	"github.com/antimatterchat/antimatter-gifs/internal/index"
	"github.com/antimatterchat/antimatter-gifs/internal/media"
)

// A 1x1 GIF.
var tinyGIF = []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;")

func newUploadServer(t *testing.T, cfg Config) http.Handler {
	t.Helper()
	dir := t.TempDir()
	store, err := index.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg.MediaDir = filepath.Join(dir, "media")
	cfg.Logger = quiet
	cfg.Now = func() time.Time { return now }
	// Without the conversion tools, GIFs are kept as they are and other files can't be converted.
	cfg.Importer = &importer.Importer{Store: store, MediaDir: cfg.MediaDir, Tools: media.Tools{}, Logger: quiet}
	return New(store, cfg).Handler()
}

type upload struct {
	key    string
	fields map[string]string
	file   []byte
}

func (u upload) do(t *testing.T, h http.Handler) (*httptest.ResponseRecorder, postsResponse) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range u.fields {
		mw.WriteField(k, v)
	}
	if u.file != nil {
		fw, _ := mw.CreateFormFile("file", "whatever.bin")
		fw.Write(u.file)
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/v2/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if u.key != "" {
		req.Header.Set("Authorization", "Bearer "+u.key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var res postsResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
	}
	return rec, res
}

func TestUploadNeedsKeys(t *testing.T) {
	gif := upload{key: "up", fields: map[string]string{"title": "Wave"}, file: tinyGIF}
	if rec, _ := gif.do(t, newUploadServer(t, Config{})); rec.Code != http.StatusNotFound {
		t.Fatalf("without upload keys: %d", rec.Code)
	}
	h := newUploadServer(t, Config{UploadKeys: []string{"up"}, APIKeys: []string{"api"}})
	for _, key := range []string{"", "api", "upx"} {
		gif.key = key
		if rec, _ := gif.do(t, h); rec.Code != http.StatusForbidden {
			t.Fatalf("key %q: %d", key, rec.Code)
		}
	}
}

func TestUploadGIF(t *testing.T) {
	h := newUploadServer(t, Config{UploadKeys: []string{"up"}})
	rec, res := upload{key: "up", fields: map[string]string{"title": "Lab wave", "tags": "Hello, wave ,hello"}, file: tinyGIF}.do(t, h)
	if rec.Code != http.StatusOK || len(res.Results) != 1 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	post := res.Results[0]
	if post.Title != "Lab wave" || !slices.Equal(post.Tags, []string{"hello", "wave"}) || post.MediaFormats["gif"].URL == "" {
		t.Fatalf("post: %+v", post)
	}

	// The new GIF can be found.
	req := httptest.NewRequest(http.MethodGet, "/v2/search?q=wave", nil)
	search := httptest.NewRecorder()
	h.ServeHTTP(search, req)
	if !strings.Contains(search.Body.String(), post.ID) {
		t.Fatalf("search: %s", search.Body)
	}

	// The same file again updates its post.
	_, again := upload{key: "up", fields: map[string]string{"title": "Lab hello"}, file: tinyGIF}.do(t, h)
	if len(again.Results) != 1 || again.Results[0].ID != post.ID || again.Results[0].Title != "Lab hello" {
		t.Fatalf("upload again: %+v", again)
	}
}

func TestUploadStickerToPack(t *testing.T) {
	h := newUploadServer(t, Config{UploadKeys: []string{"up"}})
	rec, res := upload{key: "up", fields: map[string]string{"kind": "sticker", "title": "Dot", "pack": "Tiny things"}, file: tinyGIF}.do(t, h)
	if rec.Code != http.StatusOK || len(res.Results) != 1 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	post := res.Results[0]
	if !slices.Contains(post.Flags, "sticker") || !slices.Contains(post.Tags, "tiny things") || post.MediaFormats["gif_transparent"].URL == "" {
		t.Fatalf("sticker: %+v", post)
	}
	req := httptest.NewRequest(http.MethodGet, "/v2/categories?searchfilter=sticker", nil)
	cats := httptest.NewRecorder()
	h.ServeHTTP(cats, req)
	if !strings.Contains(cats.Body.String(), `"name":"Tiny things"`) {
		t.Fatalf("categories: %s", cats.Body)
	}
}

func TestUploadInvalid(t *testing.T) {
	h := newUploadServer(t, Config{UploadKeys: []string{"up"}, UploadMaxBytes: 1 << 20})
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10"/></svg>`)
	for name, tc := range map[string]struct {
		u    upload
		code int
	}{
		"no title":      {upload{fields: map[string]string{}, file: tinyGIF}, http.StatusBadRequest},
		"no file":       {upload{fields: map[string]string{"title": "x"}}, http.StatusBadRequest},
		"text":          {upload{fields: map[string]string{"title": "x"}, file: []byte("#EXTM3U\nhttp://example.com/a.ts\n")}, http.StatusBadRequest},
		"SVG GIF":       {upload{fields: map[string]string{"title": "x"}, file: svg}, http.StatusBadRequest},
		"MP4 sticker":   {upload{fields: map[string]string{"title": "x", "kind": "sticker"}, file: []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00")}, http.StatusBadRequest},
		"GIF pack":      {upload{fields: map[string]string{"title": "x", "pack": "p"}, file: tinyGIF}, http.StatusBadRequest},
		"kind":          {upload{fields: map[string]string{"title": "x", "kind": "video"}, file: tinyGIF}, http.StatusBadRequest},
		"rating":        {upload{fields: map[string]string{"title": "x", "rating": "x"}, file: tinyGIF}, http.StatusBadRequest},
		"too many tags": {upload{fields: map[string]string{"title": "x", "tags": "a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p,q,r,s,t,u"}, file: tinyGIF}, http.StatusBadRequest},
		"too large":     {upload{fields: map[string]string{"title": "x"}, file: append(append([]byte{}, tinyGIF...), make([]byte, 3<<20)...)}, http.StatusRequestEntityTooLarge},
		"doctype":       {upload{fields: map[string]string{"title": "x", "kind": "sticker"}, file: append([]byte(`<!DOCTYPE svg [<!ENTITY a "b">]>`), svg...)}, http.StatusBadRequest},
		// A valid SVG sticker, which can't be rendered without rsvg-convert here.
		"unconvertible": {upload{fields: map[string]string{"title": "x", "kind": "sticker"}, file: svg}, http.StatusUnprocessableEntity},
	} {
		tc.u.key = "up"
		if rec, _ := tc.u.do(t, h); rec.Code != tc.code {
			t.Errorf("%s: got %d, want %d: %s", name, rec.Code, tc.code, rec.Body)
		}
	}
}

func TestSniffType(t *testing.T) {
	for head, want := range map[string]string{
		"GIF87a...":                        ".gif",
		"\x89PNG\r\n\x1a\n...":             ".png",
		"RIFF\x00\x00\x00\x00WEBPVP8X":     ".webp",
		"\x00\x00\x00\x20ftypisom\x00\x00": ".mp4",
		"\x1a\x45\xdf\xa3...":              ".webm",
		"\xef\xbb\xbf<?xml version=\"1.0\"?>\n<svg xmlns=\"http://www.w3.org/2000/svg\">": ".svg",
		"<html><body>":     "",
		"\xff\xd8\xff\xe0": "",
	} {
		if got := sniffType([]byte(head)); got != want {
			t.Errorf("%q: %q, want %q", head, got, want)
		}
	}
}
