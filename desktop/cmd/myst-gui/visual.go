package main

import (
	"math"
	"time"

	"github.com/diamondburned/gotk4/pkg/cairo"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// statusVisual is the dark, map-like panel behind the connection card. The
// official app puts a world map there; this draws a stylised network instead,
// which costs no map data and still reads as "somewhere out there" at a glance.
//
// The colour of the routes follows the tunnel state: green when traffic is
// flowing through it, amber while it is being built, red after a failure, and
// a quiet lavender when there is nothing to route.
type statusVisual struct {
	area  *gtk.DrawingArea
	state string
	phase float64
	last  time.Time
}

func newStatusVisual() *statusVisual {
	v := &statusVisual{state: "disconnected"}
	v.area = gtk.NewDrawingArea()
	v.area.SetContentHeight(316)
	v.area.SetHExpand(true)
	v.area.SetVExpand(true)
	v.area.AddCSSClass("map-panel")
	v.area.SetDrawFunc(v.draw)
	return v
}

// setState changes the route colour and, for the states that are still moving,
// starts the pulse.
func (v *statusVisual) setState(state string) {
	if v.state == state {
		return
	}
	v.state = state
	v.area.QueueDraw()
}

// tick advances the pulse while a tunnel is being built. The window calls it
// from the same timer that polls the daemon.
func (v *statusVisual) tick() {
	switch v.state {
	case "connecting", "reconnecting":
	default:
		return
	}
	now := time.Now()
	if !v.last.IsZero() && now.Sub(v.last) < 40*time.Millisecond {
		return
	}
	v.last = now
	v.phase += 0.09
	v.area.QueueDraw()
}

// routeColor is the accent shared by the routes, the nodes and the centre.
func (v *statusVisual) routeColor() (r, g, b, a float64) {
	switch v.state {
	case "connected":
		return 0.18, 0.83, 0.49, 1 // green
	case "connecting", "reconnecting":
		return 0.92, 0.65, 0.13, 1 // amber
	case "error":
		return 0.90, 0.30, 0.33, 1 // red
	default:
		return 0.55, 0.50, 0.68, 1 // muted lavender
	}
}

func (v *statusVisual) draw(_ *gtk.DrawingArea, cr *cairo.Context, width, height int) {
	w, h := float64(width), float64(height)
	if w < 10 || h < 10 {
		return
	}

	// Everything is clipped to the panel's rounded rectangle so the drawing
	// matches the corner radius of the cards around it.
	roundedRect(cr, 0, 0, w, h, 18)
	cr.Clip()

	// Deep indigo field, lighter at the top like the app's map.
	if gradient, err := cairo.NewPatternLinear(0, 0, 0, h); err == nil {
		gradient.AddColorStopRGB(0.00, 0.13, 0.10, 0.21)
		gradient.AddColorStopRGB(0.55, 0.09, 0.07, 0.15)
		gradient.AddColorStopRGB(1.00, 0.05, 0.04, 0.09)
		cr.SetSource(gradient)
		cr.Paint()
	} else {
		cr.SetSourceRGBA(0.08, 0.06, 0.13, 1)
		cr.Paint()
	}

	// A faint longitude/latitude grid, as if the panel were a globe.
	cr.SetLineWidth(1)
	cr.SetSourceRGBA(0.72, 0.62, 0.95, 0.055)
	cx, cy := w/2, h*0.38
	for i := 1; i <= 4; i++ {
		ellipse(cr, cx, cy, w*0.16*float64(i), h*0.09*float64(i))
		cr.Stroke()
	}
	for i := 1; i <= 3; i++ {
		cr.MoveTo(cx-w*0.52, cy-h*0.22+float64(i)*h*0.075)
		cr.CurveTo(cx-w*0.20, cy-h*0.34+float64(i)*h*0.075,
			cx+w*0.20, cy-h*0.34+float64(i)*h*0.075,
			cx+w*0.52, cy-h*0.22+float64(i)*h*0.075)
		cr.Stroke()
	}

	// A dot matrix, denser toward the middle, that gives the panel texture
	// without competing with the card that sits over it.
	for x := 18.0; x < w-10; x += 21 {
		for y := 18.0; y < h-10; y += 21 {
			dx, dy := (x-cx)/(w/2), (y-cy)/(h/2)
			if dx*dx+dy*dy > 1.25 {
				continue
			}
			cr.Arc(x, y, 1.15, 0, 2*math.Pi)
			cr.SetSourceRGBA(0.74, 0.66, 0.94, 0.11)
			cr.Fill()
		}
	}

	// The destinations. Fixed positions keep the picture calm across redraws;
	// the pulse lives in the radii, not in the layout.
	nodes := [][2]float64{
		{0.16, 0.20}, {0.33, 0.10}, {0.52, 0.24},
		{0.71, 0.12}, {0.86, 0.26}, {0.24, 0.44},
		{0.78, 0.46}, {0.44, 0.06},
	}
	nx, ny := w/2, h*0.34

	r, g, b, _ := v.routeColor()
	live := v.state == "connected" || v.state == "connecting" || v.state == "reconnecting"

	// Routes out of the local node to the rest of the network.
	for _, node := range nodes {
		x, y := node[0]*w, node[1]*h
		mx, my := (nx+x)/2, (ny+y)/2-18
		cr.MoveTo(nx, ny)
		cr.CurveTo(mx, ny, mx, my, x, y)
		if live {
			cr.SetSourceRGBA(r, g, b, 0.13)
			cr.SetLineWidth(7)
			cr.Stroke()
		}
		cr.SetSourceRGBA(r, g, b, 0.55)
		cr.SetLineWidth(1.25)
		cr.Stroke()
	}

	// Destination nodes, with a soft halo.
	for _, node := range nodes {
		x, y := node[0]*w, node[1]*h
		cr.Arc(x, y, 6.5, 0, 2*math.Pi)
		cr.SetSourceRGBA(r, g, b, 0.10)
		cr.Fill()
		cr.Arc(x, y, 2.6, 0, 2*math.Pi)
		cr.SetSourceRGBA(r, g, b, 0.85)
		cr.Fill()
	}

	// The local node, pulsing while something is in flight.
	pulse := 0.0
	if v.state == "connecting" || v.state == "reconnecting" {
		pulse = 3.5 + 2.5*math.Sin(v.phase)
	}
	cr.Arc(nx, ny, 25+pulse, 0, 2*math.Pi)
	cr.SetSourceRGBA(r, g, b, 0.07)
	cr.Fill()
	cr.Arc(nx, ny, 14+pulse*0.5, 0, 2*math.Pi)
	cr.SetSourceRGBA(r, g, b, 0.16)
	cr.Fill()
	cr.Arc(nx, ny, 7.5, 0, 2*math.Pi)
	cr.SetSourceRGBA(r, g, b, 0.95)
	cr.Fill()
	cr.Arc(nx, ny, 3.2, 0, 2*math.Pi)
	cr.SetSourceRGBA(1, 1, 1, 0.95)
	cr.Fill()

	cr.ResetClip()
}

// roundedRect builds a rounded-rectangle path. It stops short of filling or
// clipping because the caller decides which it wants.
func roundedRect(cr *cairo.Context, x, y, w, h, radius float64) {
	if radius > w/2 {
		radius = w / 2
	}
	if radius > h/2 {
		radius = h / 2
	}
	cr.NewPath()
	cr.Arc(x+w-radius, y+radius, radius, -math.Pi/2, 0)
	cr.Arc(x+w-radius, y+h-radius, radius, 0, math.Pi/2)
	cr.Arc(x+radius, y+h-radius, radius, math.Pi/2, math.Pi)
	cr.Arc(x+radius, y+radius, radius, math.Pi, 3*math.Pi/2)
	cr.ClosePath()
}

// ellipse builds an ellipse path centred at cx,cy using a scaled circle.
func ellipse(cr *cairo.Context, cx, cy, rx, ry float64) {
	if rx <= 0 || ry <= 0 {
		return
	}
	cr.Save()
	cr.Translate(cx, cy)
	cr.Scale(rx, ry)
	cr.Arc(0, 0, 1, 0, 2*math.Pi)
	cr.Restore()
}
