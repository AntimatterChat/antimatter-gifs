// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/antimatterchat/antimatter-gifs/internal/index"
)

// The JSON shapes of the Tenor API v2 (https://developers.google.com/tenor/guides/response-objects-and-errors).

// ResultObject is a Tenor response object: one GIF or sticker.
type ResultObject struct {
	ID                 string                 `json:"id"`
	Title              string                 `json:"title"`
	MediaFormats       map[string]MediaObject `json:"media_formats"`
	Created            float64                `json:"created"`
	ContentDescription string                 `json:"content_description"`
	ItemURL            string                 `json:"itemurl"`
	URL                string                 `json:"url"`
	Tags               []string               `json:"tags"`
	Flags              []string               `json:"flags"`
	HasAudio           bool                   `json:"hasaudio"`
	HasCaption         bool                   `json:"hascaption"`
	BgColor            string                 `json:"bg_color,omitempty"`
	// Attribution is an extension: where third-party content comes from.
	Attribution *Attribution `json:"attribution,omitempty"`
}

// MediaObject is a Tenor media object: one file of a result.
type MediaObject struct {
	URL      string  `json:"url"`
	Dims     [2]int  `json:"dims"`
	Duration float64 `json:"duration"`
	Size     int64   `json:"size"`
}

// Attribution tells where a third-party GIF comes from.
type Attribution struct {
	Source   string `json:"source"`
	SourceID string `json:"source_id,omitempty"`
	URL      string `json:"url,omitempty"`
	Text     string `json:"text,omitempty"`
}

// CategoryObject is a Tenor category object.
type CategoryObject struct {
	SearchTerm string `json:"searchterm"`
	Path       string `json:"path"`
	Image      string `json:"image"`
	Name       string `json:"name"`
}

type resultsResponse struct {
	Results []ResultObject `json:"results"`
	Next    string         `json:"next"`
}

type postsResponse struct {
	Results []ResultObject `json:"results"`
}

type termsResponse struct {
	Locale  string   `json:"locale"`
	Results []string `json:"results"`
}

type categoriesResponse struct {
	Locale string           `json:"locale"`
	Tags   []CategoryObject `json:"tags"`
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// Media formats, from the Tenor documentation.
var (
	gifFormats = []string{"preview", "gif", "mediumgif", "tinygif", "nanogif", "mp4", "loopedmp4", "tinymp4",
		"nanomp4", "webm", "tinywebm", "nanowebm", "webp", "tinywebp", "nanowebp", "gifpreview", "tinygifpreview",
		"nanogifpreview"}
	stickerFormats = []string{"webp_transparent", "tinywebp_transparent", "nanowebp_transparent",
		"gif_transparent", "tinygif_transparent", "nanogif_transparent"}
	// KnownFormats are the format names accepted for media files.
	KnownFormats = func() map[string]bool {
		m := map[string]bool{}
		for _, f := range append(append([]string{}, gifFormats...), stickerFormats...) {
			m[f] = true
		}
		return m
	}()
)

// tileFormats are the formats used for category images, best first.
var tileFormats = map[string][]string{
	index.KindGIF:     {"tinygif", "gif", "mediumgif", "nanogif", "preview"},
	index.KindSticker: {"tinygif_transparent", "gif_transparent", "tinywebp_transparent", "webp_transparent", "preview"},
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeError answers in the Google API error format used by Tenor.
func writeError(w http.ResponseWriter, code int, status, message string) {
	writeJSON(w, code, errorResponse{Error: errorBody{Code: code, Message: message, Status: status}})
}

// resultObject converts a post, keeping the formats in filter (all when nil).
func (s *Server) resultObject(r *http.Request, p *index.Post, filter map[string]bool) ResultObject {
	base := s.publicBase(r)
	mediaBase := s.mediaBase(r)
	formats := make(map[string]MediaObject, len(p.Media))
	for name, m := range p.Media {
		if filter != nil && !filter[name] {
			continue
		}
		formats[name] = MediaObject{
			URL:      mediaBase + escapePath(m.Path),
			Dims:     [2]int{m.Width, m.Height},
			Duration: m.Duration,
			Size:     m.Size,
		}
	}
	flags := []string{}
	if p.Kind == index.KindSticker {
		flags = append(flags, "sticker")
	}
	if p.Static {
		flags = append(flags, "static")
	}
	tags := p.Tags
	if tags == nil {
		tags = []string{}
	}
	itemURL := base + "/view/" + url.PathEscape(p.ID)
	res := ResultObject{
		ID:                 p.ID,
		Title:              p.Title,
		MediaFormats:       formats,
		Created:            float64(p.Created.UnixNano()) / 1e9,
		ContentDescription: p.Description,
		ItemURL:            itemURL,
		URL:                itemURL,
		Tags:               tags,
		Flags:              flags,
		HasAudio:           p.HasAudio,
		BgColor:            p.BgColor,
	}
	if p.Source != "" {
		res.Attribution = &Attribution{Source: p.Source, SourceID: p.SourceID, URL: p.SourceURL, Text: p.Attribution}
	}
	return res
}

func (s *Server) resultObjects(r *http.Request, posts []*index.Post, filter map[string]bool) []ResultObject {
	out := make([]ResultObject, 0, len(posts))
	for _, p := range posts {
		out = append(out, s.resultObject(r, p, filter))
	}
	return out
}

// tileURL returns the URL of the media to show on a category tile.
func (s *Server) tileURL(r *http.Request, p *index.Post) string {
	for _, f := range tileFormats[p.Kind] {
		if m, ok := p.Media[f]; ok {
			return s.mediaBase(r) + escapePath(m.Path)
		}
	}
	names := make([]string, 0, len(p.Media))
	for name := range p.Media {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > 0 {
		return s.mediaBase(r) + escapePath(p.Media[names[0]].Path)
	}
	return ""
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
