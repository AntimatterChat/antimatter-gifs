// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

package media

import (
	"context"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"os"
	"path/filepath"
	"testing"

	"github.com/antimatterchat/antimatter-gifs/internal/index"
)

// writeGIF writes a 3 frames 300x200 GIF lasting 0.3 s.
func writeGIF(t *testing.T, file string) {
	t.Helper()
	pal := color.Palette{color.Transparent, color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}}
	anim := &gif.GIF{}
	for i := range 3 {
		img := image.NewPaletted(image.Rect(0, 0, 300, 200), pal)
		for x := i * 50; x < i*50+100; x++ {
			for y := 50; y < 150; y++ {
				img.SetColorIndex(x, y, uint8(1+i%2))
			}
		}
		anim.Image = append(anim.Image, img)
		anim.Delay = append(anim.Delay, 10)
	}
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := gif.EncodeAll(f, anim); err != nil {
		t.Fatal(err)
	}
}

func TestCopyWithoutTools(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.gif")
	writeGIF(t, src)
	out := filepath.Join(dir, "media", "12", "4512")
	media, err := Generate(context.Background(), Tools{}, src, index.KindGIF, out, "12/4512", Options{})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := media["gif"]
	if len(media) != 1 || !ok || m.Path != "12/4512/gif.gif" || m.Width != 300 || m.Height != 200 || m.Duration != 0.3 || m.Size == 0 {
		t.Fatalf("media: %+v", media)
	}
	if _, err := os.Stat(filepath.Join(out, "gif.gif")); err != nil {
		t.Fatal(err)
	}

	media, err = Generate(context.Background(), Tools{}, src, index.KindSticker, out, "12/4512", Options{})
	if err != nil || len(media) != 1 || media["gif_transparent"].Width != 300 {
		t.Fatalf("sticker media: %+v %v", media, err)
	}

	svg := filepath.Join(dir, "sticker.svg")
	os.WriteFile(svg, []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), 0o644)
	if _, err := Generate(context.Background(), Tools{}, svg, index.KindSticker, out, "x", Options{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("SVG without tools: %v", err)
	}
}

func TestGenerateWithTools(t *testing.T) {
	tools := DetectTools(context.Background())
	if tools.FFmpeg == "" || tools.FFprobe == "" {
		t.Skip("ffmpeg isn't installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "source.gif")
	writeGIF(t, src)
	media, err := Generate(context.Background(), tools, src, index.KindGIF, filepath.Join(dir, "gif"), "gif", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if m := media["tinygif"]; m.Width != 220 || m.Height != 147 || m.Path != "gif/tinygif.gif" {
		t.Fatalf("tinygif: %+v", m)
	}
	if m := media["nanogif"]; m.Height != 90 || m.Width != 135 {
		t.Fatalf("nanogif: %+v", m)
	}
	if m := media["preview"]; m.Width != 300 || m.Duration != 0 {
		t.Fatalf("preview: %+v", m)
	}
	if tools.has("libx264") {
		if m := media["tinymp4"]; m.Width != 300 || m.Height != 200 || m.Duration <= 0 {
			t.Fatalf("tinymp4: %+v", m)
		}
		if m := media["nanomp4"]; m.Width != 150 || m.Height != 100 {
			t.Fatalf("nanomp4: %+v", m)
		}
	}

	if tools.RSVG == "" {
		t.Skip("rsvg-convert isn't installed")
	}
	svg := filepath.Join(dir, "sticker.svg")
	os.WriteFile(svg, []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100" width="100" height="100">
  <g><animateTransform attributeName="transform" type="rotate" values="0 50 50;360 50 50" dur="0.5s" repeatCount="indefinite"/>
  <rect x="25" y="25" width="50" height="50" fill="#f00"/></g></svg>`), 0o644)
	media, err = Generate(context.Background(), tools, svg, index.KindSticker, filepath.Join(dir, "sticker"), "sticker", Options{StickerSize: 128, FPS: 10})
	if err != nil {
		t.Fatal(err)
	}
	if m := media["gif_transparent"]; m.Width != 128 || m.Height != 128 || m.Duration != 0.5 {
		t.Fatalf("gif_transparent: %+v", m)
	}
	if m := media["nanogif_transparent"]; m.Width != 90 {
		t.Fatalf("nanogif_transparent: %+v", m)
	}
	if _, ok := media["mp4"]; ok {
		t.Fatal("stickers have no mp4")
	}
	if tools.has("libwebp_anim") {
		if m := media["tinywebp_transparent"]; m.Width != 128 || m.Size == 0 {
			t.Fatalf("tinywebp_transparent: %+v", m)
		}
	}
}
