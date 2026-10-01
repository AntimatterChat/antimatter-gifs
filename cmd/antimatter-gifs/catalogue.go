// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/antimatterchat/antimatter-gifs/internal/importer"
	"github.com/antimatterchat/antimatter-gifs/internal/index"
	"github.com/antimatterchat/antimatter-gifs/internal/media"
	"github.com/antimatterchat/antimatter-gifs/internal/tenor"
)

// importerFlags are the flags of the commands that import.
type importerFlags struct {
	storage
	noVariants *bool
	fps        *int
}

func newImporterFlags(fs *flag.FlagSet) importerFlags {
	return importerFlags{
		storage:    storageFlags(fs),
		noVariants: fs.Bool("no-variants", false, "keep the source files only, don't generate the other formats"),
		fps:        fs.Int("fps", 20, "frame rate of stickers rendered from SVG"),
	}
}

func (f importerFlags) importer(ctx context.Context) (*importer.Importer, func(), error) {
	store, err := f.open()
	if err != nil {
		return nil, nil, err
	}
	tools := media.DetectTools(ctx)
	slog.Info("media tools", "found", tools.String())
	if tools.FFmpeg == "" && !*f.noVariants {
		slog.Warn("ffmpeg isn't installed: only the source files are kept, other formats aren't generated")
	}
	im := &importer.Importer{
		Store:    store,
		MediaDir: f.mediaDir(),
		Tools:    tools,
		Options:  media.Options{NoVariants: *f.noVariants, FPS: *f.fps, Logger: slog.Default()},
		Logger:   slog.Default(),
	}
	return im, func() { store.Close() }, nil
}

func runImport(ctx context.Context, f importerFlags, m *importer.Manifest) error {
	im, done, err := f.importer(ctx)
	if err != nil {
		return err
	}
	defer done()
	stats, err := im.Import(ctx, m)
	slog.Info("import finished", "added", stats.Added, "updated", stats.Updated, "failed", stats.Failed)
	if err != nil {
		return err
	}
	if stats.Failed > 0 {
		return fmt.Errorf("%d items failed", stats.Failed)
	}
	return nil
}

func importFiles(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	f := newImporterFlags(fs)
	manifest := fs.String("manifest", "", "import this manifest instead of files")
	sticker := fs.Bool("sticker", false, "the files are stickers (transparent formats)")
	title := fs.String("title", "", "title (default: from the file name)")
	description := fs.String("description", "", "description, searched too")
	tags := fs.String("tags", "", "comma-separated tags")
	rating := fs.String("rating", "g", "content rating: g, pg, pg13 or r")
	category := fs.String("category", "", "add the files to this category (its search term), after its current members")
	source := fs.String("source", "", "where the files come from, e.g. an artist or site")
	sourceURL := fs.String("source-url", "", "URL of the original")
	attribution := fs.String("attribution", "", "credit to show for the files")
	fs.Parse(args)

	if *manifest != "" {
		m, err := importer.LoadManifest(*manifest)
		if err != nil {
			return err
		}
		return runImport(ctx, f, m)
	}
	if fs.NArg() == 0 {
		return errors.New("nothing to import: give files or -manifest")
	}
	validRating := false
	for _, r := range index.Ratings {
		validRating = validRating || r == *rating
	}
	if !validRating {
		return fmt.Errorf("invalid rating %q", *rating)
	}
	kind := index.KindGIF
	if *sticker {
		kind = index.KindSticker
	}
	m := &importer.Manifest{Kind: kind}
	for _, file := range fs.Args() {
		abs, err := filepath.Abs(file)
		if err != nil {
			return err
		}
		m.Items = append(m.Items, importer.Item{
			File: abs, Title: *title, Description: *description, Tags: splitComma(*tags), Rating: *rating,
			Source: *source, SourceURL: *sourceURL, Attribution: *attribution,
		})
	}
	if *category == "" {
		return runImport(ctx, f, m)
	}
	im, done, err := f.importer(ctx)
	if err != nil {
		return err
	}
	defer done()
	stats, err := im.Import(ctx, m)
	slog.Info("import finished", "added", stats.Added, "updated", stats.Updated, "failed", stats.Failed)
	if err != nil {
		return err
	}
	cat := index.Category{Kind: kind, Name: *category, SearchTerm: *category}
	if err := im.Store.AddToCategory(ctx, cat, stats.IDs); err != nil {
		return err
	}
	if stats.Failed > 0 {
		return fmt.Errorf("%d items failed", stats.Failed)
	}
	return nil
}

func importStickers(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stickers", flag.ExitOnError)
	f := newImporterFlags(fs)
	pack := fs.String("pack", env("STICKER_PACK", "stickers/pack.json"), "sticker pack file (AM_GIFS_STICKER_PACK)")
	fs.Parse(args)
	m, err := importer.StickerPackManifest(*pack)
	if err != nil {
		return err
	}
	return runImport(ctx, f, m)
}

func fetchTenor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fetch-tenor", flag.ExitOnError)
	f := newImporterFlags(fs)
	out := fs.String("out", "", "download directory (default DATA/tenor)")
	baseURL := fs.String("base-url", tenor.DefaultBaseURL, "Tenor API base URL")
	featured := fs.Int("featured", 200, "featured GIFs to fetch")
	categories := fs.Int("categories", 12, "featured categories to fetch")
	perCategory := fs.Int("per-category", 12, "GIFs per category")
	stickers := fs.Int("stickers", 48, "featured stickers to fetch")
	contentFilter := fs.String("contentfilter", "medium", "Tenor content filter: off, low, medium or high")
	locale := fs.String("locale", "en_US", "Tenor locale")
	noImport := fs.Bool("no-import", false, "only download, don't import")
	fs.Parse(args)

	if *out == "" {
		*out = filepath.Join(*f.data, "tenor")
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	fetcher := &tenor.Fetcher{
		Key:           os.Getenv("TENOR_API_KEY"),
		ClientKey:     env("TENOR_CLIENT_KEY", "antimatter-gifs"),
		BaseURL:       *baseURL,
		Locale:        *locale,
		ContentFilter: *contentFilter,
		Logger:        slog.Default(),
		Pause:         100 * time.Millisecond,
	}
	m, err := fetcher.Fetch(ctx, *out, tenor.Limits{Featured: *featured, Categories: *categories, PerCategory: *perCategory, Stickers: *stickers})
	if err != nil {
		return err
	}
	if *noImport {
		slog.Info("downloaded", "manifest", filepath.Join(*out, "manifest.json"))
		return nil
	}
	// Tenor's files are already in the formats clients use: no conversion.
	*f.noVariants = true
	return runImport(ctx, f, m)
}

func deletePosts(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("delete", flag.ExitOnError)
	f := newImporterFlags(fs)
	fs.Parse(args)
	if fs.NArg() == 0 {
		return errors.New("give the IDs of the posts to delete")
	}
	im, done, err := f.importer(ctx)
	if err != nil {
		return err
	}
	defer done()
	for _, id := range fs.Args() {
		if err := im.Delete(ctx, id); err != nil {
			return fmt.Errorf("deleting %s: %w", id, err)
		}
		slog.Info("deleted", "id", id)
	}
	return nil
}
