// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

// Package importer adds GIFs and stickers to the index from a manifest: a JSON list of items
// (a file to convert, or ready-made files per format) and categories.
package importer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/antimatterchat/antimatter-gifs/internal/index"
	"github.com/antimatterchat/antimatter-gifs/internal/media"
)

// Manifest lists the items to import. Item fields left empty take the manifest's defaults.
type Manifest struct {
	Kind        string     `json:"kind,omitempty"`
	Source      string     `json:"source,omitempty"`
	Attribution string     `json:"attribution,omitempty"`
	Items       []Item     `json:"items"`
	Categories  []Category `json:"categories,omitempty"`
	// Dir is the directory relative file paths are resolved from (the manifest's directory).
	Dir string `json:"-"`
}

// Item is a GIF or sticker to import.
type Item struct {
	// Key identifies the item in the categories of the manifest; defaults to SourceID, then File.
	Key         string   `json:"key,omitempty"`
	Kind        string   `json:"kind,omitempty"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Rating      string   `json:"rating,omitempty"`
	Score       float64  `json:"score,omitempty"`
	Static      bool     `json:"static,omitempty"`
	HasAudio    bool     `json:"has_audio,omitempty"`
	BgColor     string   `json:"bg_color,omitempty"`
	// Created is a Unix timestamp; the import time when zero.
	Created     float64 `json:"created,omitempty"`
	Source      string  `json:"source,omitempty"`
	SourceID    string  `json:"source_id,omitempty"`
	SourceURL   string  `json:"source_url,omitempty"`
	Attribution string  `json:"attribution,omitempty"`
	// File is converted to the Tenor formats (see the media package).
	File string `json:"file,omitempty"`
	// Media are ready-made files per format, used as they are instead of File.
	Media map[string]MediaFile `json:"media,omitempty"`
}

// MediaFile is a ready-made file of an item; zero dimensions and duration are probed.
type MediaFile struct {
	File     string  `json:"file"`
	Dims     [2]int  `json:"dims,omitempty"`
	Duration float64 `json:"duration,omitempty"`
}

// Category is a GIF category or a sticker pack, listing item keys in order.
type Category struct {
	Kind       string   `json:"kind,omitempty"`
	Name       string   `json:"name"`
	SearchTerm string   `json:"searchterm"`
	Position   int      `json:"position,omitempty"`
	Image      string   `json:"image,omitempty"`
	Items      []string `json:"items"`
}

// LoadManifest reads a manifest file.
func LoadManifest(file string) (*Manifest, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	m.Dir = filepath.Dir(file)
	return &m, nil
}

// Importer imports manifests into the index and the media directory.
type Importer struct {
	Store    *index.Store
	MediaDir string
	Tools    media.Tools
	Options  media.Options
	Logger   *slog.Logger
	// Now returns the current time; time.Now when nil.
	Now func() time.Time
}

// Stats counts the outcome of an import.
type Stats struct {
	Added, Updated, Failed int
	// IDs are the IDs of the imported posts, in the order of the manifest.
	IDs []string
}

// Import imports the items of the manifest, then its categories. Failed items are logged and
// counted, and skipped from categories.
func (im *Importer) Import(ctx context.Context, m *Manifest) (Stats, error) {
	var stats Stats
	if im.Logger == nil {
		im.Logger = slog.Default()
	}
	if im.Now == nil {
		im.Now = time.Now
	}
	ids := map[string]string{}
	for i := range m.Items {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		item := m.Items[i]
		id, added, err := im.importItem(ctx, m, &item)
		if err != nil {
			stats.Failed++
			im.Logger.Error("import failed", "item", firstNonEmpty(item.Key, item.SourceID, item.File, item.Title), "error", err)
			continue
		}
		stats.IDs = append(stats.IDs, id)
		if added {
			stats.Added++
		} else {
			stats.Updated++
		}
		for _, key := range []string{item.Key, item.SourceID, item.File} {
			if key != "" {
				ids[key] = id
			}
		}
		im.Logger.Info("imported", "id", id, "item", firstNonEmpty(item.Title, item.File, item.Key), "new", added)
	}
	for _, c := range m.Categories {
		kind := firstNonEmpty(c.Kind, m.Kind, index.KindGIF)
		var members []string
		for _, key := range c.Items {
			if id := ids[key]; id != "" {
				members = append(members, id)
			}
		}
		cat := index.Category{Kind: kind, Name: c.Name, SearchTerm: c.SearchTerm, Position: c.Position, ImagePostID: ids[c.Image]}
		if err := im.Store.SetCategory(ctx, cat, members); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

func (im *Importer) importItem(ctx context.Context, m *Manifest, item *Item) (string, bool, error) {
	kind := firstNonEmpty(item.Kind, m.Kind, index.KindGIF)
	if kind != index.KindGIF && kind != index.KindSticker {
		return "", false, fmt.Errorf("unknown kind %q", kind)
	}
	if item.File == "" && len(item.Media) == 0 {
		return "", false, errors.New("an item needs a file or media")
	}
	source := firstNonEmpty(item.Source, m.Source, "local")
	if item.SourceID == "" {
		if item.File == "" {
			return "", false, errors.New("an item without file needs a source_id")
		}
		sum, err := fileHash(m.resolve(item.File))
		if err != nil {
			return "", false, err
		}
		item.SourceID = "sha256:" + sum
	}

	id, err := im.Store.FindBySource(ctx, source, item.SourceID)
	added := errors.Is(err, index.ErrNotFound)
	switch {
	case added:
		id = newID()
	case err != nil:
		return "", false, err
	}

	rel := path.Join(id[len(id)-2:], id)
	dir := filepath.Join(im.MediaDir, filepath.FromSlash(rel))
	// The files are made in a new directory that replaces the current one once complete.
	tmp := dir + ".new"
	if err := os.RemoveAll(tmp); err != nil {
		return "", false, err
	}
	files, err := im.mediaFiles(ctx, m, item, kind, tmp, rel)
	if err == nil {
		if err = os.RemoveAll(dir); err == nil {
			err = os.Rename(tmp, dir)
		}
	}
	if err != nil {
		os.RemoveAll(tmp)
		return "", false, err
	}

	created := im.Now()
	if item.Created > 0 {
		sec := int64(item.Created)
		created = time.Unix(sec, int64((item.Created-float64(sec))*1e9))
	}
	post := &index.Post{
		ID: id, Title: item.Title, Description: item.Description, Kind: kind, Static: item.Static,
		Rating: item.Rating, Created: created, HasAudio: item.HasAudio, BgColor: item.BgColor, Score: item.Score,
		Source: source, SourceID: item.SourceID, SourceURL: item.SourceURL,
		Attribution: firstNonEmpty(item.Attribution, m.Attribution), Tags: item.Tags, Media: files,
	}
	for _, f := range []string{"gif", "gif_transparent", "mp4", "webp_transparent", "preview", "mediumgif", "tinygif"} {
		if mf, ok := files[f]; ok && mf.Width > 0 {
			post.Width, post.Height = mf.Width, mf.Height
			break
		}
	}
	post.Static = item.Static || isStill(files)
	if post.Title == "" && item.File != "" {
		post.Title = titleFromFile(item.File)
	}
	if err := im.Store.UpsertPost(ctx, post); err != nil {
		return "", false, err
	}
	return id, added, nil
}

// isStill tells whether the files are still images: PNG or JPEG files, or single frame GIFs.
func isStill(files map[string]index.Media) bool {
	for _, f := range files {
		switch path.Ext(f.Path) {
		case ".png", ".jpg", ".jpeg":
		case ".gif":
			if f.Duration > 0 {
				return false
			}
		default:
			return false
		}
	}
	return len(files) > 0
}

func (im *Importer) mediaFiles(ctx context.Context, m *Manifest, item *Item, kind, dir, rel string) (map[string]index.Media, error) {
	if len(item.Media) == 0 {
		return media.Generate(ctx, im.Tools, m.resolve(item.File), kind, dir, rel, im.Options)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	files := map[string]index.Media{}
	for format, mf := range item.Media {
		if !index.KnownFormat(format) {
			return nil, fmt.Errorf("unknown format %q", format)
		}
		src := m.resolve(mf.File)
		name := format + strings.ToLower(filepath.Ext(src))
		if err := copyFile(src, filepath.Join(dir, name)); err != nil {
			return nil, err
		}
		probed, err := media.Probe(ctx, im.Tools, filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("probing %s: %w", mf.File, err)
		}
		if mf.Dims[0] > 0 {
			probed.Width, probed.Height = mf.Dims[0], mf.Dims[1]
		}
		if mf.Duration > 0 {
			probed.Duration = mf.Duration
		}
		probed.Path = path.Join(rel, name)
		files[format] = probed
	}
	return files, nil
}

func (m *Manifest) resolve(file string) string {
	if filepath.IsAbs(file) || m.Dir == "" {
		return file
	}
	return filepath.Join(m.Dir, file)
}

// Delete removes a post and its files.
func (im *Importer) Delete(ctx context.Context, id string) error {
	if len(id) < 2 || strings.ContainsAny(id, `/\.`) {
		return fmt.Errorf("invalid id %q", id)
	}
	if err := im.Store.DeletePost(ctx, id); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(im.MediaDir, id[len(id)-2:], id))
}

// newID returns a random numeric ID, like Tenor's.
func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	n := binary.BigEndian.Uint64(b[:]) >> 2
	return strconv.FormatUint(n|1<<60, 10)
}

func fileHash(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)[:16]), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func titleFromFile(file string) string {
	name := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	name = strings.Join(strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == '.' }), " ")
	if name == "" {
		return ""
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
