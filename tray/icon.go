package main

import "math"

// pixmap is the (iiay) structure StatusNotifierItem carries icons in.
//
// The data is ARGB32 in network byte order — that is, A, R, G, B per pixel,
// top row first. Alpha is straight, not premultiplied: GdkPixbuf, which is what
// the GNOME appindicator extension loads these into, expects straight alpha.
// The difference only shows on the antialiased edge, but it is free to get right.
type pixmap struct {
	Width  int32
	Height int32
	Data   []byte
}

// rgb is a plain 8-bit colour.
type rgb struct{ R, G, B byte }

// Palette. Colour alone carries the state, with shape as a second cue for
// anyone who cannot rely on it: hollow means down, a keyhole means up, a slash
// means something is wrong.
var (
	colConnected   = rgb{0x2e, 0xc2, 0x7e} // green
	colConnecting  = rgb{0xe5, 0xa5, 0x0a} // amber
	colDisconnect  = rgb{0x9a, 0x99, 0x96} // grey
	colError       = rgb{0xe0, 0x1b, 0x24} // red
	colUnreachable = rgb{0x77, 0x76, 0x7b} // dim grey
)

// iconStyle is how one state is drawn.
type iconStyle struct {
	fill    rgb
	solid   bool // false draws a hollow outline
	keyhole bool
	slash   bool
}

// styleFor maps daemon state onto a drawing. unreachable means the daemon could
// not be contacted at all, which is distinct from a daemon that says "down":
// it is a filled, slashed shield rather than the empty outline of an idle VPN.
//
// It is filled rather than hollow on purpose. The slash is drawn by whitening
// the pixels the shield covers, so on a hollow outline it would only clip the
// ring and leave two disconnected white marks that read as a rendering fault.
func styleFor(state string, reachable bool) iconStyle {
	if !reachable {
		return iconStyle{fill: colUnreachable, solid: true, slash: true}
	}
	switch state {
	case stateConnected:
		return iconStyle{fill: colConnected, solid: true, keyhole: true}
	case stateConnecting:
		return iconStyle{fill: colConnecting, solid: true}
	case stateError:
		return iconStyle{fill: colError, solid: true, slash: true}
	default:
		return iconStyle{fill: colDisconnect, solid: false}
	}
}

// iconSizes are rendered together and offered as a set. Hosts pick whichever
// suits their panel; shipping only one size means an icon that is either blurry
// or downscaled depending on the desktop.
var iconSizes = []int{22, 24, 32, 48}

// renderPixmaps draws the style at every offered size.
func renderPixmaps(st iconStyle) []pixmap {
	out := make([]pixmap, 0, len(iconSizes))
	for _, size := range iconSizes {
		out = append(out, render(st, size))
	}
	return out
}

// samples is the supersampling factor per axis, so 4 means 16 samples a pixel.
const samples = 4

// render rasterises one icon.
func render(st iconStyle, size int) pixmap {
	data := make([]byte, size*size*4)
	total := float64(samples * samples)

	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var shieldHits, markHits int

			for sy := 0; sy < samples; sy++ {
				for sx := 0; sx < samples; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/samples) / float64(size)
					y := (float64(py) + (float64(sy)+0.5)/samples) / float64(size)

					if !inShield(x, y, st.solid) {
						continue
					}
					shieldHits++
					if (st.keyhole && inKeyhole(x, y)) || (st.slash && inSlash(x, y)) {
						markHits++
					}
				}
			}

			if shieldHits == 0 {
				continue // fully transparent; the buffer is already zeroed
			}

			// The mark is drawn in white over the fill, in proportion to how
			// much of this pixel's covered area it occupies.
			var markFrac float64
			if shieldHits > 0 {
				markFrac = float64(markHits) / float64(shieldHits)
			}
			c := blend(st.fill, rgb{0xff, 0xff, 0xff}, markFrac)

			i := (py*size + px) * 4
			data[i] = byte(255 * float64(shieldHits) / total)
			data[i+1] = c.R
			data[i+2] = c.G
			data[i+3] = c.B
		}
	}

	return pixmap{Width: int32(size), Height: int32(size), Data: data}
}

// blend mixes two colours, t=0 giving a and t=1 giving b.
func blend(a, b rgb, t float64) rgb {
	lerp := func(x, y byte) byte {
		return byte(float64(x) + (float64(y)-float64(x))*t + 0.5)
	}
	return rgb{lerp(a.R, b.R), lerp(a.G, b.G), lerp(a.B, b.B)}
}

// inShield reports whether a point falls on the shield. solid fills it; the
// hollow form is the same shape with the middle taken out.
func inShield(x, y float64, solid bool) bool {
	if !shieldShape(x, y, 0) {
		return false
	}
	if solid {
		return true
	}
	return !shieldShape(x, y, 0.40)
}

// shieldShape is the shield outline, optionally shrunk toward its centre by
// inset. Scaling the point away from the centre is the same as shrinking the
// shape toward it, which is cheaper than rebuilding the geometry.
func shieldShape(x, y, inset float64) bool {
	const cx, cy = 0.5, 0.5
	x = cx + (x-cx)*(1+inset)
	y = cy + (y-cy)*(1+inset)

	const (
		halfW    = 0.40 // half width at the shoulders
		top      = 0.06 // top edge
		shoulder = 0.52 // where the taper begins
		tip      = 0.95 // the point
		radius   = 0.12 // top corner rounding
	)

	if y < top || y > tip {
		return false
	}
	dx := math.Abs(x - cx)
	if dx > halfW {
		return false
	}

	if y <= shoulder {
		// Below the corners this is a plain rectangle; above them the corner
		// circle decides.
		if dy := top + radius - y; dy > 0 && dx > halfW-radius {
			ex := dx - (halfW - radius)
			return ex*ex+dy*dy <= radius*radius
		}
		return true
	}

	// An elliptical taper to the point: full width at the shoulder, zero at the
	// tip, and continuous with the rectangle above it.
	t := (y - shoulder) / (tip - shoulder)
	return dx <= halfW*math.Sqrt(1-t*t)
}

// inKeyhole marks a keyhole — the "this is locked" cue on a live tunnel.
func inKeyhole(x, y float64) bool {
	dx, dy := x-0.5, y-0.40
	if dx*dx+dy*dy <= 0.075*0.075 {
		return true
	}
	if y < 0.40 || y > 0.66 {
		return false
	}
	t := (y - 0.40) / 0.26
	return math.Abs(x-0.5) <= 0.05*(1-0.5*t)
}

// inSlash marks a diagonal bar, drawn when the daemon reports a failure.
func inSlash(x, y float64) bool {
	// Perpendicular distance to the line x + y = 1.
	d := math.Abs((x-0.5)+(y-0.5)) / math.Sqrt2
	return d < 0.055
}
