// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

package index

// Formats are the media formats of the Tenor API v2
// (https://developers.google.com/tenor/guides/response-objects-and-errors#content-formats).
var Formats = []string{
	"preview", "gif", "mediumgif", "tinygif", "nanogif",
	"mp4", "loopedmp4", "tinymp4", "nanomp4",
	"webm", "tinywebm", "nanowebm",
	"webp", "tinywebp", "nanowebp",
	"gifpreview", "tinygifpreview", "nanogifpreview",
	"webp_transparent", "tinywebp_transparent", "nanowebp_transparent",
	"gif_transparent", "tinygif_transparent", "nanogif_transparent",
}

// KnownFormat tells whether a format is one of Formats.
func KnownFormat(name string) bool {
	for _, f := range Formats {
		if f == name {
			return true
		}
	}
	return false
}
