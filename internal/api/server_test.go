// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/antimatterchat/antimatter-gifs/internal/index"
)

var now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type fixture struct {
	store    *index.Store
	mediaDir string
	handler  http.Handler
}

func newFixture(t *testing.T, cfg Config) *fixture {
	t.Helper()
	dir := t.TempDir()
	store, err := index.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	gif := func(id, title string, tags []string, score float64, w, h int, rating string) *index.Post {
		return &index.Post{
			ID: id, Title: title, Kind: index.KindGIF, Tags: tags, Score: score, Width: w, Height: h, Rating: rating,
			Created: now.Add(-time.Hour), Description: title + " animation",
			Media: map[string]index.Media{
				"gif":     {Path: "g/" + id + "/gif.gif", Width: w, Height: h, Size: 1000, Duration: 1.5},
				"tinygif": {Path: "g/" + id + "/tinygif.gif", Width: w / 2, Height: h / 2, Size: 200, Duration: 1.5},
				"mp4":     {Path: "g/" + id + "/mp4.mp4", Width: w, Height: h, Size: 500, Duration: 1.5},
			},
		}
	}
	posts := []*index.Post{
		gif("100", "Happy cat", []string{"cat", "happy"}, 5, 200, 200, "g"),
		gif("101", "Cat dance", []string{"cat", "dance"}, 3, 300, 100, "pg"),
		gif("102", "Dog wave", []string{"dog", "hello"}, 9, 200, 150, "r"),
		{
			ID: "200", Title: "It works!", Kind: index.KindSticker, Tags: []string{"science", "yes"}, Score: 10,
			Width: 320, Height: 320, Source: "antimatter", Attribution: "Antimatter stickers, CC BY 4.0",
			Media: map[string]index.Media{
				"gif_transparent":  {Path: "s/200/gif_transparent.gif", Width: 320, Height: 320, Size: 3000},
				"webp_transparent": {Path: "s/200/webp_transparent.webp", Width: 320, Height: 320, Size: 2000},
			},
		},
	}
	posts[2].Source, posts[2].SourceID, posts[2].SourceURL, posts[2].Attribution = "tenor", "t-102", "https://tenor.com/view/t-102", "Via Tenor"
	for _, p := range posts {
		if err := store.UpsertPost(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetCategory(ctx, index.Category{Kind: index.KindGIF, Name: "#cats", SearchTerm: "cats"}, []string{"101", "100"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCategory(ctx, index.Category{Kind: index.KindSticker, Name: "Lab crew", SearchTerm: "lab crew"}, []string{"200"}); err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(dir, "media")
	if err := os.MkdirAll(filepath.Join(media, "g", "100"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(media, "g", "100", "gif.gif"), []byte("GIF89a-test"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("TOP-SECRET-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.MediaDir = media
	cfg.Now = func() time.Time { return now }
	return &fixture{store: store, mediaDir: media, handler: New(store, cfg).Handler()}
}

func (f *fixture) do(t *testing.T, method, target string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) get(t *testing.T, target string, out any) *httptest.ResponseRecorder {
	t.Helper()
	rec := f.do(t, http.MethodGet, target, nil)
	if out != nil {
		// Decoding into a used value would merge maps.
		reflect.ValueOf(out).Elem().SetZero()
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("GET %s: invalid JSON %q: %v", target, rec.Body.String(), err)
		}
	}
	return rec
}

func resultIDs(rs []ResultObject) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

func eq(a, b []string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",")
}

func TestSearch(t *testing.T) {
	f := newFixture(t, Config{})
	var res resultsResponse
	rec := f.get(t, "/v2/search?q=cat&limit=1", &res)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("status %d, content type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if len(res.Results) != 1 || res.Next != "1" {
		t.Fatalf("results %v, next %q", resultIDs(res.Results), res.Next)
	}
	r := res.Results[0]
	if r.ID != "100" || r.Title != "Happy cat" || r.ContentDescription != "Happy cat animation" || !eq(r.Tags, []string{"cat", "happy"}) {
		t.Fatalf("unexpected result %+v", r)
	}
	gif := r.MediaFormats["gif"]
	if gif.URL != "http://example.com/media/g/100/gif.gif" || gif.Dims != [2]int{200, 200} || gif.Size != 1000 || gif.Duration != 1.5 {
		t.Fatalf("unexpected gif media %+v", gif)
	}
	if len(r.MediaFormats) != 3 || r.ItemURL != "http://example.com/view/100" || r.URL != r.ItemURL {
		t.Fatalf("unexpected formats or URLs %+v", r)
	}
	if r.Created != float64(now.Add(-time.Hour).Unix()) || len(r.Flags) != 0 || r.Attribution != nil {
		t.Fatalf("unexpected created, flags or attribution %+v", r)
	}

	// Next page, from the pos token.
	f.get(t, "/v2/search?q=cat&limit=1&pos="+res.Next, &res)
	if !eq(resultIDs(res.Results), []string{"101"}) || res.Next != "" {
		t.Fatalf("page 2: %v, next %q", resultIDs(res.Results), res.Next)
	}

	// media_filter keeps the requested formats only.
	f.get(t, "/v2/search?q=cat&media_filter=tinygif,mp4", &res)
	for _, r := range res.Results {
		if len(r.MediaFormats) != 2 || r.MediaFormats["tinygif"].URL == "" || r.MediaFormats["mp4"].URL == "" {
			t.Fatalf("media_filter: %+v", r.MediaFormats)
		}
	}

	// contentfilter and ar_range.
	f.get(t, "/v2/search?q=dog", &res)
	if !eq(resultIDs(res.Results), []string{"102"}) || res.Results[0].Attribution == nil || res.Results[0].Attribution.Source != "tenor" {
		t.Fatalf("dog: %+v", res.Results)
	}
	f.get(t, "/v2/search?q=dog&contentfilter=medium", &res)
	if len(res.Results) != 0 {
		t.Fatalf("contentfilter=medium kept %v", resultIDs(res.Results))
	}
	f.get(t, "/v2/search?q=cat&ar_range=standard", &res)
	if !eq(resultIDs(res.Results), []string{"100"}) {
		t.Fatalf("ar_range=standard: %v", resultIDs(res.Results))
	}

	// Stickers.
	f.get(t, "/v2/search?q=science&searchfilter=sticker", &res)
	if !eq(resultIDs(res.Results), []string{"200"}) || !eq(res.Results[0].Flags, []string{"sticker"}) ||
		res.Results[0].MediaFormats["webp_transparent"].URL == "" {
		t.Fatalf("sticker search: %+v", res.Results)
	}
	f.get(t, "/v2/search?q=science", &res)
	if len(res.Results) != 0 {
		t.Fatalf("GIF search returned stickers: %v", resultIDs(res.Results))
	}
	f.get(t, "/v2/search?q=lab%20crew&searchfilter=sticker", &res)
	if !eq(resultIDs(res.Results), []string{"200"}) {
		t.Fatalf("sticker pack search: %v", resultIDs(res.Results))
	}

	// Errors in the Google API format.
	for target, want := range map[string]int{
		"/v2/search":                         http.StatusBadRequest,
		"/v2/search?q=cat&limit=abc":         http.StatusBadRequest,
		"/v2/search?q=cat&contentfilter=bad": http.StatusBadRequest,
		"/v2/search?q=cat&ar_range=square":   http.StatusBadRequest,
		"/v2/unknown":                        http.StatusNotFound,
	} {
		var e errorResponse
		rec := f.get(t, target, &e)
		if rec.Code != want || e.Error.Code != want || e.Error.Status == "" || e.Error.Message == "" {
			t.Fatalf("GET %s: %d %+v", target, rec.Code, e)
		}
	}

	// Limits are capped at 50, random doesn't lose results.
	f.get(t, "/v2/search?q=cat&limit=500&random=true", &res)
	if len(res.Results) != 2 {
		t.Fatalf("random: %v", resultIDs(res.Results))
	}
}

func TestFeaturedAndPosts(t *testing.T) {
	f := newFixture(t, Config{PublicURL: "https://gifs.example.org/", MediaURL: "https://cdn.example.org/gifs"})
	var res resultsResponse
	f.get(t, "/v2/featured?limit=2", &res)
	if !eq(resultIDs(res.Results), []string{"102", "100"}) || res.Next != "2" {
		t.Fatalf("featured: %v next %q", resultIDs(res.Results), res.Next)
	}
	if u := res.Results[0].MediaFormats["gif"].URL; u != "https://cdn.example.org/gifs/g/102/gif.gif" {
		t.Fatalf("media URL %q", u)
	}
	if u := res.Results[0].ItemURL; u != "https://gifs.example.org/view/102" {
		t.Fatalf("item URL %q", u)
	}
	f.get(t, "/v2/featured?searchfilter=sticker", &res)
	if !eq(resultIDs(res.Results), []string{"200"}) || res.Next != "" {
		t.Fatalf("featured stickers: %v", resultIDs(res.Results))
	}

	// Shares make a post trend.
	for range 2 {
		if rec := f.do(t, http.MethodGet, "/v2/registershare?id=101&q=dance", nil); rec.Code != http.StatusOK {
			t.Fatalf("registershare: %d %s", rec.Code, rec.Body)
		}
	}
	f.get(t, "/v2/featured?limit=1", &res)
	if !eq(resultIDs(res.Results), []string{"101"}) {
		t.Fatalf("featured after shares: %v", resultIDs(res.Results))
	}
	if rec := f.do(t, http.MethodGet, "/v2/registershare?id=999", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("registershare of an unknown id: %d", rec.Code)
	}
	if rec := f.do(t, http.MethodGet, "/v2/registershare", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("registershare without id: %d", rec.Code)
	}

	var posts postsResponse
	f.get(t, "/v2/posts?ids=200,missing,100&media_filter=gif", &posts)
	if !eq(resultIDs(posts.Results), []string{"200", "100"}) || len(posts.Results[0].MediaFormats) != 0 || len(posts.Results[1].MediaFormats) != 1 {
		t.Fatalf("posts: %+v", posts.Results)
	}
	if rec := f.do(t, http.MethodGet, "/v2/posts", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("posts without ids: %d", rec.Code)
	}
	if rec := f.do(t, http.MethodGet, "/v2/posts?ids="+strings.Repeat("1,", 51), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("posts with too many ids: %d", rec.Code)
	}
}

func TestCategoriesAndTerms(t *testing.T) {
	f := newFixture(t, Config{})
	var cats categoriesResponse
	f.get(t, "/v2/categories?locale=fr_FR&contentfilter=high", &cats)
	if cats.Locale != "fr" || len(cats.Tags) != 1 {
		t.Fatalf("categories: %+v", cats)
	}
	c := cats.Tags[0]
	if c.Name != "#cats" || c.SearchTerm != "cats" || c.Image != "http://example.com/media/g/101/tinygif.gif" ||
		c.Path != "/v2/search?q=cats&locale=fr&component=categories&contentfilter=high" {
		t.Fatalf("category: %+v", c)
	}
	f.get(t, "/v2/categories?searchfilter=sticker", &cats)
	if len(cats.Tags) != 1 || cats.Tags[0].Name != "Lab crew" || cats.Tags[0].Image != "http://example.com/media/s/200/gif_transparent.gif" ||
		!strings.HasSuffix(cats.Tags[0].Path, "&searchfilter=sticker") {
		t.Fatalf("sticker packs: %+v", cats)
	}
	f.do(t, http.MethodGet, "/v2/registershare?id=102&q=wave", nil)
	f.get(t, "/v2/categories?type=trending", &cats)
	if len(cats.Tags) == 0 || cats.Tags[0].SearchTerm != "wave" || cats.Tags[0].Name != "#wave" {
		t.Fatalf("trending categories: %+v", cats)
	}
	if rec := f.do(t, http.MethodGet, "/v2/categories?type=bogus", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad type: %d", rec.Code)
	}

	var terms termsResponse
	f.get(t, "/v2/trending_terms?limit=2", &terms)
	if terms.Locale != "en" || !eq(terms.Results, []string{"wave", "cats"}) {
		t.Fatalf("trending terms: %+v", terms)
	}
	f.get(t, "/v2/autocomplete?q=da", &terms)
	if !eq(terms.Results, []string{"dance"}) {
		t.Fatalf("autocomplete: %+v", terms)
	}
	f.get(t, "/v2/search_suggestions?q=cat", &terms)
	if !eq(terms.Results, []string{"cats", "happy", "dance"}) {
		t.Fatalf("suggestions: %+v", terms)
	}
	for _, target := range []string{"/v2/autocomplete", "/v2/search_suggestions"} {
		if rec := f.do(t, http.MethodGet, target, nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s without q: %d", target, rec.Code)
		}
	}
}

func TestMedia(t *testing.T) {
	f := newFixture(t, Config{})
	rec := f.do(t, http.MethodGet, "/media/g/100/gif.gif", nil)
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK || string(body) != "GIF89a-test" || rec.Header().Get("Content-Type") != "image/gif" {
		t.Fatalf("media: %d %q %v", rec.Code, body, rec.Header())
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") || rec.Header().Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(rec.Header().Get("Cache-Control"), "public") {
		t.Fatalf("media headers: %v", rec.Header())
	}
	for _, target := range []string{"/media/", "/media/g/100", "/media/g/100/missing.gif", "/media/../secret.txt", "/media/%2e%2e/secret.txt", "/media/g/..%2f..%2fsecret.txt"} {
		if rec := f.do(t, http.MethodGet, target, nil); rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "TOP-SECRET") {
			t.Fatalf("GET %s: %d %q", target, rec.Code, rec.Body)
		}
	}
}

func TestViewAndHealth(t *testing.T) {
	f := newFixture(t, Config{})
	rec := f.do(t, http.MethodGet, "/view/102", nil)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Dog wave") || !strings.Contains(body, `href="https://tenor.com/view/t-102"`) ||
		!strings.Contains(body, "http://example.com/media/g/102/gif.gif") {
		t.Fatalf("view: %d %s", rec.Code, body)
	}
	if rec := f.do(t, http.MethodGet, "/view/nope", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("missing view: %d", rec.Code)
	}
	var health map[string]any
	if rec := f.get(t, "/healthz", &health); rec.Code != http.StatusOK || health["status"] != "ok" || health["posts"] != float64(4) {
		t.Fatalf("health: %d %v", rec.Code, health)
	}
}

func TestForwardedHeaders(t *testing.T) {
	f := newFixture(t, Config{TrustProxy: true})
	var res resultsResponse
	rec := f.do(t, http.MethodGet, "/v2/search?q=happy", map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "gifs.example.net"})
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Results) == 0 || !strings.HasPrefix(res.Results[0].MediaFormats["gif"].URL, "https://gifs.example.net/media/") {
		t.Fatalf("forwarded: %+v", res.Results)
	}
}
