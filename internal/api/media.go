// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

package api

import (
	"errors"
	"html/template"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/antimatterchat/antimatter-gifs/internal/index"
)

// mediaCSP keeps media files inert when opened directly (an SVG or HTML file can't run scripts).
const mediaCSP = "default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; sandbox"

// handleMedia serves a file of the media directory (the path is relative to it).
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	name := path.Clean("/" + r.URL.Path)
	if name == "/" || strings.Contains(name, "\x00") {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(s.cfg.MediaDir, filepath.FromSlash(name)))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "public, max-age=604800")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", mediaCSP)
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

var viewTemplate = template.Must(template.New("view").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: #1e1f22; color: #dbdee1; font: 15px/1.4 system-ui, sans-serif; }
main { max-width: min(560px, 100vw - 32px); text-align: center; }
img { max-width: 100%; border-radius: 10px; }
.tags { color: #949ba4; }
a { color: #8ab4f8; }
</style>
</head>
<body>
<main>
{{if .Image}}<img src="{{.Image}}" alt="{{.Title}}">{{end}}
<h1>{{.Title}}</h1>
{{if .Description}}<p>{{.Description}}</p>{{end}}
{{if .Tags}}<p class="tags">{{range .Tags}}#{{.}} {{end}}</p>{{end}}
{{if .Attribution}}<p>{{if .SourceURL}}<a href="{{.SourceURL}}" rel="noopener noreferrer">{{.Attribution}}</a>{{else}}{{.Attribution}}{{end}}</p>{{end}}
</main>
</body>
</html>
`))

var viewFormats = []string{"gif", "gif_transparent", "webp_transparent", "mediumgif", "tinygif", "tinygif_transparent", "preview"}

// handleView is the page of a post, the itemurl of the results.
func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	posts, err := s.store.GetPosts(r.Context(), []string{r.PathValue("id")})
	if err != nil {
		s.badRequest(w, err)
		return
	}
	if len(posts) == 0 {
		http.NotFound(w, r)
		return
	}
	p := posts[0]
	data := struct {
		Title, Description, Image, Attribution, SourceURL string
		Tags                                              []string
	}{Title: p.Title, Description: p.Description, Tags: p.Tags, Attribution: p.Attribution, SourceURL: p.SourceURL}
	if data.Title == "" {
		data.Title = map[string]string{index.KindGIF: "GIF", index.KindSticker: "Sticker"}[p.Kind]
	}
	if data.Attribution == "" && p.Source != "" {
		data.Attribution = "Source: " + p.Source
	}
	for _, f := range viewFormats {
		if m, ok := p.Media[f]; ok {
			data.Image = s.mediaBase(r) + escapePath(m.Path)
			break
		}
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; img-src * data:; style-src 'unsafe-inline'")
	h.Set("X-Content-Type-Options", "nosniff")
	if err := viewTemplate.Execute(w, data); err != nil && !errors.Is(err, http.ErrHandlerTimeout) {
		s.log.Warn("rendering a view page failed", "error", err)
	}
}
