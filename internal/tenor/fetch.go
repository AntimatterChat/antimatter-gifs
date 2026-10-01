// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

// Package tenor builds a starter dataset from the Tenor API: the featured GIFs, the GIFs of the
// featured categories and the featured stickers, with their metadata and attribution, saved as a
// manifest for the importer. It only uses the official API with the operator's own API key.
package tenor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/antimatterchat/antimatter-gifs/internal/importer"
	"github.com/antimatterchat/antimatter-gifs/internal/index"
)

// DefaultBaseURL is the Tenor API v2.
const DefaultBaseURL = "https://tenor.googleapis.com/v2"

// Default formats downloaded for GIFs and stickers.
var (
	DefaultGIFFormats     = []string{"gif", "tinygif", "nanogif", "mp4", "tinymp4", "nanomp4", "preview"}
	DefaultStickerFormats = []string{"gif_transparent", "tinygif_transparent", "nanogif_transparent",
		"webp_transparent", "tinywebp_transparent", "nanowebp_transparent", "preview"}
)

// maxFileSize skips huge files.
const maxFileSize = 12 << 20

// Fetcher downloads a dataset from Tenor.
type Fetcher struct {
	Key       string
	ClientKey string
	BaseURL   string
	Locale    string
	Country   string
	// ContentFilter is Tenor's contentfilter (off, low, medium or high); the imported posts get the
	// matching rating, so the service's own content filter keeps working.
	ContentFilter  string
	GIFFormats     []string
	StickerFormats []string
	HTTP           *http.Client
	Logger         *slog.Logger
	// Pause is the delay between two requests to Tenor.
	Pause time.Duration
}

// Limits says how much to fetch.
type Limits struct {
	Featured    int
	Categories  int
	PerCategory int
	Stickers    int
}

type result struct {
	ID                 string   `json:"id"`
	Title              string   `json:"title"`
	ContentDescription string   `json:"content_description"`
	Created            float64  `json:"created"`
	ItemURL            string   `json:"itemurl"`
	Tags               []string `json:"tags"`
	Flags              []string `json:"flags"`
	HasAudio           bool     `json:"hasaudio"`
	BgColor            string   `json:"bg_color"`
	MediaFormats       map[string]struct {
		URL      string  `json:"url"`
		Dims     [2]int  `json:"dims"`
		Duration float64 `json:"duration"`
		Size     int64   `json:"size"`
	} `json:"media_formats"`
}

// Rating returns the content rating of posts fetched with a Tenor contentfilter.
func Rating(contentFilter string) string {
	switch contentFilter {
	case "high":
		return "g"
	case "medium":
		return "pg"
	case "low":
		return "pg13"
	}
	return "r"
}

// Fetch downloads the dataset into dir (files/<id>/<format>.<ext>) and writes dir/manifest.json,
// which it returns. Files already downloaded are kept, so an interrupted fetch can be resumed.
func (f *Fetcher) Fetch(ctx context.Context, dir string, lim Limits) (*importer.Manifest, error) {
	if f.Key == "" {
		return nil, errors.New("a Tenor API key is needed (TENOR_API_KEY)")
	}
	f.defaults()
	m := &importer.Manifest{Source: "tenor", Attribution: "Via Tenor", Dir: dir}
	seen := map[string]bool{}
	rating := Rating(f.ContentFilter)
	featured := 0
	add := func(r result, kind string, score float64) error {
		if seen[r.ID] {
			return nil
		}
		item, err := f.download(ctx, dir, r, kind)
		if err != nil {
			f.Logger.Warn("skipping a Tenor post", "id", r.ID, "error", err)
			return nil
		}
		item.Rating, item.Score = rating, score
		seen[r.ID] = true
		m.Items = append(m.Items, *item)
		return ctx.Err()
	}

	// Featured GIFs, scored by rank so that the service's featured list keeps Tenor's order.
	results, err := f.list(ctx, "featured", url.Values{}, lim.Featured, f.GIFFormats)
	if err != nil {
		return nil, fmt.Errorf("featured GIFs: %w", err)
	}
	for i, r := range results {
		if err := add(r, index.KindGIF, float64(len(results)-i)); err != nil {
			return nil, err
		}
		featured++
	}

	// The GIFs of the featured categories.
	if lim.Categories > 0 && lim.PerCategory > 0 {
		cats, err := f.categories(ctx)
		if err != nil {
			return nil, fmt.Errorf("categories: %w", err)
		}
		for i, c := range cats {
			if i >= lim.Categories {
				break
			}
			results, err := f.list(ctx, "search", url.Values{"q": {c.SearchTerm}}, lim.PerCategory, f.GIFFormats)
			if err != nil {
				return nil, fmt.Errorf("category %q: %w", c.SearchTerm, err)
			}
			cat := importer.Category{Kind: index.KindGIF, Name: c.Name, SearchTerm: c.SearchTerm, Position: i}
			for _, r := range results {
				if err := add(r, index.KindGIF, 0); err != nil {
					return nil, err
				}
				if seen[r.ID] {
					cat.Items = append(cat.Items, r.ID)
				}
			}
			if len(cat.Items) > 0 {
				cat.Image = cat.Items[0]
				m.Categories = append(m.Categories, cat)
			}
		}
	}

	// Featured stickers.
	if lim.Stickers > 0 {
		results, err := f.list(ctx, "featured", url.Values{"searchfilter": {"sticker"}}, lim.Stickers, f.StickerFormats)
		if err != nil {
			return nil, fmt.Errorf("featured stickers: %w", err)
		}
		for i, r := range results {
			if err := add(r, index.KindSticker, float64(len(results)-i)); err != nil {
				return nil, err
			}
		}
	}

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644); err != nil {
		return nil, err
	}
	f.Logger.Info("fetched from Tenor", "posts", len(m.Items), "featured", featured, "categories", len(m.Categories))
	return m, nil
}

func (f *Fetcher) defaults() {
	if f.BaseURL == "" {
		f.BaseURL = DefaultBaseURL
	}
	f.BaseURL = strings.TrimSuffix(f.BaseURL, "/")
	if f.ClientKey == "" {
		f.ClientKey = "antimatter-gifs"
	}
	if f.ContentFilter == "" {
		f.ContentFilter = "medium"
	}
	if len(f.GIFFormats) == 0 {
		f.GIFFormats = DefaultGIFFormats
	}
	if len(f.StickerFormats) == 0 {
		f.StickerFormats = DefaultStickerFormats
	}
	if f.HTTP == nil {
		f.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	if f.Logger == nil {
		f.Logger = slog.Default()
	}
}

// get calls an endpoint of the API.
func (f *Fetcher) get(ctx context.Context, endpoint string, params url.Values, out any) error {
	params.Set("key", f.Key)
	params.Set("client_key", f.ClientKey)
	if f.Locale != "" {
		params.Set("locale", f.Locale)
	}
	if f.Country != "" {
		params.Set("country", f.Country)
	}
	params.Set("contentfilter", f.ContentFilter)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.BaseURL+"/"+endpoint+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	if f.Pause > 0 {
		select {
		case <-time.After(f.Pause):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	resp, err := f.HTTP.Do(req)
	if err != nil {
		// The URL holds the key: don't print it.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return uerr.Err
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%s: %s: %s", endpoint, resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// list pages through featured or search results.
func (f *Fetcher) list(ctx context.Context, endpoint string, params url.Values, n int, formats []string) ([]result, error) {
	var all []result
	pos := ""
	for len(all) < n {
		p := url.Values{}
		for k, v := range params {
			p[k] = v
		}
		p.Set("limit", strconv.Itoa(min(50, n-len(all))))
		p.Set("media_filter", strings.Join(formats, ","))
		if pos != "" {
			p.Set("pos", pos)
		}
		var page struct {
			Results []result `json:"results"`
			Next    string   `json:"next"`
		}
		if err := f.get(ctx, endpoint, p, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Results...)
		if page.Next == "" || page.Next == pos || len(page.Results) == 0 {
			break
		}
		pos = page.Next
	}
	if len(all) > n {
		all = all[:n]
	}
	return all, nil
}

type category struct {
	SearchTerm string `json:"searchterm"`
	Name       string `json:"name"`
}

func (f *Fetcher) categories(ctx context.Context) ([]category, error) {
	var res struct {
		Tags []category `json:"tags"`
	}
	err := f.get(ctx, "categories", url.Values{"type": {"featured"}}, &res)
	return res.Tags, err
}

// download saves the files of a result and returns its manifest item.
func (f *Fetcher) download(ctx context.Context, dir string, r result, kind string) (*importer.Item, error) {
	if r.ID == "" || strings.ContainsAny(r.ID, `/\.`) {
		return nil, fmt.Errorf("invalid id %q", r.ID)
	}
	formats := f.GIFFormats
	if kind == index.KindSticker {
		formats = f.StickerFormats
	}
	item := &importer.Item{
		Key: r.ID, Kind: kind, Title: r.Title, Description: r.ContentDescription, Tags: r.Tags,
		Created: r.Created, SourceID: r.ID, SourceURL: r.ItemURL, HasAudio: r.HasAudio, BgColor: r.BgColor,
		Media: map[string]importer.MediaFile{},
	}
	for _, format := range formats {
		mf, ok := r.MediaFormats[format]
		if !ok || mf.URL == "" {
			continue
		}
		if mf.Size > maxFileSize {
			f.Logger.Debug("skipping a large file", "id", r.ID, "format", format, "size", mf.Size)
			continue
		}
		u, err := url.Parse(mf.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
			continue
		}
		ext := strings.ToLower(path.Ext(u.Path))
		if ext == "" || len(ext) > 5 {
			ext = ".bin"
		}
		rel := path.Join("files", r.ID, format+ext)
		if err := f.fetchFile(ctx, mf.URL, filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			return nil, fmt.Errorf("%s: %w", format, err)
		}
		item.Media[format] = importer.MediaFile{File: rel, Dims: mf.Dims, Duration: mf.Duration}
	}
	if len(item.Media) == 0 {
		return nil, errors.New("no media in the requested formats")
	}
	return item, nil
}

func (f *Fetcher) fetchFile(ctx context.Context, src, dst string) error {
	if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return err
	}
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", src, resp.Status)
	}
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(resp.Body, maxFileSize+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxFileSize {
		err = errors.New("file too large")
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
