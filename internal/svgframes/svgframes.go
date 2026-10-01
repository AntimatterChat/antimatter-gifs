// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE for license information.

// Package svgframes turns an animated SVG into still frames, so that SVG stickers can be rendered
// to animated GIF and WebP files with tools that only draw still SVGs (rsvg-convert).
//
// It supports the subset of SMIL used by the sticker pack: <animateTransform> (translate, scale,
// rotate) and <animate attributeName="opacity">, self-closing, placed as the first children of a
// <g> that has no transform or opacity of its own, with values, keyTimes, calcMode linear or spline
// (keySplines), additive and a dur; animations repeat indefinitely from time 0.
package svgframes

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var (
	// A <g> opening tag followed by its animation elements.
	animatedGroup = regexp.MustCompile(`(<g\b[^>]*?)(\s*/?>)((?:\s*<(?:animateTransform|animate)\b[^>]*/>)+)`)
	animation     = regexp.MustCompile(`<(animateTransform|animate)\b([^>]*)/>`)
	attribute     = regexp.MustCompile(`([\w:-]+)\s*=\s*"([^"]*)"`)
	ownAttribute  = regexp.MustCompile(`\s(transform|opacity)\s*=`)
)

// anim is one animation element.
type anim struct {
	transformType string // translate, scale or rotate; empty for opacity
	values        [][]float64
	keyTimes      []float64
	splines       [][4]float64
	additive      bool
	dur           float64
}

type group struct {
	start, end int // of the whole match in the source
	openTag    string
	closeTag   string
	anims      []anim
}

// Animation is a parsed animated SVG.
type Animation struct {
	src    string
	groups []group
	// Duration is one loop of the animation in seconds; 0 for a still SVG.
	Duration float64
}

// Parse finds the animations of an SVG document.
func Parse(svg string) (*Animation, error) {
	a := &Animation{src: svg}
	for _, m := range animatedGroup.FindAllStringSubmatchIndex(svg, -1) {
		g := group{start: m[0], end: m[1], openTag: svg[m[2]:m[3]], closeTag: svg[m[4]:m[5]]}
		if strings.HasSuffix(strings.TrimSpace(g.closeTag), "/>") {
			return nil, fmt.Errorf("an empty <g/> can't hold animations")
		}
		if ownAttribute.MatchString(g.openTag) {
			return nil, fmt.Errorf("an animated <g> must not have a transform or opacity of its own: %s", g.openTag)
		}
		for _, am := range animation.FindAllStringSubmatch(svg[m[6]:m[7]], -1) {
			an, err := parseAnim(am[1], am[2])
			if err != nil {
				return nil, err
			}
			a.Duration = math.Max(a.Duration, an.dur)
			g.anims = append(g.anims, an)
		}
		a.groups = append(a.groups, g)
	}
	found := 0
	for _, g := range a.groups {
		found += len(g.anims)
	}
	if all := len(animation.FindAllStringIndex(svg, -1)); all != found || strings.Count(svg, "<animate") != all {
		return nil, fmt.Errorf("animation elements must be self-closing and the first children of a <g>")
	}
	return a, nil
}

func parseAnim(element, attrs string) (anim, error) {
	at := map[string]string{}
	for _, m := range attribute.FindAllStringSubmatch(attrs, -1) {
		at[m[1]] = m[2]
	}
	var an anim
	switch element {
	case "animateTransform":
		if at["attributeName"] != "transform" {
			return an, fmt.Errorf("animateTransform must animate transform, not %q", at["attributeName"])
		}
		an.transformType = at["type"]
		if an.transformType == "" {
			an.transformType = "translate"
		}
		if an.transformType != "translate" && an.transformType != "scale" && an.transformType != "rotate" {
			return an, fmt.Errorf("unsupported animateTransform type %q", an.transformType)
		}
	case "animate":
		if at["attributeName"] != "opacity" {
			return an, fmt.Errorf("only opacity can be animated with <animate>, not %q", at["attributeName"])
		}
	}
	for _, unsupported := range []string{"from", "to", "by", "begin"} {
		if _, ok := at[unsupported]; ok {
			return an, fmt.Errorf("the %s attribute isn't supported, use values", unsupported)
		}
	}
	var err error
	if an.dur, err = parseDuration(at["dur"]); err != nil {
		return an, err
	}
	for _, v := range splitList(at["values"]) {
		nums, err := parseNumbers(v)
		if err != nil || len(nums) == 0 {
			return an, fmt.Errorf("invalid value %q", v)
		}
		if nums, err = fillTransform(an.transformType, nums); err != nil {
			return an, fmt.Errorf("invalid value %q: %w", v, err)
		}
		an.values = append(an.values, nums)
	}
	if len(an.values) == 0 {
		return an, fmt.Errorf("an animation needs values")
	}
	if kt := splitList(at["keyTimes"]); len(kt) > 0 {
		if len(kt) != len(an.values) {
			return an, fmt.Errorf("keyTimes %q don't match the %d values", at["keyTimes"], len(an.values))
		}
		for _, k := range kt {
			f, err := strconv.ParseFloat(k, 64)
			if err != nil {
				return an, fmt.Errorf("invalid keyTimes %q", at["keyTimes"])
			}
			an.keyTimes = append(an.keyTimes, f)
		}
	} else {
		n := len(an.values)
		for i := range n {
			if n == 1 {
				an.keyTimes = append(an.keyTimes, 0)
			} else {
				an.keyTimes = append(an.keyTimes, float64(i)/float64(n-1))
			}
		}
	}
	switch at["calcMode"] {
	case "", "linear":
	case "spline":
		for _, s := range splitList(at["keySplines"]) {
			nums, err := parseNumbers(s)
			if err != nil || len(nums) != 4 {
				return an, fmt.Errorf("invalid keySplines %q", at["keySplines"])
			}
			an.splines = append(an.splines, [4]float64{nums[0], nums[1], nums[2], nums[3]})
		}
		if len(an.splines) != len(an.values)-1 {
			return an, fmt.Errorf("keySplines %q don't match the %d values", at["keySplines"], len(an.values))
		}
	default:
		return an, fmt.Errorf("unsupported calcMode %q", at["calcMode"])
	}
	an.additive = at["additive"] == "sum"
	return an, nil
}

// fillTransform completes the omitted parameters of a transform value as SVG reads them, so that
// values of different lengths interpolate like in browsers: scale(s) is scale(s s), translate(x)
// is translate(x 0) and rotate(a) is rotate(a 0 0). Otherwise "1;0.9 1.1" would scale y from 0,
// flattening the first frame.
func fillTransform(transformType string, v []float64) ([]float64, error) {
	switch transformType {
	case "":
		if len(v) != 1 {
			return nil, fmt.Errorf("an opacity is one number")
		}
		return v, nil
	case "scale":
		switch len(v) {
		case 1:
			return []float64{v[0], v[0]}, nil
		case 2:
			return v, nil
		}
		return nil, fmt.Errorf("scale takes one or two numbers")
	case "rotate":
		switch len(v) {
		case 1:
			return []float64{v[0], 0, 0}, nil
		case 3:
			return v, nil
		}
		return nil, fmt.Errorf("rotate takes one or three numbers")
	default:
		switch len(v) {
		case 1:
			return []float64{v[0], 0}, nil
		case 2:
			return v, nil
		}
		return nil, fmt.Errorf("translate takes one or two numbers")
	}
}

func parseDuration(v string) (float64, error) {
	v = strings.TrimSpace(v)
	scale := 1.0
	switch {
	case strings.HasSuffix(v, "ms"):
		v, scale = strings.TrimSuffix(v, "ms"), 0.001
	case strings.HasSuffix(v, "s"):
		v = strings.TrimSuffix(v, "s")
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return 0, fmt.Errorf("invalid dur %q", v)
	}
	return f * scale, nil
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ";") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func parseNumbers(v string) ([]float64, error) {
	fields := strings.FieldsFunc(v, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' || r == '\n' })
	nums := make([]float64, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.ParseFloat(f, 64)
		if err != nil {
			return nil, err
		}
		nums = append(nums, n)
	}
	return nums, nil
}

// value interpolates the animation at time t (seconds).
func (an anim) value(t float64) []float64 {
	if len(an.values) == 1 {
		return an.values[0]
	}
	p := math.Mod(t, an.dur) / an.dur
	last := len(an.values) - 1
	for i := 0; i < last; i++ {
		k0, k1 := an.keyTimes[i], an.keyTimes[i+1]
		if p < k0 || p > k1 && i+1 < last {
			continue
		}
		if k1 <= k0 {
			return an.values[i]
		}
		u := math.Min(1, (p-k0)/(k1-k0))
		if an.splines != nil {
			s := an.splines[i]
			u = cubicBezier(s[0], s[1], s[2], s[3], u)
		}
		a, b := an.values[i], an.values[i+1]
		out := make([]float64, max(len(a), len(b)))
		for j := range out {
			out[j] = at(a, j) + (at(b, j)-at(a, j))*u
		}
		return out
	}
	return an.values[last]
}

func at(v []float64, i int) float64 {
	if i < len(v) {
		return v[i]
	}
	return 0
}

// cubicBezier is the CSS/SMIL timing function through (0,0), (x1,y1), (x2,y2), (1,1) at x.
func cubicBezier(x1, y1, x2, y2, x float64) float64 {
	bez := func(a, b, s float64) float64 {
		return 3*a*s*(1-s)*(1-s) + 3*b*s*s*(1-s) + s*s*s
	}
	lo, hi := 0.0, 1.0
	s := x
	for range 50 {
		if bez(x1, x2, s) < x {
			lo = s
		} else {
			hi = s
		}
		s = (lo + hi) / 2
	}
	return bez(y1, y2, s)
}

func fmtNum(f float64) string {
	return strconv.FormatFloat(math.Round(f*1000)/1000, 'f', -1, 64)
}

func (an anim) transform(t float64) string {
	v := an.value(t)
	switch an.transformType {
	case "scale":
		sx := v[0]
		sy := sx
		if len(v) > 1 {
			sy = v[1]
		}
		return "scale(" + fmtNum(sx) + " " + fmtNum(sy) + ")"
	case "rotate":
		if len(v) >= 3 {
			return "rotate(" + fmtNum(v[0]) + " " + fmtNum(v[1]) + " " + fmtNum(v[2]) + ")"
		}
		return "rotate(" + fmtNum(v[0]) + ")"
	default:
		return "translate(" + fmtNum(v[0]) + " " + fmtNum(at(v, 1)) + ")"
	}
}

// Frame returns the SVG document frozen at time t (seconds): each animated group gets the
// transform and opacity of that time, and the animation elements are removed.
func (a *Animation) Frame(t float64) string {
	var b strings.Builder
	prev := 0
	for _, g := range a.groups {
		b.WriteString(a.src[prev:g.start])
		var transforms []string
		opacity := -1.0
		for _, an := range g.anims {
			if an.transformType == "" {
				opacity = an.value(t)[0]
				continue
			}
			if !an.additive {
				transforms = transforms[:0]
			}
			transforms = append(transforms, an.transform(t))
		}
		b.WriteString(g.openTag)
		if len(transforms) > 0 {
			b.WriteString(` transform="` + strings.Join(transforms, " ") + `"`)
		}
		if opacity >= 0 {
			b.WriteString(` opacity="` + fmtNum(math.Max(0, math.Min(1, opacity))) + `"`)
		}
		b.WriteString(g.closeTag)
		prev = g.end
	}
	b.WriteString(a.src[prev:])
	return b.String()
}

// Frames returns the frames of one loop at fps frames per second (a single frame for a still SVG).
func (a *Animation) Frames(fps int) []string {
	if a.Duration == 0 || fps <= 0 {
		return []string{a.Frame(0)}
	}
	n := max(1, int(math.Round(a.Duration*float64(fps))))
	frames := make([]string, n)
	for i := range n {
		frames[i] = a.Frame(float64(i) * a.Duration / float64(n))
	}
	return frames
}
