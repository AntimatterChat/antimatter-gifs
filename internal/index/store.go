// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

// Package index keeps the GIFs and stickers known to the service, with their media files, tags,
// categories and share statistics, in a SQLite database.
package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go SQLite driver
)

// Kinds of posts.
const (
	KindGIF     = "gif"
	KindSticker = "sticker"
)

// Content ratings, from the most to the least family friendly.
var Ratings = []string{"g", "pg", "pg13", "r"}

// Post is a GIF or a sticker.
type Post struct {
	ID          string
	Title       string
	Description string
	Kind        string
	// Static is true for still images.
	Static bool
	// Rating is one of Ratings.
	Rating  string
	Created time.Time
	// Width and Height are the dimensions of the full size media, used to filter by aspect ratio.
	Width    int
	Height   int
	HasAudio bool
	BgColor  string
	// Score is the curated popularity of the post, e.g. its rank on the source's featured list.
	Score float64
	// Source, SourceID, SourceURL and Attribution record where third-party content comes from.
	Source      string
	SourceID    string
	SourceURL   string
	Attribution string
	Tags        []string
	// Media maps a Tenor format name (gif, tinygif, mp4, gif_transparent...) to its file.
	Media map[string]Media
}

// Media is one file of a post, in one format.
type Media struct {
	// Path is relative to the media root, with forward slashes.
	Path     string
	Width    int
	Height   int
	Size     int64
	Duration float64
}

// Category is a curated list of posts, shown as a category tile (GIFs) or a sticker pack.
type Category struct {
	Kind       string
	Name       string
	SearchTerm string
	// ImagePostID is the post shown on the category tile; the first member when empty.
	ImagePostID string
	Position    int
}

// ErrNotFound is returned when a post doesn't exist.
var ErrNotFound = errors.New("not found")

// Store is the SQLite index.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS posts (
	id TEXT PRIMARY KEY,
	title TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL CHECK (kind IN ('gif', 'sticker')),
	static INTEGER NOT NULL DEFAULT 0,
	rating TEXT NOT NULL DEFAULT 'g',
	created REAL NOT NULL,
	width INTEGER NOT NULL DEFAULT 0,
	height INTEGER NOT NULL DEFAULT 0,
	has_audio INTEGER NOT NULL DEFAULT 0,
	bg_color TEXT NOT NULL DEFAULT '',
	score REAL NOT NULL DEFAULT 0,
	source TEXT NOT NULL DEFAULT '',
	source_id TEXT NOT NULL DEFAULT '',
	source_url TEXT NOT NULL DEFAULT '',
	attribution TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS posts_source ON posts (source, source_id) WHERE source_id != '';
CREATE INDEX IF NOT EXISTS posts_featured ON posts (kind, score DESC, created DESC);

CREATE TABLE IF NOT EXISTS media (
	post_id TEXT NOT NULL REFERENCES posts (id) ON DELETE CASCADE,
	format TEXT NOT NULL,
	path TEXT NOT NULL,
	width INTEGER NOT NULL DEFAULT 0,
	height INTEGER NOT NULL DEFAULT 0,
	size INTEGER NOT NULL DEFAULT 0,
	duration REAL NOT NULL DEFAULT 0,
	PRIMARY KEY (post_id, format)
);

CREATE TABLE IF NOT EXISTS tags (
	post_id TEXT NOT NULL REFERENCES posts (id) ON DELETE CASCADE,
	tag TEXT NOT NULL,
	position INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (post_id, tag)
);
CREATE INDEX IF NOT EXISTS tags_tag ON tags (tag);

CREATE VIRTUAL TABLE IF NOT EXISTS posts_fts USING fts5 (
	post_id UNINDEXED, title, tags, description,
	tokenize = 'porter unicode61 remove_diacritics 2'
);

CREATE TABLE IF NOT EXISTS categories (
	id INTEGER PRIMARY KEY,
	kind TEXT NOT NULL,
	name TEXT NOT NULL,
	searchterm TEXT NOT NULL,
	image_post_id TEXT NOT NULL DEFAULT '',
	position INTEGER NOT NULL DEFAULT 0,
	UNIQUE (kind, searchterm)
);

CREATE TABLE IF NOT EXISTS category_posts (
	category_id INTEGER NOT NULL REFERENCES categories (id) ON DELETE CASCADE,
	post_id TEXT NOT NULL REFERENCES posts (id) ON DELETE CASCADE,
	position INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (category_id, post_id)
);

-- Shares and searches are only counted per post or term and day: no user or client is stored.
CREATE TABLE IF NOT EXISTS shares (
	post_id TEXT NOT NULL REFERENCES posts (id) ON DELETE CASCADE,
	day INTEGER NOT NULL,
	count INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (post_id, day)
);

CREATE TABLE IF NOT EXISTS terms (
	term TEXT NOT NULL,
	day INTEGER NOT NULL,
	searches INTEGER NOT NULL DEFAULT 0,
	shares INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (term, day)
);
`

// Open opens (and creates or migrates) the index at path. Use ":memory:" for a throwaway index.
func Open(path string) (*Store, error) {
	dsn := "file:" + url.PathEscape(path) + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	if path == ":memory:" {
		// A shared cache keeps one in-memory database for all the pool's connections.
		dsn = "file:memdb" + fmt.Sprint(time.Now().UnixNano()) + "?mode=memory&cache=shared&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	} else {
		dsn += "&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating the index schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Ping checks that the database answers and returns the number of posts.
func (s *Store) Ping(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM posts`).Scan(&n)
	return n, err
}

// UpsertPost adds a post, or replaces the post with the same ID with its media and tags.
func (s *Store) UpsertPost(ctx context.Context, p *Post) error {
	if p.ID == "" || (p.Kind != KindGIF && p.Kind != KindSticker) {
		return fmt.Errorf("invalid post %q of kind %q", p.ID, p.Kind)
	}
	if p.Rating == "" {
		p.Rating = "g"
	}
	if p.Created.IsZero() {
		p.Created = time.Now()
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO posts (id, title, description, kind, static, rating, created, width, height, has_audio,
				bg_color, score, source, source_id, source_url, attribution)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET title = excluded.title, description = excluded.description,
				kind = excluded.kind, static = excluded.static, rating = excluded.rating, created = excluded.created,
				width = excluded.width, height = excluded.height, has_audio = excluded.has_audio,
				bg_color = excluded.bg_color, score = excluded.score, source = excluded.source,
				source_id = excluded.source_id, source_url = excluded.source_url, attribution = excluded.attribution`,
			p.ID, p.Title, p.Description, p.Kind, p.Static, p.Rating, unixSeconds(p.Created), p.Width, p.Height,
			p.HasAudio, p.BgColor, p.Score, p.Source, p.SourceID, p.SourceURL, p.Attribution)
		if err != nil {
			return err
		}
		for _, q := range []string{`DELETE FROM media WHERE post_id = ?`, `DELETE FROM tags WHERE post_id = ?`, `DELETE FROM posts_fts WHERE post_id = ?`} {
			if _, err := tx.ExecContext(ctx, q, p.ID); err != nil {
				return err
			}
		}
		for format, m := range p.Media {
			if _, err := tx.ExecContext(ctx, `INSERT INTO media (post_id, format, path, width, height, size, duration) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				p.ID, format, m.Path, m.Width, m.Height, m.Size, m.Duration); err != nil {
				return err
			}
		}
		tags := NormalizeTags(p.Tags)
		for i, tag := range tags {
			if _, err := tx.ExecContext(ctx, `INSERT INTO tags (post_id, tag, position) VALUES (?, ?, ?)`, p.ID, tag, i); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO posts_fts (post_id, title, tags, description) VALUES (?, ?, ?, ?)`,
			p.ID, p.Title, strings.Join(tags, " "), p.Description)
		return err
	})
}

// DeletePost removes a post.
func (s *Store) DeletePost(ctx context.Context, id string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM posts_fts WHERE post_id = ?`, id); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM posts WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// FindBySource returns the ID of the post imported from the given source item.
func (s *Store) FindBySource(ctx context.Context, source, sourceID string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM posts WHERE source = ? AND source_id = ?`, source, sourceID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// GetPosts returns the posts with the given IDs, in the same order, skipping unknown IDs.
func (s *Store) GetPosts(ctx context.Context, ids []string) ([]*Post, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	in, args := inClause(ids)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, title, description, kind, static, rating, created, width, height, has_audio, bg_color, score,
			source, source_id, source_url, attribution
		FROM posts WHERE id IN (`+in+`)`, args...)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*Post, len(ids))
	for rows.Next() {
		p := &Post{Media: map[string]Media{}}
		var created float64
		if err := rows.Scan(&p.ID, &p.Title, &p.Description, &p.Kind, &p.Static, &p.Rating, &created, &p.Width,
			&p.Height, &p.HasAudio, &p.BgColor, &p.Score, &p.Source, &p.SourceID, &p.SourceURL, &p.Attribution); err != nil {
			rows.Close()
			return nil, err
		}
		p.Created = fromUnixSeconds(created)
		byID[p.ID] = p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.db.QueryContext(ctx, `SELECT post_id, format, path, width, height, size, duration FROM media WHERE post_id IN (`+in+`)`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, format string
		var m Media
		if err := rows.Scan(&id, &format, &m.Path, &m.Width, &m.Height, &m.Size, &m.Duration); err != nil {
			rows.Close()
			return nil, err
		}
		if p := byID[id]; p != nil {
			p.Media[format] = m
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.db.QueryContext(ctx, `SELECT post_id, tag FROM tags WHERE post_id IN (`+in+`) ORDER BY post_id, position`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, tag string
		if err := rows.Scan(&id, &tag); err != nil {
			rows.Close()
			return nil, err
		}
		if p := byID[id]; p != nil {
			p.Tags = append(p.Tags, tag)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	posts := make([]*Post, 0, len(ids))
	for _, id := range ids {
		if p := byID[id]; p != nil {
			posts = append(posts, p)
		}
	}
	return posts, nil
}

// SetCategory creates or replaces a category and its members, in order.
func (s *Store) SetCategory(ctx context.Context, c Category, postIDs []string) error {
	c.SearchTerm = strings.ToLower(strings.TrimSpace(c.SearchTerm))
	if c.SearchTerm == "" || (c.Kind != KindGIF && c.Kind != KindSticker) {
		return fmt.Errorf("invalid category %q of kind %q", c.SearchTerm, c.Kind)
	}
	if c.Name == "" {
		c.Name = c.SearchTerm
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, `
			INSERT INTO categories (kind, name, searchterm, image_post_id, position) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (kind, searchterm) DO UPDATE SET name = excluded.name, image_post_id = excluded.image_post_id,
				position = excluded.position
			RETURNING id`, c.Kind, c.Name, c.SearchTerm, c.ImagePostID, c.Position).Scan(&id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM category_posts WHERE category_id = ?`, id); err != nil {
			return err
		}
		for i, postID := range postIDs {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO category_posts (category_id, post_id, position) VALUES (?, ?, ?)`, id, postID, i); err != nil {
				return fmt.Errorf("adding %s to category %q: %w", postID, c.SearchTerm, err)
			}
		}
		return nil
	})
}

// AddToCategory appends posts to a category, creating it when it doesn't exist.
func (s *Store) AddToCategory(ctx context.Context, c Category, postIDs []string) error {
	c.SearchTerm = NormalizeTerm(c.SearchTerm)
	if c.SearchTerm == "" || (c.Kind != KindGIF && c.Kind != KindSticker) {
		return fmt.Errorf("invalid category %q of kind %q", c.SearchTerm, c.Kind)
	}
	if c.Name == "" {
		c.Name = c.SearchTerm
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, `
			INSERT INTO categories (kind, name, searchterm, position)
			VALUES (?, ?, ?, (SELECT COALESCE(MAX(position) + 1, 0) FROM categories WHERE kind = ?))
			ON CONFLICT (kind, searchterm) DO UPDATE SET name = name
			RETURNING id`, c.Kind, c.Name, c.SearchTerm, c.Kind).Scan(&id)
		if err != nil {
			return err
		}
		for _, postID := range postIDs {
			if _, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO category_posts (category_id, post_id, position)
				VALUES (?, ?, (SELECT COALESCE(MAX(position) + 1, 0) FROM category_posts WHERE category_id = ?))`,
				id, postID, id); err != nil {
				return fmt.Errorf("adding %s to category %q: %w", postID, c.SearchTerm, err)
			}
		}
		return nil
	})
}

// Categories returns the categories of a kind with their tile post (the image post, else the first
// member), in order. Categories without posts are skipped.
func (s *Store) Categories(ctx context.Context, kind string) ([]Category, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.name, c.searchterm, COALESCE(NULLIF(c.image_post_id, ''),
			(SELECT cp.post_id FROM category_posts cp WHERE cp.category_id = c.id ORDER BY cp.position LIMIT 1), ''), c.position
		FROM categories c
		WHERE c.kind = ? AND EXISTS (SELECT 1 FROM category_posts cp WHERE cp.category_id = c.id)
		ORDER BY c.position, c.name`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cats []Category
	for rows.Next() {
		c := Category{Kind: kind}
		if err := rows.Scan(&c.Name, &c.SearchTerm, &c.ImagePostID, &c.Position); err != nil {
			return nil, err
		}
		cats = append(cats, c)
	}
	return cats, rows.Err()
}

// RecordShare counts a share of a post, and of the search term that led to it.
func (s *Store) RecordShare(ctx context.Context, postID, term string, at time.Time) error {
	day := dayOf(at)
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO shares (post_id, day, count) SELECT id, ?, 1 FROM posts WHERE id = ?
			ON CONFLICT (post_id, day) DO UPDATE SET count = count + 1`, day, postID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if term = NormalizeTerm(term); term != "" {
			_, err = tx.ExecContext(ctx, `
				INSERT INTO terms (term, day, shares) VALUES (?, ?, 1)
				ON CONFLICT (term, day) DO UPDATE SET shares = shares + 1`, term, day)
		}
		return err
	})
}

// RecordSearch counts a search for a term.
func (s *Store) RecordSearch(ctx context.Context, term string, at time.Time) error {
	if term = NormalizeTerm(term); term == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO terms (term, day, searches) VALUES (?, ?, 1)
		ON CONFLICT (term, day) DO UPDATE SET searches = searches + 1`, term, dayOf(at))
	return err
}

// PruneStats drops the share and search statistics older than the given time.
func (s *Store) PruneStats(ctx context.Context, before time.Time) error {
	day := dayOf(before)
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM shares WHERE day < ?`, day); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM terms WHERE day < ?`, day)
		return err
	})
}

func (s *Store) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// NormalizeTags lowercases, trims and deduplicates tags, keeping their order.
func NormalizeTags(tags []string) []string {
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.Join(strings.Fields(strings.ToLower(t)), " ")
		t = strings.TrimPrefix(t, "#")
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// NormalizeTerm lowercases and trims a search term; terms over 50 characters are dropped.
func NormalizeTerm(term string) string {
	term = strings.Join(strings.Fields(strings.ToLower(term)), " ")
	if len([]rune(term)) > 50 {
		return ""
	}
	return term
}

func inClause(values []string) (string, []any) {
	args := make([]any, len(values))
	for i, v := range values {
		args[i] = v
	}
	return strings.TrimSuffix(strings.Repeat("?, ", len(values)), ", "), args
}

func dayOf(t time.Time) int64 {
	return t.UTC().Unix() / 86400
}

func unixSeconds(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}

func fromUnixSeconds(f float64) time.Time {
	sec := int64(f)
	return time.Unix(sec, int64((f-float64(sec))*1e9))
}
