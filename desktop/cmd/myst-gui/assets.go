package main

import (
	_ "embed"

	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// The Mysterium mark, embedded so the window looks the same whether it was
// installed as a package or run straight out of an extracted folder. The
// package also installs themed icons; this is only the small white wordmark
// glyph in the header.
//
//go:embed assets/logo.png
var logoPNG []byte

// logoImage returns the mark at the requested logical size, or nil if the
// embedded image cannot be decoded. A nil image is not fatal: the wordmark text
// beside it still identifies the app.
func logoImage(size int) *gtk.Image {
	texture, err := gdk.NewTextureFromBytes(glib.NewBytes(logoPNG))
	if err != nil {
		return nil
	}
	img := gtk.NewImageFromPaintable(texture)
	img.SetPixelSize(size)
	return img
}
