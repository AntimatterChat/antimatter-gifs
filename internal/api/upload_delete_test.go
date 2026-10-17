// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antimatterchat/gifs-server/internal/index"
)

func deleteUpload(h http.Handler, id, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodDelete, "/v2/upload/"+id, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestDeleteUploadNeedsKeys(t *testing.T) {
	if rec := deleteUpload(newUploadServer(t, Config{}), "123", "up"); rec.Code != http.StatusNotFound {
		t.Fatalf("without upload keys: %d", rec.Code)
	}
	h := newUploadServer(t, Config{UploadKeys: []string{"up"}, APIKeys: []string{"api"}})
	_, res := upload{key: "up", fields: map[string]string{"title": "Wave"}, file: tinyGIF}.do(t, h)
	if len(res.Results) != 1 {
		t.Fatal("no upload")
	}
	for _, key := range []string{"", "api", "upx"} {
		if rec := deleteUpload(h, res.Results[0].ID, key); rec.Code != http.StatusForbidden {
			t.Fatalf("key %q: %d", key, rec.Code)
		}
	}
}

func TestDeleteUpload(t *testing.T) {
	h, store, mediaDir := newUploadServerStore(t, Config{UploadKeys: []string{"up"}})
	_, res := upload{key: "up", fields: map[string]string{"kind": "sticker", "title": "Dot", "pack": "Tiny things"}, file: tinyGIF}.do(t, h)
	if len(res.Results) != 1 {
		t.Fatal("no upload")
	}
	id := res.Results[0].ID
	dir := filepath.Join(mediaDir, id[len(id)-2:], id)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("media before: %v", err)
	}

	if rec := deleteUpload(h, id, "up"); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if posts, _ := store.GetPosts(context.Background(), []string{id}); len(posts) != 0 {
		t.Fatalf("post still there: %+v", posts)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("media still there: %v", err)
	}

	// Gone from the search and from its (now empty) pack.
	for _, path := range []string{"/v2/search?q=dot&searchfilter=sticker", "/v2/categories?searchfilter=sticker"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if strings.Contains(rec.Body.String(), id) || strings.Contains(rec.Body.String(), "Tiny things") {
			t.Fatalf("%s: %s", path, rec.Body)
		}
	}

	// Deleting it again: not found.
	if rec := deleteUpload(h, id, "up"); rec.Code != http.StatusNotFound {
		t.Fatalf("delete again: %d", rec.Code)
	}
}

func TestDeleteUploadRefusesTheCatalogue(t *testing.T) {
	h, store, _ := newUploadServerStore(t, Config{UploadKeys: []string{"up"}})
	ctx := context.Background()
	for _, source := range []string{"", "tenor", "import"} {
		id := "9900" + source
		if err := store.UpsertPost(ctx, &index.Post{ID: id, Title: "Catalogue", Kind: index.KindGIF, Source: source, Created: now}); err != nil {
			t.Fatal(err)
		}
		if rec := deleteUpload(h, id, "up"); rec.Code != http.StatusNotFound {
			t.Fatalf("source %q: %d", source, rec.Code)
		}
		if posts, _ := store.GetPosts(ctx, []string{id}); len(posts) != 1 {
			t.Fatalf("source %q: the catalogue post was deleted", source)
		}
	}
}

func TestDeleteUploadInvalid(t *testing.T) {
	h := newUploadServer(t, Config{UploadKeys: []string{"up"}})
	for id, code := range map[string]int{
		"doesnotexist":          http.StatusNotFound,
		"a.b":                   http.StatusBadRequest,
		"a,b":                   http.StatusBadRequest,
		strings.Repeat("1", 65): http.StatusBadRequest,
	} {
		if rec := deleteUpload(h, id, "up"); rec.Code != code {
			t.Errorf("%q: got %d, want %d", id, rec.Code, code)
		}
	}
}
