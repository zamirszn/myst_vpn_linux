package main

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"
)

// TestIconPreview writes a contact sheet of every state so the shapes can be
// looked at rather than only asserted about:
//
//	MYST_ICON_PREVIEW=/tmp/icons.png go test -run TestIconPreview ./...
//
// Opt-in because it is a drawing tool, not an assertion — a test suite should
// not leave files behind as a side effect of running.
func TestIconPreview(t *testing.T) {
	out := os.Getenv("MYST_ICON_PREVIEW")
	if out == "" {
		t.Skip("set MYST_ICON_PREVIEW=<path.png> to render the icon sheet")
	}

	states := []struct {
		name      string
		state     string
		reachable bool
	}{
		{"connected", stateConnected, true},
		{"connecting", stateConnecting, true},
		{"disconnected", stateDisconnected, true},
		{"error", stateError, true},
		{"unreachable", stateDisconnected, false},
	}

	const scale = 5
	cell := 48 * scale
	canvas := image.NewRGBA(image.Rect(0, 0, cell*len(states), cell*2+22*scale))

	// Top row on light, bottom row on dark, so contrast is visible either way.
	fillRect(canvas, image.Rect(0, 0, canvas.Bounds().Dx(), cell), color.RGBA{0xd0, 0xd0, 0xd0, 0xff})
	fillRect(canvas, image.Rect(0, cell, canvas.Bounds().Dx(), canvas.Bounds().Dy()), color.RGBA{0x30, 0x30, 0x30, 0xff})

	for i, s := range states {
		st := styleFor(s.state, s.reachable)
		blit(canvas, render(st, 48), i*cell, 0, scale)
		blit(canvas, render(st, 22), i*cell, cell, scale)
	}

	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, canvas); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", out)
}

func fillRect(dst *image.RGBA, r image.Rectangle, c color.RGBA) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			dst.SetRGBA(x, y, c)
		}
	}
}

// blit draws a pixmap scaled up by an integer factor, nearest-neighbour,
// compositing over whatever is already there. Replacing the pixel outright
// would punch transparent holes into the background instead of letting it show
// through, which makes the preview lie about the icon.
func blit(dst *image.RGBA, p pixmap, ox, oy, scale int) {
	for y := 0; y < int(p.Height); y++ {
		for x := 0; x < int(p.Width); x++ {
			i := (y*int(p.Width) + x) * 4
			src := color.RGBA{p.Data[i+1], p.Data[i+2], p.Data[i+3], p.Data[i]}
			for dy := 0; dy < scale; dy++ {
				for dx := 0; dx < scale; dx++ {
					px, py := ox+x*scale+dx, oy+y*scale+dy
					dst.SetRGBA(px, py, over(src, dst.RGBAAt(px, py)))
				}
			}
		}
	}
}

// over alpha-composites src onto an opaque background.
func over(src, bg color.RGBA) color.RGBA {
	a := float64(src.A) / 255
	mix := func(s, b uint8) uint8 {
		return uint8(float64(s)*a + float64(b)*(1-a) + 0.5)
	}
	return color.RGBA{mix(src.R, bg.R), mix(src.G, bg.G), mix(src.B, bg.B), 0xff}
}
