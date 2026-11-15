package theme

import (
	"math"

	"github.com/egoist/mygo/ui"
)

// GPUI's two color types, carried across with GPUI's own arithmetic
// (zed crates/gpui/src/color.rs at the pinned revision), so a token's
// Hsla is the very value the Rust build held, and a blend lands on the
// same tone.
//
// Each sum of products is rounded to float32 at every step, as Rust's f32
// arithmetic is: Go may fuse `a*b + c` into one instruction on arm64,
// which rounds once, so the products are converted explicitly.

// Rgba is an RGBA color, every channel 0..1 (GPUI's `Rgba`).
type Rgba struct {
	R, G, B, A float32
}

// Hsla is an HSLA color, every component 0..1 (GPUI's `Hsla`).
type Hsla struct {
	H, S, L, A float32
}

// rgb converts an RGB hex color code number (GPUI's `rgb`).
func rgb(hex uint32) Rgba {
	return Rgba{
		R: float32(uint8(hex>>16)) / 255.0,
		G: float32(uint8(hex>>8)) / 255.0,
		B: float32(uint8(hex)) / 255.0,
		A: 1.0,
	}
}

// rgba converts an RGBA hex color code number (GPUI's `rgba`).
func rgba(hex uint32) Rgba {
	return Rgba{
		R: float32(uint8(hex>>24)) / 255.0,
		G: float32(uint8(hex>>16)) / 255.0,
		B: float32(uint8(hex>>8)) / 255.0,
		A: float32(uint8(hex)) / 255.0,
	}
}

// Blend blends other over c (GPUI's `Rgba::blend`).
func (c Rgba) Blend(other Rgba) Rgba {
	if other.A >= 1.0 {
		return other
	} else if other.A <= 0.0 {
		return c
	}
	return Rgba{
		R: float32(c.R*(1.0-other.A)) + float32(other.R*other.A),
		G: float32(c.G*(1.0-other.A)) + float32(other.G*other.A),
		B: float32(c.B*(1.0-other.A)) + float32(other.B*other.A),
		A: c.A,
	}
}

// ToHsla converts (GPUI's `impl From<Rgba> for Hsla`).
func (c Rgba) ToHsla() Hsla {
	r, g, b := c.R, c.G, c.B
	hi := max(r, max(g, b))
	lo := min(r, min(g, b))
	delta := hi - lo

	l := (hi + lo) / 2.0
	var s float32
	switch {
	case l == 0.0 || l == 1.0:
		s = 0.0
	case l < 0.5:
		s = delta / float32(2.0*l)
	default:
		s = delta / (2.0 - float32(2.0*l))
	}

	var h float32
	switch {
	case delta == 0.0:
		h = 0.0
	case hi == r:
		h = remEuclid((g-b)/delta, 6.0) / 6.0
	case hi == g:
		h = ((b-r)/delta + 2.0) / 6.0
	default:
		h = ((r-g)/delta + 4.0) / 6.0
	}
	return Hsla{H: h, S: s, L: l, A: c.A}
}

// remEuclid is Rust's f32::rem_euclid: the remainder, never negative.
func remEuclid(x, m float32) float32 {
	r := float32(math.Mod(float64(x), float64(m)))
	if r < 0 {
		r += float32(math.Abs(float64(m)))
	}
	return r
}

// ToRgba converts (GPUI's `impl From<Hsla> for Rgba`).
func (c Hsla) ToRgba() Rgba {
	h, s, l := c.H, c.S, c.L

	cc := (1.0 - float32(math.Abs(float64(float32(2.0*l)-1.0)))) * s
	x := cc * (1.0 - float32(math.Abs(float64(remF32(float32(h*6.0), 2.0)-1.0))))
	m := l - cc/2.0
	cm := cc + m
	xm := x + m

	var r, g, b float32
	switch int32(math.Floor(float64(float32(h * 6.0)))) {
	case 0, 6:
		r, g, b = cm, xm, m
	case 1:
		r, g, b = xm, cm, m
	case 2:
		r, g, b = m, cm, xm
	case 3:
		r, g, b = m, xm, cm
	case 4:
		r, g, b = xm, m, cm
	default:
		r, g, b = cm, m, xm
	}
	return Rgba{R: clamp01(r), G: clamp01(g), B: clamp01(b), A: c.A}
}

// remF32 is Rust's `%` on f32: the sign follows the dividend.
func remF32(x, m float32) float32 { return float32(math.Mod(float64(x), float64(m))) }

func clamp01(v float32) float32 { return min(max(v, 0), 1) }

// Blend blends other over c (GPUI's `Hsla::blend`): other itself when
// opaque, c when other is transparent, else a mix in RGB weighted by
// other's alpha, keeping c's alpha.
func (c Hsla) Blend(other Hsla) Hsla {
	alpha := other.A
	if alpha >= 1.0 {
		return other
	} else if alpha <= 0.0 {
		return c
	}
	return c.ToRgba().Blend(other.ToRgba()).ToHsla()
}

// Grayscale drops the saturation (GPUI's `Hsla::grayscale`).
func (c Hsla) Grayscale() Hsla { return Hsla{H: c.H, S: 0, L: c.L, A: c.A} }

// Opacity multiplies the alpha by factor, clamped to 0..1 (GPUI's
// `Hsla::opacity`).
func (c Hsla) Opacity(factor float32) Hsla {
	return Hsla{H: c.H, S: c.S, L: c.L, A: c.A * clamp01(factor)}
}

// Alpha replaces the alpha, clamped to 0..1 (GPUI's `Hsla::alpha`).
func (c Hsla) Alpha(a float32) Hsla { return Hsla{H: c.H, S: c.S, L: c.L, A: clamp01(a)} }

// Color is c as mygo paints it: straight-alpha sRGB, each channel rounded
// to the nearest of 256 levels.
func (c Hsla) Color() ui.Color {
	r := c.ToRgba()
	return ui.RGBA(level(r.R), level(r.G), level(r.B), r.A)
}

func level(v float32) uint8 { return uint8(math.Round(float64(clamp01(v)) * 255)) }
