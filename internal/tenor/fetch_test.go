// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

package tenor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/antimatterchat/antimatter-gifs/internal/importer"
)

// fakeTenor answers like the Tenor API with generated posts.
func fakeTenor(t *testing.T, downloads *atomic.Int32) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	post := func(id string, sticker bool) map[string]any {
		formats := map[string]any{
			"gif":     map[string]any{"url": srv.URL + "/m/" + id + "/a.gif", "dims": []int{200, 100}, "duration": 1.2, "size": 10},
			"tinygif": map[string]any{"url": srv.URL + "/m/" + id + "/b.gif", "dims": []int{100, 50}, "duration": 1.2, "size": 5},
			"webm":    map[string]any{"url": srv.URL + "/m/" + id + "/c.webm", "dims": []int{200, 100}, "size": 8},
		}
		if sticker {
			formats = map[string]any{
				"gif_transparent": map[string]any{"url": srv.URL + "/m/" + id + "/s.gif", "dims": []int{300, 300}, "size": 9},
			}
		}
		return map[string]any{
			"id": id, "title": "", "content_description": "Post " + id, "created": 1700000000.5,
			"itemurl": "https://tenor.com/view/post-" + id, "tags": []string{"tag" + id}, "media_formats": formats,
		}
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if strings.HasPrefix(r.URL.Path, "/m/") {
			downloads.Add(1)
			fmt.Fprint(w, "media "+r.URL.Path)
			return
		}
		if q.Get("key") != "secret" || q.Get("client_key") != "antimatter-gifs" || q.Get("contentfilter") != "high" {
			http.Error(w, `{"error":{"code":400}}`, http.StatusBadRequest)
			return
		}
		var out any
		switch r.URL.Path {
		case "/v2/featured":
			prefix, total := "f", 7
			if q.Get("searchfilter") == "sticker" {
				prefix, total = "s", 2
			}
			start, _ := strconv.Atoi(q.Get("pos"))
			limit, _ := strconv.Atoi(q.Get("limit"))
			var results []any
			for i := start; i < min(start+limit, total); i++ {
				results = append(results, post(prefix+strconv.Itoa(i), prefix == "s"))
			}
			next := ""
			if start+limit < total {
				next = strconv.Itoa(start + limit)
			}
			out = map[string]any{"results": results, "next": next}
		case "/v2/categories":
			out = map[string]any{"tags": []any{
				map[string]any{"searchterm": "happy", "name": "#happy"},
				map[string]any{"searchterm": "sad", "name": "#sad"},
				map[string]any{"searchterm": "ignored", "name": "#ignored"},
			}}
		case "/v2/search":
			// The first result is also featured: it is downloaded once.
			out = map[string]any{"results": []any{post("f0", false), post(q.Get("q")+"1", false)}, "next": ""}
		default:
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetch(t *testing.T) {
	var downloads atomic.Int32
	srv := fakeTenor(t, &downloads)
	dir := t.TempDir()
	f := &Fetcher{Key: "secret", BaseURL: srv.URL + "/v2/", ContentFilter: "high", GIFFormats: []string{"gif", "tinygif", "mp4"}}
	m, err := f.Fetch(context.Background(), dir, Limits{Featured: 5, Categories: 2, PerCategory: 2, Stickers: 3})
	if err != nil {
		t.Fatal(err)
	}
	// 5 featured (two pages), 2 category posts (f0 is already there), 2 stickers.
	if len(m.Items) != 9 {
		t.Fatalf("%d items", len(m.Items))
	}
	if downloads.Load() != 16 {
		t.Fatalf("%d downloads", downloads.Load())
	}
	first := m.Items[0]
	if first.SourceID != "f0" || first.Key != "f0" || first.Score != 5 || first.Rating != "g" || first.Kind != "gif" ||
		first.SourceURL != "https://tenor.com/view/post-f0" || first.Description != "Post f0" || first.Created != 1700000000.5 {
		t.Fatalf("first item %+v", first)
	}
	if mf := first.Media["gif"]; mf.File != "files/f0/gif.gif" || mf.Dims != [2]int{200, 100} || mf.Duration != 1.2 {
		t.Fatalf("gif file %+v", mf)
	}
	if _, ok := first.Media["webm"]; ok {
		t.Fatal("downloaded a format that wasn't asked for")
	}
	data, err := os.ReadFile(filepath.Join(dir, "files", "f0", "tinygif.gif"))
	if err != nil || string(data) != "media /m/f0/b.gif" {
		t.Fatalf("file %q %v", data, err)
	}
	if len(m.Categories) != 2 || m.Categories[0].Name != "#happy" || strings.Join(m.Categories[0].Items, ",") != "f0,happy1" || m.Categories[0].Image != "f0" {
		t.Fatalf("categories %+v", m.Categories)
	}
	last := m.Items[len(m.Items)-1]
	if last.Kind != "sticker" || last.Media["gif_transparent"].File != "files/s1/gif_transparent.gif" || last.Score != 1 {
		t.Fatalf("sticker %+v", last)
	}

	// The manifest is saved for the importer, and a second fetch reuses the files.
	saved, err := importer.LoadManifest(filepath.Join(dir, "manifest.json"))
	if err != nil || len(saved.Items) != 9 || saved.Source != "tenor" || saved.Attribution != "Via Tenor" {
		t.Fatalf("saved manifest %+v %v", saved, err)
	}
	downloads.Store(0)
	if _, err := f.Fetch(context.Background(), dir, Limits{Featured: 5}); err != nil {
		t.Fatal(err)
	}
	if downloads.Load() != 0 {
		t.Fatalf("%d downloads on the second fetch", downloads.Load())
	}
}

func TestFetchErrors(t *testing.T) {
	if _, err := (&Fetcher{}).Fetch(context.Background(), t.TempDir(), Limits{Featured: 1}); err == nil {
		t.Fatal("fetched without a key")
	}
	var downloads atomic.Int32
	srv := fakeTenor(t, &downloads)
	f := &Fetcher{Key: "wrong", BaseURL: srv.URL + "/v2"}
	_, err := f.Fetch(context.Background(), t.TempDir(), Limits{Featured: 1})
	if err == nil || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("error %v", err)
	}
}

func TestRating(t *testing.T) {
	for filter, want := range map[string]string{"high": "g", "medium": "pg", "low": "pg13", "off": "r", "": "r"} {
		if got := Rating(filter); got != want {
			t.Errorf("Rating(%q) = %q", filter, got)
		}
	}
}
