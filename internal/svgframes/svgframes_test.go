// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

package svgframes

import (
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const sample = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10">
  <g fill="red">
    <animateTransform attributeName="transform" type="translate" values="0 0;10 20;0 0" dur="2s" repeatCount="indefinite"/>
    <animateTransform attributeName="transform" type="rotate" values="0 5 5;90 5 5;0 5 5" dur="2s" repeatCount="indefinite" additive="sum" calcMode="spline" keySplines=".42 0 .58 1;.42 0 .58 1"/>
    <rect width="5" height="5"/>
  </g>
  <g><animate attributeName="opacity" values="1;0;1" keyTimes="0;.25;1" dur="2s" repeatCount="indefinite"/><circle r="1"/></g>
</svg>`

func TestFrames(t *testing.T) {
	a, err := Parse(sample)
	if err != nil {
		t.Fatal(err)
	}
	if a.Duration != 2 {
		t.Fatalf("duration %v", a.Duration)
	}
	f0 := a.Frame(0)
	if strings.Contains(f0, "<animate") {
		t.Fatalf("animation elements left: %s", f0)
	}
	if !strings.Contains(f0, `<g fill="red" transform="translate(0 0) rotate(0 5 5)">`) || !strings.Contains(f0, `<g opacity="1">`) {
		t.Fatalf("frame 0: %s", f0)
	}
	f1 := a.Frame(1)
	if !strings.Contains(f1, `transform="translate(10 20) rotate(90 5 5)"`) || !strings.Contains(f1, `opacity="0.333"`) {
		t.Fatalf("frame at 1s: %s", f1)
	}
	// Ease-in-out: a quarter of the way in time is less than a quarter of the way in value.
	f := a.Frame(0.25)
	if !strings.Contains(f, "translate(2.5 5) rotate(") || strings.Contains(f, "rotate(22.5 ") {
		t.Fatalf("frame at 0.25s: %s", f)
	}
	// Loops.
	if a.Frame(2.5) != a.Frame(0.5) {
		t.Fatal("frames don't loop")
	}
	if frames := a.Frames(10); len(frames) != 20 || frames[0] != f0 || frames[10] != f1 {
		t.Fatalf("got %d frames", len(frames))
	}
}

// Values may omit parameters: scale(s) is scale(s s), translate(x) translate(x 0) and rotate(a)
// rotate(a 0 0), also when interpolating with values that give them.
func TestShortValues(t *testing.T) {
	a, err := Parse(`<svg><g>` +
		`<animateTransform attributeName="transform" type="translate" values="4;0 8" dur="1s"/>` +
		`<animateTransform attributeName="transform" type="scale" values="1;0.9 1.1;1" dur="1s" additive="sum"/>` +
		`<animateTransform attributeName="transform" type="rotate" values="0;90 5 5" dur="1s" additive="sum"/>` +
		`<rect/></g></svg>`)
	if err != nil {
		t.Fatal(err)
	}
	if f := a.Frame(0); !strings.Contains(f, `transform="translate(4 0) scale(1 1) rotate(0 0 0)"`) {
		t.Fatalf("frame 0: %s", f)
	}
	if f := a.Frame(0.5); !strings.Contains(f, `transform="translate(2 4) scale(0.9 1.1) rotate(45 2.5 2.5)"`) {
		t.Fatalf("frame at 0.5s: %s", f)
	}
}

func TestStill(t *testing.T) {
	a, err := Parse(`<svg><g transform="scale(2)"><rect/></g></svg>`)
	if err != nil {
		t.Fatal(err)
	}
	if frames := a.Frames(20); len(frames) != 1 || frames[0] != `<svg><g transform="scale(2)"><rect/></g></svg>` {
		t.Fatalf("still frames: %v", frames)
	}
}

func TestInvalid(t *testing.T) {
	for name, svg := range map[string]string{
		"own transform": `<g transform="scale(2)"><animateTransform attributeName="transform" type="rotate" values="0;1" dur="1s"/></g>`,
		"not first":     `<g><rect/><animateTransform attributeName="transform" type="rotate" values="0;1" dur="1s"/></g>`,
		"from/to":       `<g><animateTransform attributeName="transform" type="rotate" from="0" to="1" dur="1s"/></g>`,
		"skewX":         `<g><animateTransform attributeName="transform" type="skewX" values="0;1" dur="1s"/></g>`,
		"fill":          `<g><animate attributeName="fill" values="red;blue" dur="1s"/></g>`,
		"keyTimes":      `<g><animate attributeName="opacity" values="0;1" keyTimes="0;.5;1" dur="1s"/></g>`,
		"keySplines":    `<g><animate attributeName="opacity" values="0;1;0" calcMode="spline" keySplines="0 0 1 1" dur="1s"/></g>`,
		"dur":           `<g><animate attributeName="opacity" values="0;1" dur="indefinite"/></g>`,
		"discrete":      `<g><animate attributeName="opacity" values="0;1" calcMode="discrete" dur="1s"/></g>`,
		"not closed":    `<g><animate attributeName="opacity" values="0;1" dur="1s"></animate></g>`,
		"rotate (a x)":  `<g><animateTransform attributeName="transform" type="rotate" values="0 5;90 5" dur="1s"/></g>`,
		"scale (3)":     `<g><animateTransform attributeName="transform" type="scale" values="1 1 1;2" dur="1s"/></g>`,
		"opacity (2)":   `<g><animate attributeName="opacity" values="0 1;1" dur="1s"/></g>`,
	} {
		if _, err := Parse(svg); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestCubicBezier(t *testing.T) {
	for _, x := range []float64{0, 0.3, 0.5, 1} {
		if y := cubicBezier(0, 0, 1, 1, x); math.Abs(y-x) > 1e-6 {
			t.Errorf("linear bezier at %v = %v", x, y)
		}
	}
	if y := cubicBezier(.45, 0, .55, 1, 0.5); math.Abs(y-0.5) > 1e-6 {
		t.Errorf("symmetric ease at 0.5 = %v", y)
	}
}

// A scale with a zero factor flattens what it draws.
var flattened = regexp.MustCompile(`scale\((0|-?[\d.]+ 0)\)`)

// The sticker pack of the repository must stay renderable, with nothing flattened to nothing.
func TestStickerPack(t *testing.T) {
	files, err := filepath.Glob("../../stickers/svg/*.svg")
	if err != nil || len(files) == 0 {
		t.Skip("no sticker pack")
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		a, err := Parse(string(data))
		if err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		if a.Duration < 0.5 || a.Duration > 5 {
			t.Errorf("%s: duration %v", file, a.Duration)
		}
		for _, frame := range a.Frames(20) {
			if strings.Contains(frame, "<animate") || strings.Contains(frame, "NaN") {
				t.Fatalf("%s: bad frame", file)
			}
			if m := flattened.FindString(frame); m != "" {
				t.Fatalf("%s: a frame has %s", file, m)
			}
		}
	}
}
