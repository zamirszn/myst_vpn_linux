# The development machine

Notes about the box this was built and tested on. Not about the client, which is
why they are no longer in the README — kept because one of them is actionable.

## `mystd` is not what keeps this box awake

It was already configured that way, and the systemd masks predate this project by
over a year (2025-05-28):

| Setting | Value | Effect |
|---|---|---|
| `sleep.target` | masked | cannot suspend at all |
| `suspend.target` | masked | cannot suspend at all |
| `org.gnome.desktop.session idle-delay` | `0` | screen never blanks |
| `org.gnome.desktop.screensaver lock-enabled` | `false` | screen never locks |
| `sleep-inactive-ac-type` / `-battery-type` | `nothing` | GNOME never suspends |

This matters when testing the wake lock: the daemon's inhibitor is not the reason
the machine stays up here, so "it did not suspend" is not evidence the inhibitor
works. Test that on a machine with stock power settings.

The one gap: `hibernate.target` and `hybrid-sleep.target` are **not** masked, so
`systemctl hibernate` still works. To close it:

```sh
sudo systemctl mask hibernate.target hybrid-sleep.target
```

`systemd-inhibit --list` shows what is currently holding the machine awake.

## Wifi testing is not possible here

Every visible AP is WPA2/WPA3 and the only saved networks are out of range, so
the live drop tests simulate a dropped tunnel from outside the daemon
(`wg-quick down myst0`) rather than by actually changing networks. That exercises
the same detection path — the interface vanishes — but it does *not* exercise the
default-route check, which is the signal a real network change produces. That
check is covered by unit tests only.

## `sudo` needs a password

`sudo -n true` fails, so every privileged step has to be run by hand as
`sudo sh <script>` rather than from an automated loop.
