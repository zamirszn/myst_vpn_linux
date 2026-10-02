package main

import (
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// Palette, sampled from the official Mysterium VPN desktop app: a desaturated
// indigo panel, slightly lighter cards, a magenta-purple accent for the primary
// action and a green bar across the top while the tunnel is up.
//
// It is a stylesheet rather than per-widget calls so the look lives in one
// place and the window code only has to say what a widget *is* (a card, a
// section title) by class name.
const stylesheet = `
@define-color bg        #2b2a44;
@define-color card      #363455;
@define-color field     #3e3c62;
@define-color field_hi  #48466f;
@define-color accent    #c14ef0;
@define-color accent_hi #d066f5;
@define-color muted     #a9a7c2;
@define-color green     #4c9a1e;
@define-color amber     #b9831b;

window, window.background {
	background-color: @bg;
	color: #ffffff;
}

label { color: #ffffff; }

/* --- top bar ----------------------------------------------------------- */

.connected-banner {
	background-color: @green;
	color: #ffffff;
	font-weight: 600;
	font-size: 13px;
	padding: 5px 0;
}
.connected-banner.connecting { background-color: @amber; }

.brand {
	font-size: 15px;
	letter-spacing: 1px;
	color: #ffffff;
}

/* --- cards ------------------------------------------------------------- */

.card {
	background-color: @card;
	border-radius: 14px;
	padding: 16px;
}

.status-heading {
	font-size: 20px;
	color: #ffffff;
}
.status-detail {
	color: @muted;
	font-size: 13px;
}

.section-title {
	color: @muted;
	font-size: 13px;
	font-weight: 600;
	margin-top: 4px;
}
.muted-key { color: @muted; }

/* --- buttons ----------------------------------------------------------- */

button {
	background-image: none;
	box-shadow: none;
	text-shadow: none;
	border: none;
	color: #ffffff;
}

button.connect-btn,
button.disconnect-btn {
	border-radius: 10px;
	padding: 11px 16px;
	font-weight: 600;
	font-size: 14px;
	margin-top: 6px;
}
button.connect-btn { background-color: @accent; }
button.connect-btn:hover { background-color: @accent_hi; }
button.disconnect-btn { background-color: #5d5c86; }
button.disconnect-btn:hover { background-color: #6c6b98; }
button.connect-btn:disabled,
button.disconnect-btn:disabled { opacity: 0.55; }

/* the small outlined pill used for Copy, like the Connect pills in the list */
button.chip {
	background-color: @field;
	border-radius: 8px;
	padding: 3px 12px;
	font-size: 12px;
}
button.chip:hover { background-color: @field_hi; }
button.chip:disabled { opacity: 0.45; }

/* Sign out, once clicked: the colour says "this will do something". */
button.chip.armed { background-color: #8c3b52; }
button.chip.armed:hover { background-color: #a14561; }

/* a quiet text button, for the secondary actions on the login screen */
button.link-btn {
	background-color: transparent;
	color: @muted;
	padding: 4px 8px;
	font-size: 13px;
}
button.link-btn:hover { color: #ffffff; }
button.link-btn:disabled { opacity: 0.45; }

/* --- login ------------------------------------------------------------- */

.login-title {
	font-size: 22px;
	font-weight: 700;
	color: #ffffff;
}
.login-sub {
	color: @muted;
	font-size: 13px;
}
.error-text {
	color: #ff8f9d;
	font-size: 13px;
}
.account-label {
	color: @muted;
	font-size: 12px;
}

/* --- inputs ------------------------------------------------------------ */

searchentry, entry {
	background-color: @field;
	background-image: none;
	color: #ffffff;
	border: none;
	box-shadow: none;
	border-radius: 12px;
	padding: 8px 12px;
	min-height: 22px;
}
searchentry image { color: @muted; }
searchentry:disabled { opacity: 0.6; }

dropdown > button {
	background-color: @field;
	border-radius: 12px;
	padding: 8px 12px;
	min-height: 22px;
}
dropdown > button:hover { background-color: @field_hi; }
dropdown > button:disabled { opacity: 0.5; }

popover > contents {
	background-color: @card;
	color: #ffffff;
	border-radius: 12px;
	border: 1px solid @field_hi;
}
popover listview, popover list { background-color: transparent; color: #ffffff; }
popover row:hover, popover listview > row:hover { background-color: @field_hi; }
popover row:selected, popover listview > row:selected { background-color: @accent; }

/* --- search results ---------------------------------------------------- */

list.results {
	background-color: @card;
	border-radius: 12px;
}
list.results row { padding: 2px 0; }
list.results row:hover { background-color: @field_hi; }
list.results row:first-child { border-radius: 12px 12px 0 0; }
list.results row:last-child  { border-radius: 0 0 12px 12px; }

/* --- IP type tabs ------------------------------------------------------ */

notebook { background: transparent; }
notebook > header {
	background: transparent;
	box-shadow: none;
	border: none;
	padding: 0;
}
notebook > header > tabs > tab {
	background: transparent;
	border: none;
	border-bottom: 2px solid transparent;
	border-radius: 0;
	box-shadow: none;
	padding: 8px 14px;
	min-width: 0;
}
notebook > header > tabs > tab label { color: @muted; font-weight: 600; }
notebook > header > tabs > tab:hover label { color: #ffffff; }
notebook > header > tabs > tab:checked { border-bottom-color: @accent; }
notebook > header > tabs > tab:checked label { color: #ffffff; }
notebook > header > tabs > tab:disabled label { opacity: 0.5; }
notebook > stack { min-height: 0; }

/* --- misc -------------------------------------------------------------- */

scrolledwindow, scrolledwindow > viewport { background: transparent; }
scrollbar slider { background-color: @field_hi; border-radius: 6px; min-width: 6px; }
scrollbar trough { background: transparent; }
`

// loadStyle installs the stylesheet for the whole display and asks for the dark
// variant of the system theme, so anything the stylesheet does not cover
// (popover arrows, scrollbars, selection handles) still matches.
func loadStyle() {
	if s := gtk.SettingsGetDefault(); s != nil {
		s.SetObjectProperty("gtk-application-prefer-dark-theme", true)
	}

	display := gdk.DisplayGetDefault()
	if display == nil {
		return // no display; nothing to style (and nothing to show either)
	}
	provider := gtk.NewCSSProvider()
	provider.LoadFromData(stylesheet)
	gtk.StyleContextAddProviderForDisplay(display, provider, gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
}