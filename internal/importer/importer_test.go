// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

package importer

import (
	"context"
	"image"
	"image/color"
	"image/gif"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/antimatterchat/antimatter-gifs/internal/index"
	"github.com/antimatterchat/antimatter-gifs/internal/media"
)

func writeGIF(t *testing.T, file string, frames int) {
	t.Helper()
	pal := color.Palette{color.Black, color.White}
	anim := &gif.GIF{}
	for i := range frames {
		img := image.NewPaletted(image.Rect(0, 0, 40, 20), pal)
		img.SetColorIndex(i, 0, 1)
		anim.Image = append(anim.Image, img)
		anim.Delay = append(anim.Delay, 5)
	}
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := gif.EncodeAll(f, anim); err != nil {
		t.Fatal(err)
	}
}

func TestImport(t *testing.T) {
	dir := t.TempDir()
	store, err := index.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	writeGIF(t, filepath.Join(dir, "happy-cat.gif"), 4)
	writeGIF(t, filepath.Join(dir, "still.gif"), 1)
	writeGIF(t, filepath.Join(dir, "tiny.gif"), 2)

	ctx := context.Background()
	im := &Importer{Store: store, MediaDir: filepath.Join(dir, "media"), Tools: media.Tools{}, Now: func() time.Time { return time.Unix(1700000000, 0) }}
	m := &Manifest{
		Dir:         dir,
		Source:      "tenor",
		Attribution: "Via Tenor",
		Items: []Item{
			{File: "happy-cat.gif", Tags: []string{"cat"}, SourceID: "t1", Score: 3},
			{File: "still.gif", Title: "Still", Source: "local"},
			{Key: "made", Title: "Ready made", SourceID: "t3", Media: map[string]MediaFile{
				"tinygif": {File: "tiny.gif", Dims: [2]int{20, 10}},
				"gif":     {File: "tiny.gif"},
			}},
			{File: "missing.gif", SourceID: "t4"},
			{File: "happy-cat.gif", SourceID: "t5", Media: map[string]MediaFile{"bogus": {File: "tiny.gif"}}},
		},
		Categories: []Category{{Name: "#cats", SearchTerm: "cats", Items: []string{"made", "t1", "t4"}, Image: "t1"}},
	}
	stats, err := im.Import(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	if stats != (Stats{Added: 3, Failed: 2}) {
		t.Fatalf("stats %+v", stats)
	}

	id, err := store.FindBySource(ctx, "tenor", "t1")
	if err != nil {
		t.Fatal(err)
	}
	posts, _ := store.GetPosts(ctx, []string{id})
	p := posts[0]
	if p.Title != "Happy cat" || p.Static || p.Width != 40 || p.Attribution != "Via Tenor" || p.Score != 3 || p.Created.Unix() != 1700000000 {
		t.Fatalf("post %+v", p)
	}
	gifFile := p.Media["gif"]
	if gifFile.Path != id[len(id)-2:]+"/"+id+"/gif.gif" || gifFile.Duration != 0.2 {
		t.Fatalf("media %+v", gifFile)
	}
	if _, err := os.Stat(filepath.Join(im.MediaDir, filepath.FromSlash(gifFile.Path))); err != nil {
		t.Fatal(err)
	}

	stillID, err := store.FindBySource(ctx, "local", "")
	if err == nil || stillID != "" {
		t.Fatal("still image without content hash")
	}
	results, _, _ := store.Search(ctx, "still", index.Filter{Kind: index.KindGIF}, 0, 5, time.Now())
	if len(results) != 1 || !results[0].Static || results[0].Source != "local" || results[0].SourceID[:7] != "sha256:" {
		t.Fatalf("still: %+v", results)
	}

	madeID, _ := store.FindBySource(ctx, "tenor", "t3")
	posts, _ = store.GetPosts(ctx, []string{madeID})
	if tiny := posts[0].Media["tinygif"]; tiny.Width != 20 || tiny.Height != 10 || tiny.Duration != 0.1 {
		t.Fatalf("ready made tinygif %+v", tiny)
	}

	cats, _ := store.Categories(ctx, index.KindGIF)
	if len(cats) != 1 || cats[0].ImagePostID != id {
		t.Fatalf("categories %+v", cats)
	}
	results, _, _ = store.Search(ctx, "cats", index.Filter{Kind: index.KindGIF}, 0, 5, time.Now())
	if len(results) != 2 || results[0].ID != madeID || results[1].ID != id {
		t.Fatalf("category order: %+v", results)
	}

	// Importing again updates the same posts.
	m.Items = m.Items[:1]
	m.Items[0].Title = "Happier cat"
	stats, err = im.Import(ctx, m)
	if err != nil || stats != (Stats{Updated: 1}) {
		t.Fatalf("re-import: %+v %v", stats, err)
	}
	posts, _ = store.GetPosts(ctx, []string{id})
	if posts[0].Title != "Happier cat" {
		t.Fatalf("re-imported %+v", posts[0])
	}

	if err := im.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(im.MediaDir, id[len(id)-2:], id)); !os.IsNotExist(err) {
		t.Fatalf("media left after delete: %v", err)
	}
	if err := im.Delete(ctx, "../x"); err == nil {
		t.Fatal("deleted an invalid id")
	}
}

func TestLoadManifest(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "manifest.json")
	os.WriteFile(file, []byte(`{"kind":"sticker","items":[{"file":"a.svg","tags":["x"]}]}`), 0o644)
	m, err := LoadManifest(file)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != "sticker" || len(m.Items) != 1 || m.resolve("a.svg") != filepath.Join(dir, "a.svg") || m.resolve("/abs.gif") != "/abs.gif" {
		t.Fatalf("manifest %+v", m)
	}
}

func TestNewID(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := newID()
		if len(id) < 18 || seen[id] {
			t.Fatalf("id %q", id)
		}
		seen[id] = true
	}
}
