// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

package index

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Filter restricts the posts returned by searches and the featured list.
type Filter struct {
	// Kind is KindGIF or KindSticker.
	Kind string
	// Static, when set, keeps only still (true) or only animated (false) posts.
	Static *bool
	// Ratings lists the allowed content ratings; empty allows all.
	Ratings []string
	// MinAspect and MaxAspect bound width/height of the full size media when non zero.
	MinAspect float64
	MaxAspect float64
}

// TrendingWindow is how far back shares count for trending posts and terms.
const TrendingWindow = 7 * 24 * time.Hour

// maxCandidates bounds the posts ranked for one search.
const maxCandidates = 2000

func (f Filter) where() (string, []any) {
	conds := []string{"p.kind = ?"}
	args := []any{f.Kind}
	if f.Static != nil {
		conds = append(conds, "p.static = ?")
		args = append(args, *f.Static)
	}
	if len(f.Ratings) > 0 {
		in, inArgs := inClause(f.Ratings)
		conds = append(conds, "p.rating IN ("+in+")")
		args = append(args, inArgs...)
	}
	if f.MinAspect > 0 {
		conds = append(conds, "p.height > 0 AND CAST(p.width AS REAL) / p.height >= ?")
		args = append(args, f.MinAspect)
	}
	if f.MaxAspect > 0 {
		conds = append(conds, "p.height > 0 AND CAST(p.width AS REAL) / p.height <= ?")
		args = append(args, f.MaxAspect)
	}
	return strings.Join(conds, " AND "), args
}

// Featured returns a page of the featured posts: curated score plus recent shares, newest first.
// It also tells whether there are more.
func (s *Store) Featured(ctx context.Context, f Filter, offset, limit int, now time.Time) ([]*Post, bool, error) {
	where, args := f.where()
	args = append([]any{dayOf(now.Add(-TrendingWindow))}, args...)
	args = append(args, limit+1, offset)
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id FROM posts p
		LEFT JOIN (SELECT post_id, SUM(count) AS n FROM shares WHERE day >= ? GROUP BY post_id) sh ON sh.post_id = p.id
		WHERE `+where+`
		ORDER BY p.score + 5 * COALESCE(sh.n, 0) DESC, p.created DESC, p.id
		LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, false, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, false, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(ids) > limit
	if more {
		ids = ids[:limit]
	}
	posts, err := s.GetPosts(ctx, ids)
	return posts, more, err
}

type candidate struct {
	id      string
	rank    float64
	created float64
}

// Search returns a page of the posts matching the query, best first, and whether there are more.
//
// Ranking: BM25 over the title, tags and description (stemmed, prefix matching on every word),
// scaled by the share of query words the post matches, plus a small popularity boost (curated
// score and recent shares). When the query is exactly the search term of a category, its curated
// members come first, in their order.
func (s *Store) Search(ctx context.Context, query string, f Filter, offset, limit int, now time.Time) ([]*Post, bool, error) {
	words := QueryWords(query)
	if len(words) == 0 {
		return nil, false, nil
	}
	ranked := map[string]*candidate{}

	// Curated members of the category with this exact search term.
	where, args := f.where()
	catArgs := append([]any{f.Kind, NormalizeTerm(query)}, args...)
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, cp.position, p.created FROM categories c
		JOIN category_posts cp ON cp.category_id = c.id
		JOIN posts p ON p.id = cp.post_id
		WHERE c.kind = ? AND c.searchterm = ? AND `+where, catArgs...)
	if err != nil {
		return nil, false, err
	}
	for rows.Next() {
		var c candidate
		var pos int
		if err := rows.Scan(&c.id, &pos, &c.created); err != nil {
			rows.Close()
			return nil, false, err
		}
		c.rank = 1e6 - float64(pos)
		ranked[c.id] = &c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	match := make([]string, len(words))
	for i, w := range words {
		match[i] = `"` + strings.ReplaceAll(w, `"`, `""`) + `"*`
	}
	ftsArgs := append([]any{dayOf(now.Add(-TrendingWindow)), strings.Join(match, " OR ")}, args...)
	ftsArgs = append(ftsArgs, maxCandidates)
	rows, err = s.db.QueryContext(ctx, `
		SELECT p.id, bm25(posts_fts, 0.0, 4.0, 3.0, 1.0), p.score, COALESCE(sh.n, 0), p.created,
			f.title || ' ' || f.tags || ' ' || f.description
		FROM posts_fts f
		JOIN posts p ON p.id = f.post_id
		LEFT JOIN (SELECT post_id, SUM(count) AS n FROM shares WHERE day >= ? GROUP BY post_id) sh ON sh.post_id = p.id
		WHERE posts_fts MATCH ? AND `+where+`
		ORDER BY bm25(posts_fts, 0.0, 4.0, 3.0, 1.0)
		LIMIT ?`, ftsArgs...)
	if err != nil {
		return nil, false, err
	}
	for rows.Next() {
		var c candidate
		var bm25, score, shares float64
		var text string
		if err := rows.Scan(&c.id, &bm25, &score, &shares, &c.created, &text); err != nil {
			rows.Close()
			return nil, false, err
		}
		coverage := wordCoverage(words, Tokenize(text))
		c.rank = -bm25*coverage*coverage + 0.2*math.Log1p(math.Max(score, 0)+5*shares)
		if ranked[c.id] != nil {
			continue // curated, keeps its place
		}
		ranked[c.id] = &c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	list := make([]*candidate, 0, len(ranked))
	for _, c := range ranked {
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].rank != list[j].rank {
			return list[i].rank > list[j].rank
		}
		if list[i].created != list[j].created {
			return list[i].created > list[j].created
		}
		return list[i].id < list[j].id
	})
	if offset >= len(list) {
		return nil, false, nil
	}
	end := min(offset+limit, len(list))
	ids := make([]string, 0, end-offset)
	for _, c := range list[offset:end] {
		ids = append(ids, c.id)
	}
	posts, err := s.GetPosts(ctx, ids)
	return posts, end < len(list), err
}

// wordCoverage is the share of query words that start a word of the text (prefix matching, like
// the full-text query), with a light plural/suffix tolerance through the shared stem.
func wordCoverage(query, text []string) float64 {
	matched := 0
	for _, q := range query {
		for _, t := range text {
			if strings.HasPrefix(t, q) || strings.HasPrefix(q, t) && len(t) >= 3 && len(q)-len(t) <= 2 {
				matched++
				break
			}
		}
	}
	return float64(matched) / float64(len(query))
}

// Tokenize splits text into lowercase words (letters and digits).
func Tokenize(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// QueryWords returns the words of a search query, at most 8 of them.
func QueryWords(query string) []string {
	words := Tokenize(query)
	if len(words) > 8 {
		words = words[:8]
	}
	return words
}

// Autocomplete returns the tags and popular search terms starting with the prefix.
func (s *Store) Autocomplete(ctx context.Context, prefix string, limit int, now time.Time) ([]string, error) {
	prefix = NormalizeTerm(prefix)
	if prefix == "" {
		return nil, nil
	}
	like := escapeLike(prefix) + "%"
	rows, err := s.db.QueryContext(ctx, `
		SELECT term FROM (
			SELECT term, SUM(weight) AS w FROM (
				SELECT tag AS term, COUNT(*) AS weight FROM tags WHERE tag LIKE ? ESCAPE '\' GROUP BY tag
				UNION ALL
				SELECT term, SUM(searches + 3 * shares) AS weight FROM terms WHERE term LIKE ? ESCAPE '\' AND day >= ? GROUP BY term
			)
			GROUP BY term
		)
		ORDER BY term = ? DESC, w DESC, length(term), term
		LIMIT ?`, like, like, dayOf(now.Add(-4*TrendingWindow)), prefix, limit)
	if err != nil {
		return nil, err
	}
	return scanStrings(rows)
}

// Suggestions returns search terms related to the query: the tags that most often come with the
// best matches of the query, and the categories that contain its words.
func (s *Store) Suggestions(ctx context.Context, query string, kind string, limit int, now time.Time) ([]string, error) {
	words := QueryWords(query)
	if len(words) == 0 {
		return nil, nil
	}
	posts, _, err := s.Search(ctx, query, Filter{Kind: kind}, 0, 50, now)
	if err != nil {
		return nil, err
	}
	norm := NormalizeTerm(query)
	weights := map[string]float64{}
	for i, p := range posts {
		for _, tag := range p.Tags {
			if tag == norm {
				continue
			}
			weights[tag] += 1 / float64(i+3)
		}
	}
	cats, err := s.Categories(ctx, kind)
	if err != nil {
		return nil, err
	}
	for _, c := range cats {
		if c.SearchTerm != norm && wordCoverage(words, Tokenize(c.SearchTerm)) > 0 {
			weights[c.SearchTerm] += 1
		}
	}
	return topTerms(weights, limit), nil
}

// TrendingTerms returns the most shared and searched terms of the last week, completed with the
// featured categories and the most used tags.
func (s *Store) TrendingTerms(ctx context.Context, limit int, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT term FROM terms WHERE day >= ? GROUP BY term
		HAVING SUM(searches + 3 * shares) >= 2
		ORDER BY SUM(searches + 3 * shares) DESC, term LIMIT ?`, dayOf(now.Add(-TrendingWindow)), limit)
	if err != nil {
		return nil, err
	}
	terms, err := scanStrings(rows)
	if err != nil || len(terms) >= limit {
		return terms, err
	}
	seen := map[string]bool{}
	for _, t := range terms {
		seen[t] = true
	}
	add := func(t string) {
		if len(terms) < limit && !seen[t] {
			seen[t] = true
			terms = append(terms, t)
		}
	}
	cats, err := s.Categories(ctx, KindGIF)
	if err != nil {
		return nil, err
	}
	for _, c := range cats {
		add(c.SearchTerm)
	}
	rows, err = s.db.QueryContext(ctx, `SELECT tag FROM tags GROUP BY tag ORDER BY COUNT(*) DESC, tag LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	tags, err := scanStrings(rows)
	if err != nil {
		return nil, err
	}
	for _, t := range tags {
		add(t)
	}
	return terms, nil
}

// TopSharedTerms returns the terms shared the most in the trending window, to build trending
// categories.
func (s *Store) TopSharedTerms(ctx context.Context, limit int, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT term FROM terms WHERE day >= ? GROUP BY term HAVING SUM(shares) > 0
		ORDER BY SUM(shares) DESC, SUM(searches) DESC, term LIMIT ?`, dayOf(now.Add(-TrendingWindow)), limit)
	if err != nil {
		return nil, err
	}
	return scanStrings(rows)
}

func topTerms(weights map[string]float64, limit int) []string {
	terms := make([]string, 0, len(weights))
	for t := range weights {
		terms = append(terms, t)
	}
	sort.Slice(terms, func(i, j int) bool {
		if weights[terms[i]] != weights[terms[j]] {
			return weights[terms[i]] > weights[terms[j]]
		}
		return terms[i] < terms[j]
	})
	if len(terms) > limit {
		terms = terms[:limit]
	}
	return terms
}

type stringRows interface {
	Next() bool
	Scan(...any) error
	Close() error
	Err() error
}

func scanStrings(rows stringRows) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
