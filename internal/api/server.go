// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

// Package api serves the Tenor API v2 surface (search, featured, categories, search_suggestions,
// autocomplete, trending_terms, registershare and posts) over the index, plus the media files.
package api

import (
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/antimatterchat/antimatter-gifs/internal/importer"
	"github.com/antimatterchat/antimatter-gifs/internal/index"
)

// Config configures the server.
type Config struct {
	// PublicURL is the external URL of the service, e.g. https://gifs.example.com. When empty, it is
	// derived from each request.
	PublicURL string
	// MediaURL is the base URL of the media files, e.g. a CDN or object storage bucket mirroring
	// MediaDir. When empty, media are served by the service under PublicURL + "/media/".
	MediaURL string
	// MediaDir is the directory of the media files, served under /media/ when non-empty.
	MediaDir string
	// APIKeys are the accepted API keys; when empty, no key is required.
	APIKeys []string
	// CORSOrigins are the origins allowed to call the API from a browser ("*" for all).
	CORSOrigins []string
	// RateLimit is the sustained number of API requests per second allowed per client (API key and
	// IP address), RateBurst the size of bursts. A zero RateLimit disables rate limiting.
	RateLimit float64
	RateBurst int
	// TrustProxy uses the X-Forwarded-For/-Proto/-Host headers of a reverse proxy.
	TrustProxy bool
	// UploadKeys are the keys of the clients allowed to add GIFs and stickers with POST
	// /v2/upload (Authorization: Bearer <key>), which Importer converts; no uploads when empty.
	UploadKeys []string
	Importer   *importer.Importer
	// UploadMaxBytes is the largest uploaded file; DefaultUploadMaxBytes when zero.
	UploadMaxBytes int64
	Logger         *slog.Logger
	// Now returns the current time; time.Now when nil (tests set it).
	Now func() time.Time
}

// Server answers the API requests.
type Server struct {
	cfg     Config
	store   *index.Store
	log     *slog.Logger
	limiter *limiter
	keys    [][]byte

	uploadKeys [][]byte
	uploadMu   sync.Mutex
}

// New returns a server over the store.
func New(store *index.Store, cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.PublicURL = strings.TrimSuffix(cfg.PublicURL, "/")
	if cfg.MediaURL != "" && !strings.HasSuffix(cfg.MediaURL, "/") {
		cfg.MediaURL += "/"
	}
	s := &Server{cfg: cfg, store: store, log: cfg.Logger}
	for _, k := range cfg.APIKeys {
		if k = strings.TrimSpace(k); k != "" {
			s.keys = append(s.keys, []byte(k))
		}
	}
	for _, k := range cfg.UploadKeys {
		if k = strings.TrimSpace(k); k != "" {
			s.uploadKeys = append(s.uploadKeys, []byte(k))
		}
	}
	if cfg.RateLimit > 0 {
		burst := cfg.RateBurst
		if burst < 1 {
			burst = int(cfg.RateLimit*2) + 1
		}
		s.limiter = newLimiter(cfg.RateLimit, burst)
	}
	return s
}

// Handler returns the HTTP handler of the service.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	api := func(h http.HandlerFunc) http.Handler {
		return s.cors(s.rateLimit(s.requireKey(h)))
	}
	mux.Handle("/v2/search", api(s.handleSearch))
	mux.Handle("/v2/featured", api(s.handleFeatured))
	mux.Handle("/v2/categories", api(s.handleCategories))
	mux.Handle("/v2/search_suggestions", api(s.handleSuggestions))
	mux.Handle("/v2/autocomplete", api(s.handleAutocomplete))
	mux.Handle("/v2/trending_terms", api(s.handleTrendingTerms))
	mux.Handle("/v2/registershare", api(s.handleRegisterShare))
	mux.Handle("/v2/posts", api(s.handlePosts))
	// Not a Tenor endpoint: trusted clients (the Antimatter GIFs plugin) add GIFs and stickers.
	mux.Handle("POST /v2/upload", s.rateLimit(s.requireUploadKey(http.HandlerFunc(s.handleUpload))))
	mux.Handle("/v2/", s.cors(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Unknown endpoint.")
	})))
	if s.cfg.MediaDir != "" {
		mux.Handle("GET /media/", s.cors(http.StripPrefix("/media/", http.HandlerFunc(s.handleMedia))))
	}
	mux.Handle("GET /view/{id}", http.HandlerFunc(s.handleView))
	mux.HandleFunc("GET /healthz", s.handleHealth)
	return s.recoverer(s.accessLog(mux))
}

// params are the request parameters shared by the endpoints.
type params struct {
	limit       int
	offset      int
	kind        string
	filter      index.Filter
	mediaFilter map[string]bool
	random      bool
	locale      string
}

// errBadRequest is an invalid parameter; its message is shown to the client.
type errBadRequest struct{ msg string }

func (e errBadRequest) Error() string { return e.msg }

func (s *Server) parseParams(r *http.Request) (params, error) {
	q := r.URL.Query()
	p := params{limit: 20, kind: index.KindGIF, locale: "en"}

	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return p, errBadRequest{"Invalid value for limit: " + v}
		}
		p.limit = min(n, 50)
	}
	// pos is an opaque token for clients; here it's the offset. Unknown tokens start over.
	if v := q.Get("pos"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			p.offset = n
		}
	}
	if loc := q.Get("locale"); loc != "" {
		p.locale = strings.ToLower(strings.FieldsFunc(loc, func(r rune) bool { return r == '_' || r == '-' })[0])
	}

	for _, f := range splitList(q.Get("searchfilter")) {
		switch f {
		case "sticker":
			p.kind = index.KindSticker
		case "static":
			static := true
			p.filter.Static = &static
		case "-static":
			static := false
			p.filter.Static = &static
		}
	}
	p.filter.Kind = p.kind

	switch v := q.Get("contentfilter"); v {
	case "", "off":
	case "low":
		p.filter.Ratings = []string{"g", "pg", "pg13"}
	case "medium":
		p.filter.Ratings = []string{"g", "pg"}
	case "high":
		p.filter.Ratings = []string{"g"}
	default:
		return p, errBadRequest{"Invalid value for contentfilter: " + v}
	}

	switch v := q.Get("ar_range"); v {
	case "", "all":
	case "wide":
		p.filter.MinAspect, p.filter.MaxAspect = 0.42, 2.36
	case "standard":
		p.filter.MinAspect, p.filter.MaxAspect = 0.56, 1.78
	default:
		return p, errBadRequest{"Invalid value for ar_range: " + v}
	}

	if formats := splitList(q.Get("media_filter")); len(formats) > 0 {
		p.mediaFilter = map[string]bool{}
		for _, f := range formats {
			// The Tenor API v1 presets.
			switch f {
			case "minimal":
				f = "gif,tinygif,mp4"
			case "basic":
				f = "gif,tinygif,nanogif,mp4,tinymp4,nanomp4"
			}
			for _, name := range strings.Split(f, ",") {
				p.mediaFilter[name] = true
			}
		}
	}
	p.random = q.Get("random") == "true"
	return p, nil
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(strings.ToLower(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (s *Server) badRequest(w http.ResponseWriter, err error) {
	var bad errBadRequest
	if errors.As(err, &bad) {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", bad.msg)
		return
	}
	s.log.Error("request failed", "error", err)
	writeError(w, http.StatusInternalServerError, "INTERNAL", "Internal error.")
}

func (s *Server) writeResults(w http.ResponseWriter, r *http.Request, p params, posts []*index.Post, more bool) {
	results := s.resultObjects(r, posts, p.mediaFilter)
	if p.random {
		rand.Shuffle(len(results), func(i, j int) { results[i], results[j] = results[j], results[i] })
	}
	next := ""
	if more {
		next = strconv.Itoa(p.offset + len(posts))
	}
	writeJSON(w, http.StatusOK, resultsResponse{Results: results, Next: next})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	p, err := s.parseParams(r)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Missing required parameter q.")
		return
	}
	now := s.cfg.Now()
	posts, more, err := s.store.Search(r.Context(), query, p.filter, p.offset, p.limit, now)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	if p.offset == 0 && len(posts) > 0 {
		if err := s.store.RecordSearch(r.Context(), query, now); err != nil {
			s.log.Warn("counting a search failed", "error", err)
		}
	}
	s.writeResults(w, r, p, posts, more)
}

func (s *Server) handleFeatured(w http.ResponseWriter, r *http.Request) {
	p, err := s.parseParams(r)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	posts, more, err := s.store.Featured(r.Context(), p.filter, p.offset, p.limit, s.cfg.Now())
	if err != nil {
		s.badRequest(w, err)
		return
	}
	s.writeResults(w, r, p, posts, more)
}

func (s *Server) handlePosts(w http.ResponseWriter, r *http.Request) {
	p, err := s.parseParams(r)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	var ids []string
	for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Missing required parameter ids.")
		return
	}
	if len(ids) > 50 {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "At most 50 ids are allowed.")
		return
	}
	posts, err := s.store.GetPosts(r.Context(), ids)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, postsResponse{Results: s.resultObjects(r, posts, p.mediaFilter)})
}

// handleCategories lists the curated categories (type=featured, the default) or the categories
// made from the most shared terms of the week (type=trending). With searchfilter=sticker (an
// extension), the featured categories are the sticker packs.
func (s *Server) handleCategories(w http.ResponseWriter, r *http.Request) {
	p, err := s.parseParams(r)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	q := r.URL.Query()
	ctx := r.Context()
	now := s.cfg.Now()
	var tags []CategoryObject
	add := func(name, term string, post *index.Post) {
		path := "/v2/search?q=" + url.QueryEscape(term) + "&locale=" + url.QueryEscape(p.locale) + "&component=categories"
		if v := q.Get("contentfilter"); v != "" {
			path += "&contentfilter=" + url.QueryEscape(v)
		}
		if p.kind == index.KindSticker {
			path += "&searchfilter=sticker"
		}
		tags = append(tags, CategoryObject{SearchTerm: term, Path: path, Image: s.tileURL(r, post), Name: name})
	}

	switch typ := q.Get("type"); typ {
	case "", "featured":
		cats, err := s.store.Categories(ctx, p.kind)
		if err != nil {
			s.badRequest(w, err)
			return
		}
		ids := make([]string, len(cats))
		for i, c := range cats {
			ids[i] = c.ImagePostID
		}
		posts, err := s.store.GetPosts(ctx, ids)
		if err != nil {
			s.badRequest(w, err)
			return
		}
		byID := make(map[string]*index.Post, len(posts))
		for _, post := range posts {
			byID[post.ID] = post
		}
		for _, c := range cats {
			if post := byID[c.ImagePostID]; post != nil {
				add(c.Name, c.SearchTerm, post)
			}
		}
	case "trending":
		terms, err := s.store.TopSharedTerms(ctx, 20, now)
		if err != nil {
			s.badRequest(w, err)
			return
		}
		if len(terms) < 8 {
			more, err := s.store.TrendingTerms(ctx, 20, now)
			if err != nil {
				s.badRequest(w, err)
				return
			}
			terms = appendNew(terms, more...)
		}
		for _, term := range terms {
			posts, _, err := s.store.Search(ctx, term, p.filter, 0, 1, now)
			if err != nil {
				s.badRequest(w, err)
				return
			}
			if len(posts) > 0 {
				add("#"+term, term, posts[0])
			}
		}
	default:
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid value for type: "+typ)
		return
	}
	if tags == nil {
		tags = []CategoryObject{}
	}
	writeJSON(w, http.StatusOK, categoriesResponse{Locale: p.locale, Tags: tags})
}

func appendNew(list []string, values ...string) []string {
	seen := make(map[string]bool, len(list))
	for _, v := range list {
		seen[v] = true
	}
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			list = append(list, v)
		}
	}
	return list
}

func (s *Server) writeTerms(w http.ResponseWriter, p params, terms []string, err error) {
	if err != nil {
		s.badRequest(w, err)
		return
	}
	if terms == nil {
		terms = []string{}
	}
	writeJSON(w, http.StatusOK, termsResponse{Locale: p.locale, Results: terms})
}

func (s *Server) handleSuggestions(w http.ResponseWriter, r *http.Request) {
	p, err := s.parseParams(r)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	query := r.URL.Query().Get("q")
	if strings.TrimSpace(query) == "" {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Missing required parameter q.")
		return
	}
	terms, err := s.store.Suggestions(r.Context(), query, p.kind, p.limit, s.cfg.Now())
	s.writeTerms(w, p, terms, err)
}

func (s *Server) handleAutocomplete(w http.ResponseWriter, r *http.Request) {
	p, err := s.parseParams(r)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	query := r.URL.Query().Get("q")
	if strings.TrimSpace(query) == "" {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Missing required parameter q.")
		return
	}
	terms, err := s.store.Autocomplete(r.Context(), query, p.limit, s.cfg.Now())
	s.writeTerms(w, p, terms, err)
}

func (s *Server) handleTrendingTerms(w http.ResponseWriter, r *http.Request) {
	p, err := s.parseParams(r)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	terms, err := s.store.TrendingTerms(r.Context(), p.limit, s.cfg.Now())
	s.writeTerms(w, p, terms, err)
}

func (s *Server) handleRegisterShare(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			q = r.Form
		}
	}
	id := strings.TrimSpace(q.Get("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Missing required parameter id.")
		return
	}
	err := s.store.RecordShare(r.Context(), id, q.Get("q"), s.cfg.Now())
	if errors.Is(err, index.ErrNotFound) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Unknown id.")
		return
	}
	if err != nil {
		s.badRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	n, err := s.store.Ping(r.Context())
	if err != nil {
		s.log.Error("health check failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "posts": n})
}

// publicBase is the external URL of the service, without trailing slash.
func (s *Server) publicBase(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if s.cfg.TrustProxy {
		if v := firstHeaderValue(r, "X-Forwarded-Proto"); v == "http" || v == "https" {
			scheme = v
		}
		if v := firstHeaderValue(r, "X-Forwarded-Host"); v != "" {
			host = v
		}
	}
	return scheme + "://" + host
}

// mediaBase is the base URL of the media files, with a trailing slash.
func (s *Server) mediaBase(r *http.Request) string {
	if s.cfg.MediaURL != "" {
		return s.cfg.MediaURL
	}
	return s.publicBase(r) + "/media/"
}

func firstHeaderValue(r *http.Request, name string) string {
	v, _, _ := strings.Cut(r.Header.Get(name), ",")
	return strings.TrimSpace(v)
}
