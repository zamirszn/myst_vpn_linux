# GUI login/logout fix

This update fixes the case where the VPN screen appeared without any account controls.

- The GUI starts on the email magic-link login screen and only switches to the VPN screen after the daemon confirms an account is signed in.
- If the daemon reports `logged_in: false`, login is shown and the account bar is hidden.
- If an older daemon omits `logged_in`, the GUI now shows login rather than silently leaving the user on the VPN screen with no login/logout controls.
- If the daemon cannot be reached, login remains visible with an explanation. Login requests still require the daemon to be running and updated.
- After successful login, the account bar displays the account and a **Sign out** button. Sign out requires confirmation and asks the daemon to disconnect and remove the credentials.
- Both the root `myst --gui` path and the separate `desktop/myst-gui` path have the same state-handling fix.

## Build the combined app (recommended)

From this project directory, install the Go and GTK4 development dependencies, then run:

```bash
go build -o myst ./cmd/myst
```

Run the rebuilt app with:

```bash
./myst --gui
```

## Build the separate desktop GUI

```bash
./build-desktop-gui.sh
```

The separate GUI build requires Go, GTK4 development headers, `pkg-config`, and network access (or cached Go modules). The login/logout protocol also requires a daemon built from this project revision; restart/reinstall the daemon if it is older.

## Verification

The account-state unit tests pass for both daemon-client packages. A full GTK GUI build could not be verified in this environment because the Go module proxy was unreachable, so this ZIP contains updated source rather than a precompiled executable.
