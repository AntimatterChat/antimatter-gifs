// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

package importer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/antimatterchat/antimatter-gifs/internal/index"
)

// stickerPack is the pack.json of a directory of SVG stickers (see stickers/ in the repository).
type stickerPack struct {
	License string `json:"license"`
	Author  string `json:"author"`
	// Attribution is the credit shown for the stickers; built from Author and License when empty.
	Attribution string `json:"attribution"`
	Packs       []struct {
		Slug     string `json:"slug"`
		Name     string `json:"name"`
		Stickers []struct {
			Slug        string   `json:"slug"`
			Label       string   `json:"label"`
			Title       string   `json:"title"`
			Description string   `json:"description"`
			Tags        []string `json:"tags"`
			Bg          string   `json:"bg"`
			File        string   `json:"file"`
		} `json:"stickers"`
	} `json:"packs"`
}

// StickerPackSource is the source of the stickers imported from a sticker pack.
const StickerPackSource = "stickerpack"

// StickerPackManifest builds the manifest of a sticker pack file: each pack becomes a sticker
// category, in order, and the featured stickers follow the order of the packs.
func StickerPackManifest(file string) (*Manifest, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var sp stickerPack
	if err := json.Unmarshal(data, &sp); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	attribution := sp.Attribution
	if attribution == "" && sp.Author != "" {
		attribution = strings.TrimSuffix("Stickers by "+sp.Author+", "+strings.ReplaceAll(sp.License, "-", " "), ", ")
	}
	m := &Manifest{Kind: index.KindSticker, Source: StickerPackSource, Attribution: attribution, Dir: filepath.Dir(file)}
	total := 0
	for _, p := range sp.Packs {
		total += len(p.Stickers)
	}
	n := 0
	for i, p := range sp.Packs {
		if p.Slug == "" || p.Name == "" {
			return nil, fmt.Errorf("%s: pack %d needs a slug and a name", file, i)
		}
		cat := Category{Kind: index.KindSticker, Name: p.Name, SearchTerm: strings.ToLower(p.Name), Position: i}
		for _, s := range p.Stickers {
			if s.Slug == "" || s.File == "" {
				return nil, fmt.Errorf("%s: a sticker of pack %s needs a slug and a file", file, p.Slug)
			}
			key := p.Slug + "/" + s.Slug
			title := s.Title
			if title == "" {
				title = s.Label
			}
			m.Items = append(m.Items, Item{
				Key: key, SourceID: key, Title: title, Description: s.Description,
				Tags: append(append([]string{}, s.Tags...), strings.ToLower(p.Name)), BgColor: s.Bg,
				File: s.File, Score: float64(total - n),
			})
			cat.Items = append(cat.Items, key)
			n++
		}
		m.Categories = append(m.Categories, cat)
	}
	return m, nil
}
