// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

// Package media stores the files of GIFs and stickers and generates their Tenor formats (tinygif,
// nanogif, mp4, webm, transparent GIF and WebP...) with ffmpeg, gifsicle and rsvg-convert when
// they are installed. Without them, only the formats of the source file are kept.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/gif"
	_ "image/jpeg" // probing still images
	_ "image/png"  // probing still images
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/antimatterchat/antimatter-gifs/internal/index"
	"github.com/antimatterchat/antimatter-gifs/internal/svgframes"
)

// Tools are the external programs used to generate formats; empty paths are missing tools.
type Tools struct {
	FFmpeg   string
	FFprobe  string
	Gifsicle string
	RSVG     string
	encoders map[string]bool
}

// DetectTools looks for ffmpeg, ffprobe, gifsicle and rsvg-convert in the PATH, and for the
// encoders of ffmpeg.
func DetectTools(ctx context.Context) Tools {
	var t Tools
	t.FFmpeg, _ = exec.LookPath("ffmpeg")
	t.FFprobe, _ = exec.LookPath("ffprobe")
	t.Gifsicle, _ = exec.LookPath("gifsicle")
	t.RSVG, _ = exec.LookPath("rsvg-convert")
	t.encoders = map[string]bool{}
	if t.FFmpeg != "" {
		out, err := exec.CommandContext(ctx, t.FFmpeg, "-hide_banner", "-encoders").Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if f := strings.Fields(line); len(f) >= 2 && len(f[0]) == 6 && f[0][0] == 'V' {
					t.encoders[f[1]] = true
				}
			}
		}
	}
	return t
}

// String describes the available tools.
func (t Tools) String() string {
	var have []string
	for name, p := range map[string]string{"ffmpeg": t.FFmpeg, "ffprobe": t.FFprobe, "gifsicle": t.Gifsicle, "rsvg-convert": t.RSVG} {
		if p != "" {
			have = append(have, name)
		}
	}
	if len(have) == 0 {
		return "none"
	}
	return strings.Join(have, ", ")
}

func (t Tools) has(encoder string) bool { return t.encoders[encoder] }

// Options tune the generation of formats.
type Options struct {
	// NoVariants only keeps the source file, in its own format.
	NoVariants bool
	// FPS is the frame rate of stickers rendered from SVG (20 when zero).
	FPS int
	// StickerSize is the size of the largest sticker formats (512 when zero).
	StickerSize int
	Logger      *slog.Logger
}

// ErrUnsupported tells that a file can't be used without a missing tool.
var ErrUnsupported = errors.New("unsupported file")

// formatExt is the file extension of each format.
func formatExt(format string) string {
	switch {
	case strings.Contains(format, "mp4"):
		return ".mp4"
	case strings.Contains(format, "webm"):
		return ".webm"
	case strings.Contains(format, "webp"):
		return ".webp"
	case strings.Contains(format, "preview"):
		return ".png"
	}
	return ".gif"
}

// variant is a format to generate: the media fits in a box (0 for no limit), and kind tells
// the encoder.
type variant struct {
	format string
	w, h   int
}

var gifVariants = []variant{
	{"tinygif", 220, 0}, {"nanogif", 0, 90},
	{"mp4", 640, 640}, {"tinymp4", 320, 320}, {"nanomp4", 150, 150},
	{"webm", 640, 640}, {"tinywebm", 320, 320}, {"nanowebm", 150, 150},
	{"preview", 640, 640},
}

var stickerVariants = []variant{
	{"tinygif_transparent", 220, 220}, {"nanogif_transparent", 90, 90},
	{"webp_transparent", 0, 0}, {"tinywebp_transparent", 220, 220}, {"nanowebp_transparent", 90, 90},
	{"preview", 0, 0},
}

// Generate stores the source file of a post in dir (the absolute directory of the post, whose
// path relative to the media root is rel) and generates its other formats. It returns the media
// per format.
func Generate(ctx context.Context, tools Tools, src, kind, dir, rel string, opts Options) (map[string]index.Media, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.FPS <= 0 {
		opts.FPS = 20
	}
	if opts.StickerSize <= 0 {
		opts.StickerSize = 512
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	g := &generator{ctx: ctx, tools: tools, dir: dir, rel: rel, opts: opts, media: map[string]index.Media{}}
	ext := strings.ToLower(filepath.Ext(src))

	input := []string{"-i", src}
	if ext == ".svg" {
		if tools.RSVG == "" || tools.FFmpeg == "" {
			return nil, fmt.Errorf("%w: rendering SVG stickers needs rsvg-convert and ffmpeg", ErrUnsupported)
		}
		frames, err := os.MkdirTemp("", "antimatter-gifs-frames-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(frames)
		if err := g.renderSVG(src, frames); err != nil {
			return nil, err
		}
		input = []string{"-framerate", strconv.Itoa(opts.FPS), "-i", filepath.Join(frames, "f%04d.png")}
	}

	main := "gif"
	if kind == index.KindSticker {
		main = "gif_transparent"
	}
	switch {
	case ext == ".gif":
		if err := g.copy(src, main); err != nil {
			return nil, err
		}
	case tools.FFmpeg == "" || opts.NoVariants && ext != ".svg":
		// Keep the source in the format it is in.
		format := map[string]string{".mp4": "mp4", ".webm": "webm", ".png": "preview", ".jpg": "preview", ".jpeg": "preview"}[ext]
		if ext == ".webp" && kind == index.KindSticker {
			format = "webp_transparent"
		}
		if format == "" {
			return nil, fmt.Errorf("%w: converting %s files needs ffmpeg", ErrUnsupported, ext)
		}
		return g.media, g.copy(src, format)
	default:
		box := 640
		if kind == index.KindSticker {
			box = opts.StickerSize
		}
		if err := g.ffmpeg(input, variant{main, box, box}, kind); err != nil {
			return nil, err
		}
	}
	if tools.FFmpeg == "" || opts.NoVariants {
		return g.media, nil
	}

	if kind == index.KindGIF && tools.Gifsicle != "" {
		// A lossy copy of the full size GIF.
		if err := g.run(tools.Gifsicle, "-O3", "--lossy=60", filepath.Join(dir, "gif.gif"), "-o", filepath.Join(dir, "mediumgif.gif")); err != nil {
			opts.Logger.Warn("gifsicle can't make the mediumgif format", "error", err)
		} else if err := g.record("mediumgif"); err != nil {
			return nil, err
		}
	}
	variants := gifVariants
	if kind == index.KindSticker {
		variants = stickerVariants
	}
	for _, v := range variants {
		if err := g.ffmpeg(input, v, kind); err != nil {
			if errors.Is(err, ErrUnsupported) {
				opts.Logger.Debug("skipping a format", "format", v.format, "reason", err)
				continue
			}
			return nil, err
		}
	}
	g.fillWebPDimensions()
	return g.media, nil
}

// fillWebPDimensions takes the dimensions and duration of animated WebP files, which ffprobe
// can't read, from the GIF of the same size.
func (g *generator) fillWebPDimensions() {
	for format, m := range g.media {
		if !strings.Contains(format, "webp") || m.Width != 0 {
			continue
		}
		if twin, ok := g.media[strings.Replace(format, "webp", "gif", 1)]; ok {
			m.Width, m.Height, m.Duration = twin.Width, twin.Height, twin.Duration
			g.media[format] = m
		}
	}
}

type generator struct {
	ctx   context.Context
	tools Tools
	dir   string
	rel   string
	opts  Options
	media map[string]index.Media
}

func (g *generator) run(name string, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(g.ctx, name, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = msg[len(msg)-500:]
		}
		return fmt.Errorf("%s: %w: %s", filepath.Base(name), err, msg)
	}
	return nil
}

func (g *generator) renderSVG(src, frames string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	anim, err := svgframes.Parse(string(data))
	if err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	size := strconv.Itoa(g.opts.StickerSize)
	for i, frame := range anim.Frames(g.opts.FPS) {
		svg := filepath.Join(frames, fmt.Sprintf("f%04d.svg", i))
		if err := os.WriteFile(svg, []byte(frame), 0o644); err != nil {
			return err
		}
		if err := g.run(g.tools.RSVG, "-w", size, "-h", size, "-o", strings.TrimSuffix(svg, ".svg")+".png", svg); err != nil {
			return err
		}
	}
	return nil
}

func (g *generator) copy(src, format string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	ext := formatExt(format)
	if format == "preview" {
		ext = strings.ToLower(filepath.Ext(src))
	}
	out, err := os.Create(filepath.Join(g.dir, format+ext))
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return g.recordFile(format, format+ext)
}

// ffmpeg writes one format.
func (g *generator) ffmpeg(input []string, v variant, kind string) error {
	ext := formatExt(v.format)
	out := filepath.Join(g.dir, v.format+ext)
	scale := "scale=w='min(iw," + boxSide(v.w) + ")':h='min(ih," + boxSide(v.h) + ")':force_original_aspect_ratio=decrease:flags=lanczos"
	args := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, input...)
	transparent := kind == index.KindSticker
	switch ext {
	case ".gif":
		palette := "palettegen=stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=4:diff_mode=rectangle"
		if transparent {
			palette = "palettegen=reserve_transparent=1[p];[b][p]paletteuse=alpha_threshold=128:dither=bayer:bayer_scale=4"
		}
		args = append(args, "-filter_complex", "[0:v]"+scale+",split[a][b];[a]"+palette, "-loop", "0")
		if transparent {
			// Frame differencing leaves trails with transparency.
			args = append(args, "-gifflags", "-offsetting-transdiff")
		}
	case ".mp4":
		if !g.tools.has("libx264") {
			return fmt.Errorf("%w: ffmpeg has no libx264", ErrUnsupported)
		}
		args = append(args, "-vf", scale+":force_divisible_by=2,format=yuv420p", "-c:v", "libx264", "-preset", "veryfast",
			"-crf", "23", "-movflags", "+faststart", "-an")
	case ".webm":
		switch {
		case g.tools.has("libvpx-vp9"):
			args = append(args, "-c:v", "libvpx-vp9", "-b:v", "0", "-crf", "40", "-row-mt", "1")
		case g.tools.has("libvpx"):
			args = append(args, "-c:v", "libvpx", "-b:v", "1M", "-crf", "30")
		default:
			return fmt.Errorf("%w: ffmpeg has no VP9 or VP8 encoder", ErrUnsupported)
		}
		args = append(args, "-vf", scale+":force_divisible_by=2,format=yuv420p", "-an")
	case ".webp":
		encoder := "libwebp_anim"
		if !g.tools.has(encoder) {
			return fmt.Errorf("%w: ffmpeg has no libwebp_anim", ErrUnsupported)
		}
		pixFmt := "yuv420p"
		if transparent {
			pixFmt = "yuva420p"
		}
		args = append(args, "-vf", scale+",format="+pixFmt, "-c:v", encoder, "-q:v", "75", "-loop", "0", "-an")
	case ".png":
		args = append(args, "-vf", scale, "-frames:v", "1", "-update", "1")
	}
	if err := g.run(g.tools.FFmpeg, append(args, out)...); err != nil {
		return err
	}
	if ext == ".gif" && g.tools.Gifsicle != "" {
		if err := g.run(g.tools.Gifsicle, "-O3", "--batch", out); err != nil {
			g.opts.Logger.Warn("gifsicle can't optimize a GIF", "file", out, "error", err)
		}
	}
	return g.record(v.format)
}

func boxSide(n int) string {
	if n <= 0 {
		return "100000"
	}
	return strconv.Itoa(n)
}

func (g *generator) record(format string) error {
	return g.recordFile(format, format+formatExt(format))
}

func (g *generator) recordFile(format, name string) error {
	m, err := Probe(g.ctx, g.tools, filepath.Join(g.dir, name))
	if err != nil {
		return fmt.Errorf("probing %s: %w", name, err)
	}
	m.Path = path.Join(g.rel, name)
	g.media[format] = m
	return nil
}

// Probe returns the dimensions, duration and size of a media file, with ffprobe when available
// and else with the Go image decoders (GIF, PNG and JPEG).
func Probe(ctx context.Context, tools Tools, file string) (index.Media, error) {
	var m index.Media
	info, err := os.Stat(file)
	if err != nil {
		return m, err
	}
	m.Size = info.Size()
	ext := strings.ToLower(filepath.Ext(file))
	if ext == ".gif" {
		return probeGIF(file, m)
	}
	if tools.FFprobe != "" {
		out, err := exec.CommandContext(ctx, tools.FFprobe, "-v", "error", "-select_streams", "v:0",
			"-show_entries", "stream=width,height:format=duration", "-of", "json", file).Output()
		if err == nil {
			var res struct {
				Streams []struct{ Width, Height int }
				Format  struct{ Duration string }
			}
			if json.Unmarshal(out, &res) == nil && len(res.Streams) > 0 {
				m.Width, m.Height = res.Streams[0].Width, res.Streams[0].Height
				m.Duration, _ = strconv.ParseFloat(res.Format.Duration, 64)
				if ext == ".png" || ext == ".jpg" || ext == ".jpeg" {
					m.Duration = 0
				}
				return m, nil
			}
		}
	}
	f, err := os.Open(file)
	if err != nil {
		return m, err
	}
	defer f.Close()
	if cfg, _, err := image.DecodeConfig(f); err == nil {
		m.Width, m.Height = cfg.Width, cfg.Height
	}
	return m, nil
}

func probeGIF(file string, m index.Media) (index.Media, error) {
	f, err := os.Open(file)
	if err != nil {
		return m, err
	}
	defer f.Close()
	g, err := gif.DecodeAll(f)
	if err != nil {
		return m, err
	}
	m.Width, m.Height = g.Config.Width, g.Config.Height
	if len(g.Image) > 1 {
		cs := 0
		for _, d := range g.Delay {
			cs += d
		}
		m.Duration = float64(cs) / 100
	}
	return m, nil
}
