// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

package api

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/antimatterchat/antimatter-gifs/internal/importer"
	"github.com/antimatterchat/antimatter-gifs/internal/index"
	"github.com/antimatterchat/antimatter-gifs/internal/svgframes"
)

// DefaultUploadMaxBytes is the largest file POST /v2/upload accepts by default.
const DefaultUploadMaxBytes = 16 << 20

// UploadSource is the source of the posts added with POST /v2/upload.
const UploadSource = "upload"

// The file types uploads accept, by kind.
var uploadTypes = map[string][]string{
	index.KindGIF:     {".gif", ".png", ".webp", ".mp4", ".webm"},
	index.KindSticker: {".png", ".webp", ".gif", ".svg"},
}

var uploadRatings = []string{"g", "pg", "pg-13", "r"}

// requireUploadKey lets through the requests with one of the upload keys as a bearer token. The
// endpoint doesn't exist without upload keys.
func (s *Server) requireUploadKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.uploadKeys) == 0 || s.cfg.Importer == nil {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Uploads are turned off.")
			return
		}
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		valid := 0
		for _, k := range s.uploadKeys {
			valid |= subtle.ConstantTimeCompare(k, []byte(strings.TrimSpace(key)))
		}
		if !ok || valid != 1 {
			writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "The request is missing a valid upload key.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sniffType returns the extension of the file type of the first bytes of a file, from its
// content rather than its name: the tools converting it must not be handed something else, such
// as a playlist ffmpeg would follow.
func sniffType(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return ".gif"
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return ".png"
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return ".webp"
	case len(head) >= 12 && string(head[4:8]) == "ftyp":
		return ".mp4"
	case bytes.HasPrefix(head, []byte{0x1a, 0x45, 0xdf, 0xa3}):
		return ".webm"
	}
	text := bytes.TrimPrefix(head, []byte("\xef\xbb\xbf"))
	if utf8.Valid(text) || utf8.Valid(text[:max(0, len(text)-3)]) {
		if bytes.Contains(text, []byte("<svg")) {
			return ".svg"
		}
	}
	return ""
}

// checkSVG accepts the SVG stickers the renderer can draw safely: no document type (entities)
// and only the animations it supports.
func checkSVG(file string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if bytes.Contains(data, []byte("<!DOCTYPE")) || bytes.Contains(data, []byte("<!ENTITY")) {
		return errBadRequest{"SVG files with a document type aren't accepted."}
	}
	if _, err := svgframes.Parse(string(data)); err != nil {
		return errBadRequest{"Unsupported SVG animation: " + err.Error()}
	}
	return nil
}

// uploadTags reads the comma-separated tags of an upload.
func uploadTags(v string) ([]string, error) {
	var tags []string
	for _, t := range strings.Split(v, ",") {
		t = strings.ToLower(strings.Join(strings.Fields(t), " "))
		if t == "" || slices.Contains(tags, t) {
			continue
		}
		if utf8.RuneCountInString(t) > 40 {
			return nil, errBadRequest{"A tag can have up to 40 characters."}
		}
		tags = append(tags, t)
	}
	if len(tags) > 20 {
		return nil, errBadRequest{"An upload can have up to 20 tags."}
	}
	return tags, nil
}

// formText returns a form value without surrounding spaces, checking its length.
func formText(r *http.Request, name string, maxLen int) (string, error) {
	v := strings.TrimSpace(r.FormValue(name))
	if utf8.RuneCountInString(v) > maxLen {
		return "", errBadRequest{fmt.Sprintf("The %s can have up to %d characters.", name, maxLen)}
	}
	return v, nil
}

// handleUpload adds a GIF or sticker sent as multipart/form-data: file, kind (gif or sticker),
// title, and optionally tags (comma-separated), description, rating, attribution and, for
// stickers, the pack to add it to. It is converted to the Tenor formats like imported files, and
// the new post is returned like by /v2/posts. Uploading the same file again updates its post.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	maxBytes := s.cfg.UploadMaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultUploadMaxBytes
	}
	tooLarge := fmt.Sprintf("The file is larger than %d MB.", maxBytes>>20)
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+1<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "INVALID_ARGUMENT", tooLarge)
			return
		}
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Expected a multipart/form-data request.")
		return
	}
	defer r.MultipartForm.RemoveAll()

	post, err := s.upload(r, maxBytes)
	var bad errBadRequest
	switch {
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", bad.msg)
	case errors.Is(err, errTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "INVALID_ARGUMENT", tooLarge)
	case errors.Is(err, errUnconvertible):
		writeError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", "The file couldn't be converted: is it a valid image or video?")
	case err != nil:
		s.log.Error("upload failed", "error", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "Internal error.")
	default:
		writeJSON(w, http.StatusOK, postsResponse{Results: s.resultObjects(r, []*index.Post{post}, nil)})
	}
}

var (
	errTooLarge      = errors.New("file too large")
	errUnconvertible = errors.New("file can't be converted")
)

func (s *Server) upload(r *http.Request, maxBytes int64) (*index.Post, error) {
	kind := r.FormValue("kind")
	if kind == "" {
		kind = index.KindGIF
	}
	if kind != index.KindGIF && kind != index.KindSticker {
		return nil, errBadRequest{"Invalid value for kind: " + kind}
	}
	title, err := formText(r, "title", 100)
	if err != nil {
		return nil, err
	}
	if title == "" {
		return nil, errBadRequest{"Missing required parameter title."}
	}
	description, err := formText(r, "description", 300)
	if err != nil {
		return nil, err
	}
	attribution, err := formText(r, "attribution", 200)
	if err != nil {
		return nil, err
	}
	pack, err := formText(r, "pack", 60)
	if err != nil {
		return nil, err
	}
	if pack != "" && kind != index.KindSticker {
		return nil, errBadRequest{"Only stickers go in packs."}
	}
	rating := strings.ToLower(r.FormValue("rating"))
	if rating == "" {
		rating = "g"
	}
	if !slices.Contains(uploadRatings, rating) {
		return nil, errBadRequest{"Invalid value for rating: " + rating}
	}
	tags, err := uploadTags(r.FormValue("tags"))
	if err != nil {
		return nil, err
	}
	if pack != "" && !slices.Contains(tags, strings.ToLower(pack)) {
		tags = append(tags, strings.ToLower(pack))
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		return nil, errBadRequest{"Missing required file."}
	}
	defer file.Close()
	if header.Size > maxBytes {
		return nil, errTooLarge
	}
	head := make([]byte, 1024)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, errBadRequest{"The file is empty."}
	}
	head = head[:n]
	ext := sniffType(head)
	if !slices.Contains(uploadTypes[kind], ext) {
		return nil, errBadRequest{fmt.Sprintf("Unsupported file type: %ss can be %s files.", kind, strings.ToUpper(strings.ReplaceAll(strings.Join(uploadTypes[kind], ", "), ".", "")))}
	}

	dir, err := os.MkdirTemp("", "antimatter-gifs-upload-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "upload"+ext)
	out, err := os.Create(src)
	if err != nil {
		return nil, err
	}
	_, err = io.Copy(out, io.MultiReader(bytes.NewReader(head), file))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if ext == ".svg" {
		if err := checkSVG(src); err != nil {
			return nil, err
		}
	}

	// One conversion at a time: they are heavy.
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	stats, err := s.cfg.Importer.Import(r.Context(), &importer.Manifest{
		Kind:   kind,
		Source: UploadSource,
		Items: []importer.Item{{
			Title: title, Description: description, Tags: tags, Rating: rating, Attribution: attribution, File: src,
		}},
	})
	if err != nil {
		return nil, err
	}
	if len(stats.IDs) == 0 {
		return nil, errUnconvertible
	}
	id := stats.IDs[0]
	if pack != "" {
		if err := s.store.AddToCategory(r.Context(), index.Category{Kind: index.KindSticker, Name: pack, SearchTerm: pack}, []string{id}); err != nil {
			return nil, err
		}
	}
	posts, err := s.store.GetPosts(r.Context(), []string{id})
	if err != nil {
		return nil, err
	}
	if len(posts) == 0 {
		return nil, errUnconvertible
	}
	return posts[0], nil
}
