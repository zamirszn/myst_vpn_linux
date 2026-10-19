package main

import (
	"context"
	"strings"
	"time"

	gio "github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"

	"github.com/zamirszn/mysterium-linux-client/desktop/internal/daemonclient"
	"github.com/zamirszn/mysterium-linux-client/desktop/internal/regions"
)

// The window has two screens and the daemon decides which one is showing: the
// login screen while it holds no credentials, the main screen otherwise. Nothing
// in the window remembers who is signed in. It reads the answer from every
// status poll, so a login or logout done from the CLI changes the window too.
//
// Logging in is the official app's flow: type an email, get a link, come back.
// The one difference is the last step. The official app is registered to catch
// the link and opens itself; a third-party client cannot be, so the person pastes
// the link instead. The window does no parsing and holds no secret in doing so:
// the daemon keeps the PKCE verifier, and takes the pasted text as it is.

// loginTimeout bounds one login request. The daemon's own call to the API gives
// up after 30 seconds, so this only has to outlast that.
const loginTimeout = 45 * time.Second

// signOutConfirmWindow is how long the Sign out button stays armed after the
// first click. Signing out drops a live tunnel and forgets the account, so a
// stray click must not do it — but a dialog is more machinery than that is worth.
const signOutConfirmWindow = 4 * time.Second

// loginView is the widgets of the login screen.
type loginView struct {
	steps *gtk.Stack // "email", then "link"

	email    *gtk.Entry
	send     *gtk.Button
	emailErr *gtk.Label

	sentTo   *gtk.Label
	link     *gtk.Entry
	finish   *gtk.Button
	resend   *gtk.Button
	back     *gtk.Button
	linkNote *gtk.Label
	linkErr  *gtk.Label

	// asked is the address the link was sent to, for resending. Only the main
	// loop touches it.
	asked string
}

// newText is a wrapped, left-aligned label with a style class.
func newText(class, text string) *gtk.Label {
	l := gtk.NewLabel(text)
	l.SetWrap(true)
	l.SetXAlign(0)
	l.SetHAlign(gtk.AlignStart)
	l.AddCSSClass(class)
	return l
}

// buildLogin assembles the login screen.
func (w *window) buildLogin() *gtk.Box {
	l := &w.login

	// --- step 1: the address -------------------------------------------------
	l.email = gtk.NewEntry()
	l.email.SetObjectProperty("placeholder-text", "Email address")
	l.email.ConnectActivate(func() { w.sendLink(false) })

	l.send = gtk.NewButtonWithLabel("Continue")
	l.send.AddCSSClass("connect-btn")
	l.send.ConnectClicked(func() { w.sendLink(false) })

	l.emailErr = newText("error-text", "")
	l.emailErr.SetVisible(false)

	emailStep := gtk.NewBox(gtk.OrientationVertical, 12)
	emailStep.AddCSSClass("login-card")
	emailStep.Append(newText("login-title", "Log in"))
	emailStep.Append(newText("login-sub",
		"Enter the email address of your Mysterium VPN account and we'll send you a link to log in."))
	emailStep.Append(l.email)
	emailStep.Append(l.emailErr)
	emailStep.Append(l.send)

	// --- step 2: the link ----------------------------------------------------
	l.sentTo = newText("login-sub", "")

	l.link = gtk.NewEntry()
	l.link.SetObjectProperty("placeholder-text", "Paste the link from the email")
	l.link.SetHExpand(true)
	l.link.ConnectActivate(func() { w.finishLogin() })

	// Right-click paste works in the entry already; this makes the one step
	// everyone gets stuck on obvious, and tells them when the clipboard is
	// empty instead of leaving the Log in button to say "paste the link first".
	paste := gtk.NewButtonWithLabel("Paste")
	paste.AddCSSClass("chip")
	paste.ConnectClicked(func() {
		paste.SetSensitive(false)
		w.win.Clipboard().ReadTextAsync(context.Background(), func(res gio.AsyncResulter) {
			text, err := w.win.Clipboard().ReadTextFinish(res)
			w.onMain(func() {
				paste.SetSensitive(true)
				text = strings.TrimSpace(text)
				if err != nil || text == "" {
					w.loginFail(l.linkErr, "Your clipboard is empty. Copy the link from the email first.")
					return
				}
				l.link.SetText(text)
				l.linkErr.SetVisible(false)
				l.link.GrabFocus()
			})
		})
	})

	entryRow := gtk.NewBox(gtk.OrientationHorizontal, 8)
	entryRow.Append(l.link)
	entryRow.Append(paste)

	l.finish = gtk.NewButtonWithLabel("Log in")
	l.finish.AddCSSClass("connect-btn")
	l.finish.ConnectClicked(func() { w.finishLogin() })

	l.linkNote = newText("login-sub", "")
	l.linkNote.SetVisible(false)
	l.linkErr = newText("error-text", "")
	l.linkErr.SetVisible(false)

	l.resend = gtk.NewButtonWithLabel("Send a new link")
	l.resend.AddCSSClass("link-btn")
	l.resend.ConnectClicked(func() { w.sendLink(true) })

	l.back = gtk.NewButtonWithLabel("Use a different email")
	l.back.AddCSSClass("link-btn")
	l.back.ConnectClicked(func() { w.showLoginEmail() })

	links := gtk.NewBox(gtk.OrientationHorizontal, 4)
	links.Append(l.resend)
	links.Append(l.back)

	linkStep := gtk.NewBox(gtk.OrientationVertical, 12)
	linkStep.AddCSSClass("login-card")
	linkStep.Append(newText("login-title", "Check your email"))
	linkStep.Append(l.sentTo)
	linkStep.Append(entryRow)
	linkStep.Append(l.linkErr)
	linkStep.Append(l.linkNote)
	linkStep.Append(l.finish)
	linkStep.Append(links)

	l.steps = gtk.NewStack()
	l.steps.AddNamed(emailStep, "email")
	l.steps.AddNamed(linkStep, "link")
	l.steps.SetVisibleChildName("email")

	root := gtk.NewBox(gtk.OrientationVertical, 0)
	root.SetMarginStart(20)
	root.SetMarginEnd(20)
	root.SetMarginBottom(20)
	root.SetVExpand(true)
	root.SetVAlign(gtk.AlignCenter)
	root.Append(l.steps)
	return root
}

// loginErrorText turns a failure into words for the login screen. The
// daemon's own messages are already written for a person, so most pass through;
// the exception is a daemon too old to know the command, which is the one
// failure the person cannot fix by trying again.
func loginErrorText(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "unknown command") {
		return "The daemon is older than this window and cannot log in. " +
			"Rebuild mystd, install it and restart the service, then try again."
	}
	return msg
}

// loginFail shows a message under a step's field.
func (w *window) loginFail(label *gtk.Label, msg string) {
	label.SetText(msg)
	label.SetVisible(true)
}

// setLoginBusy locks the login screen while a request is out, so a second click
// cannot start a second link, and says what is happening on the buttons.
func (w *window) setLoginBusy(on bool) {
	l := &w.login
	l.email.SetSensitive(!on)
	l.link.SetSensitive(!on)
	l.send.SetSensitive(!on)
	l.finish.SetSensitive(!on)
	l.resend.SetSensitive(!on)
	l.back.SetSensitive(!on)
	if on {
		l.send.SetLabel("Sending…")
		l.finish.SetLabel("Logging in…")
		return
	}
	l.send.SetLabel("Continue")
	l.finish.SetLabel("Log in")
}

// showLoginEmail goes to the first step, dropping whatever the second held.
func (w *window) showLoginEmail() {
	w.resetLogin()
	w.login.email.GrabFocus()
}

// resetLogin returns the login screen to its first step with nothing on it. It
// does not move focus, so it is safe to call while the screen is hidden.
func (w *window) resetLogin() {
	l := &w.login
	l.asked = ""
	l.link.SetText("")
	l.emailErr.SetVisible(false)
	l.linkErr.SetVisible(false)
	l.linkNote.SetVisible(false)
	l.steps.SetVisibleChildName("email")
}

// showLoginLink moves to the second step for a link sent to addr.
func (w *window) showLoginLink(addr string) {
	l := &w.login
	l.asked = addr
	l.sentTo.SetText("We sent a login link to " + addr + ". Open the email and paste its link here — " +
		"right-click the link and choose Copy link address. It only works for a few minutes.")
	l.link.SetText("")
	l.linkErr.SetVisible(false)
	l.linkNote.SetVisible(false)
	l.steps.SetVisibleChildName("link")
	l.link.GrabFocus()
}

// sendLink asks the daemon to email a login link. With resend it uses the
// address already given, from the second step, and stays on that step.
func (w *window) sendLink(resend bool) {
	l := &w.login

	addr := strings.TrimSpace(l.email.Text())
	errLabel := l.emailErr
	if resend {
		addr = l.asked
		errLabel = l.linkErr
	}
	errLabel.SetVisible(false)
	l.linkNote.SetVisible(false)

	if addr == "" {
		w.loginFail(errLabel, "Enter your email address.")
		return
	}
	if !w.loginBusy.CompareAndSwap(false, true) {
		return
	}
	w.setLoginBusy(true)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
		defer cancel()

		_, err := w.client.LoginStart(ctx, addr)

		w.onMain(func() {
			w.loginBusy.Store(false)
			w.setLoginBusy(false)
			switch {
			case err != nil:
				w.loginFail(errLabel, loginErrorText(err))
			case resend:
				l.linkNote.SetText("A new link is on its way. Use the newest one — the earlier link no longer works.")
				l.linkNote.SetVisible(true)
			default:
				w.showLoginLink(addr)
			}
		})
	}()
}

// finishLogin hands the pasted link to the daemon. On success the daemon's
// reply says it now holds an account, and applying it is what switches screens.
func (w *window) finishLogin() {
	l := &w.login

	pasted := strings.TrimSpace(l.link.Text())
	l.linkErr.SetVisible(false)
	if pasted == "" {
		w.loginFail(l.linkErr, "Paste the link from the email first.")
		return
	}
	if !w.loginBusy.CompareAndSwap(false, true) {
		return
	}
	w.setLoginBusy(true)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
		defer cancel()

		st, err := w.client.LoginFinish(ctx, pasted)

		w.onMain(func() {
			w.loginBusy.Store(false)
			w.setLoginBusy(false)
			if err != nil {
				w.loginFail(l.linkErr, loginErrorText(err))
				return
			}
			// This answer is newer than any poll still in flight, and one of
			// those could say "signed out" and put the login screen straight
			// back over the account that was just logged in.
			w.generation.Add(1)
			w.resetLogin()
			l.email.SetText("")
			if st != nil {
				w.apply(*st)
			}
		})
	}()
}

// buildAccountBar is the top-right corner of the main screen: who is signed in,
// and the way out.
func (w *window) buildAccountBar() *gtk.Box {
	w.account = gtk.NewLabel("")
	w.account.AddCSSClass("account-label")
	w.account.SetEllipsize(pango.EllipsizeMiddle)
	w.account.SetMaxWidthChars(22)

	w.signOut = gtk.NewButtonWithLabel("Sign out")
	w.signOut.AddCSSClass("chip")
	w.signOut.ConnectClicked(func() { w.onSignOutClicked() })

	w.accountBar = gtk.NewBox(gtk.OrientationHorizontal, 10)
	w.accountBar.SetVAlign(gtk.AlignCenter)
	w.accountBar.Append(w.account)
	w.accountBar.Append(w.signOut)
	return w.accountBar
}

// onSignOutClicked asks twice. The first click arms the button and says what
// will happen; a second click inside the window does it.
func (w *window) onSignOutClicked() {
	if w.busy.Load() {
		return
	}
	if w.signOutArmed {
		w.disarmSignOut()
		w.doSignOut()
		return
	}

	w.signOutArmed = true
	if w.onScreen().State == daemonclient.StateConnected {
		w.signOut.SetLabel("Disconnect and sign out?")
	} else {
		w.signOut.SetLabel("Confirm sign out")
	}
	w.signOut.AddCSSClass("armed")

	seq := w.armSeq
	glib.TimeoutAdd(uint(signOutConfirmWindow.Milliseconds()), func() bool {
		// A timer left over from an earlier arming must not cancel a newer one.
		if w.armSeq == seq {
			w.disarmSignOut()
		}
		return false // one shot
	})
}

// disarmSignOut puts the button back, and retires any timer waiting to do so.
func (w *window) disarmSignOut() {
	w.armSeq++
	w.signOutArmed = false
	w.signOut.SetLabel("Sign out")
	w.signOut.RemoveCSSClass("armed")
}

// doSignOut has the daemon close any tunnel and delete the credentials.
func (w *window) doSignOut() {
	if !w.busy.CompareAndSwap(false, true) {
		return
	}
	w.notice = ""
	w.action.SetSensitive(false)
	w.signOut.SetSensitive(false)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()

		st, err := w.client.Logout(ctx)

		w.onMain(func() {
			w.generation.Add(1)
			w.busy.Store(false)
			w.action.SetSensitive(true)
			w.signOut.SetSensitive(true)

			switch {
			case err != nil && st != nil:
				// Refused — most likely a tunnel that would not close. The
				// reason has to outlast the next poll, which would otherwise
				// repaint the detail line over it two seconds later.
				w.notice = err.Error()
				w.apply(*st)
			case err != nil:
				w.showUnreachable(err)
			default:
				w.apply(*st)
			}
		})
	}()
}

// onAccountChanged drops everything that belongs to the previous account. It
// runs when the window goes from signed in to signed out and back, so the next
// account is never offered the last one's countries and cities.
func (w *window) onAccountChanged() {
	// Any lookup still in flight was made for the old account; its answer is
	// discarded when it lands rather than filling the picker.
	w.epoch++

	// Reset state in-place. The widgets are already attached to the visible GTK
	// hierarchy; replacing their Go pointers here creates detached widgets and
	// leaves the visible controls wired to stale state after login/logout.
	w.clearSearch()
	w.places = nil
	w.placesByType = map[string][]regions.Place{}
	w.loadingPlaces = map[string]bool{}
	w.haveCountries.Store(false)
	w.cityChoice = ""
	w.notice = ""

	w.choices = regions.All
	w.region.SetModel(gtk.NewStringList(w.pickerLabels()))
	w.region.SetSelected(0)
	w.ipType.SetCurrentPage(0)
	w.city.SetModel(gtk.NewStringList([]string{"Any city"}))
	w.city.SetSelected(0)
	w.note.SetMarkup(fallbackNote)
	w.note.SetVisible(true)
	w.search.SetSensitive(false)
	w.results.SetVisible(false)
	w.copy.SetSensitive(false)
	w.exitLine.SetText("Not connected")
	w.iface.SetText("—")
	w.ipv6.SetText("—")
	w.ks.SetText("—")
	w.refreshCityPicker()
	w.applyCitySensitivity()
	w.refreshLocationCard()
}
