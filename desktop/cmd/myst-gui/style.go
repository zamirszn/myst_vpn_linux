package main

import (
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// Palette, sampled from the official Mysterium VPN app: a near-black indigo
// background, deep violet surfaces, a magenta accent for the primary action and
// a lavender "connection card" that carries the location and the one button
// that matters.
//
// It is a stylesheet rather than per-widget calls so the look lives in one
// place and the window code only has to say what a widget *is* (a card, a
// section title) by class name.
const stylesheet = `
@define-color bg           #0f0c18;
@define-color header       #231936;
@define-color surface      #211a33;
@define-color field        #2b2142;
@define-color field_hi     #3a2d57;
@define-color line         #382b52;
@define-color accent       #c04ef0;
@define-color accent_hi    #d66cf7;
@define-color accent_dark  #9d2ad4;
@define-color green        #16a552;
@define-color green_hi     #1fbd61;
@define-color red          #b3352e;
@define-color amber        #c58a1c;
@define-color muted        #a79fbe;
@define-color light        #f3edf8;
@define-color light_text   #1c1526;
@define-color light_muted  #6b6182;
@define-color light_line   #ded1eb;

window, window.background {
	background-image: linear-gradient(155deg, #1a1129 0%, #0f0c18 52%, #0a0810 100%);
	color: #ffffff;
}

label { color: #ffffff; }

/* --- top bar ----------------------------------------------------------- */

.header {
	background-color: @header;
	padding: 13px 18px;
	border-bottom: 1px solid #34254e;
}

.brand-strong {
	font-size: 14px;
	font-weight: 800;
	letter-spacing: 2px;
	color: #ffffff;
}
.brand-light {
	font-size: 14px;
	font-weight: 300;
	letter-spacing: 2px;
	color: #ded5ee;
}
.brand-sub {
	font-size: 9px;
	letter-spacing: 2px;
	color: @muted;
}
.account-label {
	color: @muted;
	font-size: 11px;
}

/* --- full-width connection strip --------------------------------------- */

.status-strip {
	background-color: @green;
	color: #ffffff;
	font-size: 12px;
	font-weight: 700;
	letter-spacing: 0.5px;
	padding: 7px 0;
	border: none;
}
.status-strip.connecting { background-color: @amber; }
.status-strip.error      { background-color: @red; }
.status-strip.offline    { background-color: #4c4560; }

/* --- the lavender card over the map ------------------------------------ */

.status-card {
	background-color: @light;
	border-radius: 18px;
	padding: 15px 16px 14px 16px;
	border: 1px solid #ffffff;
}
.status-card label { color: @light_text; }

.status-card .status-heading {
	font-size: 19px;
	font-weight: 800;
}
.status-card .status-detail {
	font-size: 12px;
	color: @light_muted;
}
.status-card .location-name {
	font-size: 16px;
	font-weight: 700;
}
.status-card .location-meta {
	font-size: 12px;
	color: @light_muted;
}
.status-card .exit-ip {
	font-size: 11px;
	color: @light_muted;
}
.status-card .exit-value {
	font-family: monospace;
	font-size: 12px;
	color: @light_text;
}
.status-card .flag {
	font-size: 22px;
}

.status-card button.chip {
	background-color: #e5d9f1;
	color: @light_text;
	border-radius: 8px;
	padding: 4px 11px;
	font-size: 11px;
	font-weight: 600;
}
.status-card button.chip:hover { background-color: #dbcbe9; }
.status-card button.chip:disabled { opacity: 0.45; }

button.connect-btn,
button.disconnect-btn {
	border-radius: 12px;
	padding: 12px 16px;
	font-weight: 800;
	font-size: 15px;
	margin-top: 2px;
}
button.connect-btn {
	background-image: linear-gradient(135deg, @accent_hi 0%, @accent_dark 100%);
	color: #ffffff;
}
button.connect-btn:hover { background-image: linear-gradient(135deg, #e07dfb 0%, #ad38e2 100%); }
button.disconnect-btn {
	background-color: #ffffff;
	color: @light_text;
	border: 1px solid @light_line;
}
button.disconnect-btn:hover { background-color: #fbf8fe; }
button.connect-btn:disabled,
button.disconnect-btn:disabled { opacity: 0.55; }

/* --- generic buttons --------------------------------------------------- */

button {
	background-image: none;
	box-shadow: none;
	text-shadow: none;
	border: none;
	color: #ffffff;
}

button.chip {
	background-color: @field;
	border-radius: 9px;
	padding: 5px 12px;
	font-size: 11px;
	font-weight: 600;
}
button.chip:hover { background-color: @field_hi; }
button.chip:disabled { opacity: 0.45; }

button.chip.armed { background-color: #7d2a3e; }
button.chip.armed:hover { background-color: #94324a; }

button.link-btn {
	background-color: transparent;
	color: @muted;
	padding: 4px 8px;
	font-size: 12px;
}
button.link-btn:hover { color: #ffffff; }
button.link-btn:disabled { opacity: 0.45; }

/* --- cards and section titles ------------------------------------------ */

.card {
	background-color: @surface;
	border-radius: 14px;
	padding: 14px;
	border: 1px solid @line;
}

.section-title {
	color: @muted;
	font-size: 11px;
	font-weight: 700;
	letter-spacing: 1px;
	margin-top: 2px;
}
.muted-key { color: @muted; font-size: 12px; }
.detail-value { color: #ffffff; font-size: 12px; }

/* --- login ------------------------------------------------------------- */

.login-card {
	background-color: @surface;
	border: 1px solid @line;
	border-radius: 18px;
	padding: 22px;
}
.login-title {
	font-size: 22px;
	font-weight: 800;
	color: #ffffff;
}
.login-sub {
	color: @muted;
	font-size: 12px;
}
.error-text {
	color: #ff93a0;
	font-size: 12px;
}
.login-foot {
	color: #6f6884;
	font-size: 10px;
}

/* --- inputs ------------------------------------------------------------ */

searchentry, entry {
	background-color: @field;
	background-image: none;
	color: #ffffff;
	border: 1px solid @line;
	box-shadow: none;
	border-radius: 12px;
	padding: 8px 12px;
	min-height: 24px;
}
searchentry:focus, entry:focus { border-color: #5a4780; }
searchentry image { color: @muted; }
searchentry:disabled { opacity: 0.6; }

dropdown > button {
	background-color: @field;
	border: 1px solid @line;
	border-radius: 12px;
	padding: 9px 13px;
	min-height: 24px;
	font-size: 14px;
}
dropdown > button:hover { background-color: @field_hi; }
dropdown > button:disabled { opacity: 0.5; }

popover > contents {
	background-color: @surface;
	color: #ffffff;
	border-radius: 12px;
	border: 1px solid @line;
}
popover listview, popover list { background-color: transparent; color: #ffffff; }
popover row:hover, popover listview > row:hover { background-color: @field_hi; }
popover row:selected, popover listview > row:selected { background-color: @accent_dark; }

/* --- search results ---------------------------------------------------- */

list.results {
	background-color: @surface;
	border: 1px solid @line;
	border-radius: 12px;
}
list.results row { padding: 2px 0; }
list.results row:hover { background-color: @field_hi; }
list.results row:first-child { border-radius: 12px 12px 0 0; }
list.results row:last-child  { border-radius: 0 0 12px 12px; }

/* --- IP type segmented control ----------------------------------------- */

notebook { background: transparent; }
notebook > header {
	background-color: @field;
	border: 1px solid @line;
	border-radius: 12px;
	box-shadow: none;
	padding: 3px;
}
notebook > header > tabs > tab {
	background: transparent;
	border: none;
	border-radius: 9px;
	box-shadow: none;
	padding: 7px 12px;
	min-width: 0;
}
notebook > header > tabs > tab label {
	color: @muted;
	font-size: 12px;
	font-weight: 700;
}
notebook > header > tabs > tab:hover label { color: #ffffff; }
notebook > header > tabs > tab:checked { background-color: @field_hi; }
notebook > header > tabs > tab:checked label { color: #ffffff; }
notebook > header > tabs > tab:disabled label { opacity: 0.5; }
notebook > stack { min-height: 0; }

/* --- misc -------------------------------------------------------------- */

scrolledwindow, scrolledwindow > viewport { background: transparent; }
scrollbar slider { background-color: @field_hi; border-radius: 6px; min-width: 6px; }
scrollbar trough { background: transparent; }

.map-panel { background-color: transparent; }
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
