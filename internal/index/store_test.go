// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

package index

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func addPost(t *testing.T, s *Store, p *Post) {
	t.Helper()
	if p.Kind == "" {
		p.Kind = KindGIF
	}
	if p.Media == nil {
		p.Media = map[string]Media{"gif": {Path: p.ID + "/gif.gif", Width: 200, Height: 100, Size: 1234}}
	}
	if p.Width == 0 {
		p.Width, p.Height = 200, 100
	}
	if p.Created.IsZero() {
		p.Created = now
	}
	if err := s.UpsertPost(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func ids(posts []*Post) []string {
	out := make([]string, len(posts))
	for i, p := range posts {
		out[i] = p.ID
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestUpsertAndGet(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	addPost(t, s, &Post{ID: "1", Title: "Happy cat", Tags: []string{"Cat", "#happy", "cat"}, Source: "tenor", SourceID: "t1"})
	addPost(t, s, &Post{ID: "1", Title: "Happy cat dancing", Tags: []string{"cat", "dance"}, Source: "tenor", SourceID: "t1"})

	posts, err := s.GetPosts(ctx, []string{"missing", "1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 || posts[0].Title != "Happy cat dancing" || !equal(posts[0].Tags, []string{"cat", "dance"}) {
		t.Fatalf("unexpected post %+v", posts)
	}
	if m := posts[0].Media["gif"]; m.Path != "1/gif.gif" || m.Width != 200 || m.Size != 1234 {
		t.Fatalf("unexpected media %+v", m)
	}
	if !posts[0].Created.Equal(now) {
		t.Fatalf("created %v, want %v", posts[0].Created, now)
	}
	if id, err := s.FindBySource(ctx, "tenor", "t1"); err != nil || id != "1" {
		t.Fatalf("FindBySource = %q, %v", id, err)
	}
	if n, err := s.Ping(ctx); err != nil || n != 1 {
		t.Fatalf("Ping = %d, %v", n, err)
	}
	if err := s.DeletePost(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePost(ctx, "1"); err != ErrNotFound {
		t.Fatalf("second delete: %v", err)
	}
}

func TestSearchRanking(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	addPost(t, s, &Post{ID: "dog", Title: "Good dog", Tags: []string{"dog"}})
	addPost(t, s, &Post{ID: "cat-desc", Title: "Sleepy", Description: "a cat sleeping on a keyboard", Tags: []string{"sleep"}})
	addPost(t, s, &Post{ID: "cat-title", Title: "Happy cats", Tags: []string{"cat", "happy"}})
	addPost(t, s, &Post{ID: "sticker", Kind: KindSticker, Title: "Cat sticker", Tags: []string{"cat"}})
	addPost(t, s, &Post{ID: "nsfw-cat", Title: "Cat", Tags: []string{"cat"}, Rating: "r"})

	got, more, err := s.Search(ctx, "cat", Filter{Kind: KindGIF, Ratings: []string{"g"}}, 0, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	if !equal(ids(got), []string{"cat-title", "cat-desc"}) || more {
		t.Fatalf("search cat = %v (more %v)", ids(got), more)
	}

	// Prefix matching, stemming and both words beating one.
	got, _, _ = s.Search(ctx, "happy ca", Filter{Kind: KindGIF}, 0, 10, now)
	if len(got) == 0 || got[0].ID != "cat-title" {
		t.Fatalf("search 'happy ca' = %v", ids(got))
	}

	got, _, _ = s.Search(ctx, "cat", Filter{Kind: KindSticker}, 0, 10, now)
	if !equal(ids(got), []string{"sticker"}) {
		t.Fatalf("sticker search = %v", ids(got))
	}

	// Pagination.
	page1, more, _ := s.Search(ctx, "cat", Filter{Kind: KindGIF}, 0, 2, now)
	page2, more2, _ := s.Search(ctx, "cat", Filter{Kind: KindGIF}, 2, 2, now)
	if len(page1) != 2 || !more || len(page2) != 1 || more2 {
		t.Fatalf("pages %v (%v) %v (%v)", ids(page1), more, ids(page2), more2)
	}

	// Quotes and operators are plain words.
	if _, _, err := s.Search(ctx, `cat" OR dog NEAR(`, Filter{Kind: KindGIF}, 0, 10, now); err != nil {
		t.Fatalf("search with operators: %v", err)
	}
}

func TestCategoriesComeFirstForTheirTerm(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	addPost(t, s, &Post{ID: "a", Title: "Party party party", Tags: []string{"party"}})
	addPost(t, s, &Post{ID: "b", Title: "Confetti", Tags: []string{"celebrate"}})
	if err := s.SetCategory(ctx, Category{Kind: KindGIF, Name: "#Party", SearchTerm: "Party", Position: 1}, []string{"b", "a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCategory(ctx, Category{Kind: KindGIF, Name: "Empty", SearchTerm: "empty"}, nil); err != nil {
		t.Fatal(err)
	}
	cats, err := s.Categories(ctx, KindGIF)
	if err != nil {
		t.Fatal(err)
	}
	if len(cats) != 1 || cats[0].SearchTerm != "party" || cats[0].ImagePostID != "b" {
		t.Fatalf("categories = %+v", cats)
	}
	got, _, _ := s.Search(ctx, "party", Filter{Kind: KindGIF}, 0, 10, now)
	if !equal(ids(got), []string{"b", "a"}) {
		t.Fatalf("search party = %v", ids(got))
	}

	addPost(t, s, &Post{ID: "c", Title: "Streamers"})
	if err := s.AddToCategory(ctx, Category{Kind: KindGIF, SearchTerm: "party"}, []string{"c", "b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddToCategory(ctx, Category{Kind: KindGIF, SearchTerm: "Fresh"}, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Search(ctx, "party", Filter{Kind: KindGIF}, 0, 10, now)
	if !equal(ids(got), []string{"b", "a", "c"}) {
		t.Fatalf("search party after adding = %v", ids(got))
	}
	cats, _ = s.Categories(ctx, KindGIF)
	if len(cats) != 2 || cats[1].SearchTerm != "fresh" || cats[1].Name != "fresh" || cats[1].Position != 2 {
		t.Fatalf("categories after adding = %+v", cats)
	}
}

func TestFeaturedAndTrending(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	addPost(t, s, &Post{ID: "low", Title: "Low", Tags: []string{"wave"}, Score: 1})
	addPost(t, s, &Post{ID: "high", Title: "High", Tags: []string{"wave", "hello"}, Score: 10})
	addPost(t, s, &Post{ID: "wide", Title: "Wide", Tags: []string{"hello"}, Width: 400, Height: 100})

	got, _, err := s.Featured(ctx, Filter{Kind: KindGIF}, 0, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	if !equal(ids(got), []string{"high", "low", "wide"}) {
		t.Fatalf("featured = %v", ids(got))
	}
	got, _, _ = s.Featured(ctx, Filter{Kind: KindGIF, MinAspect: 0.56, MaxAspect: 1.78}, 0, 10, now)
	if len(got) != 0 {
		t.Fatalf("standard aspect = %v", ids(got))
	}

	// Recent shares move a post up; old shares don't count.
	for range 3 {
		if err := s.RecordShare(ctx, "low", "Waving", now); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordShare(ctx, "wide", "", now.Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordShare(ctx, "missing", "", now); err != ErrNotFound {
		t.Fatalf("share of a missing post: %v", err)
	}
	got, _, _ = s.Featured(ctx, Filter{Kind: KindGIF}, 0, 2, now)
	if !equal(ids(got), []string{"low", "high"}) {
		t.Fatalf("featured after shares = %v", ids(got))
	}

	if err := s.RecordSearch(ctx, "Hello  there", now); err != nil {
		t.Fatal(err)
	}
	terms, err := s.TrendingTerms(ctx, 3, now)
	if err != nil {
		t.Fatal(err)
	}
	if !equal(terms, []string{"waving", "hello", "wave"}) {
		t.Fatalf("trending terms = %v", terms)
	}
	shared, _ := s.TopSharedTerms(ctx, 5, now)
	if !equal(shared, []string{"waving"}) {
		t.Fatalf("top shared terms = %v", shared)
	}

	ac, err := s.Autocomplete(ctx, "he", 5, now)
	if err != nil {
		t.Fatal(err)
	}
	if !equal(ac, []string{"hello", "hello there"}) {
		t.Fatalf("autocomplete = %v", ac)
	}

	sug, err := s.Suggestions(ctx, "wave", KindGIF, 5, now)
	if err != nil {
		t.Fatal(err)
	}
	if !equal(sug, []string{"hello"}) {
		t.Fatalf("suggestions = %v", sug)
	}

	if err := s.PruneStats(ctx, now.Add(time.Hour*24)); err != nil {
		t.Fatal(err)
	}
	shared, _ = s.TopSharedTerms(ctx, 5, now)
	if len(shared) != 0 {
		t.Fatalf("pruned terms = %v", shared)
	}
}
