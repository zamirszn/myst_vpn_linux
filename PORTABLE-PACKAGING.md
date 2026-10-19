# Build a portable Mysterium Linux installation package

This project contains two Go modules: the daemon at the repository root and the
GTK desktop app under `desktop/`. Both are built from the same source tree so
the GUI and daemon IPC protocol cannot drift apart.

## Build on Ubuntu/Debian amd64

Install build prerequisites:

```bash
sudo apt update
sudo apt install -y build-essential pkg-config libgtk-4-dev gobject-introspection policykit-1 zip golang-go
```

Then run from the repository root:

```bash
./build-portable-package.sh
```

This produces:

- `dist/mysterium-vpn_1.0.0_amd64.deb` — the recommended installer
- `dist/mysterium-vpn-linux-amd64.zip`
- `dist/mysterium-vpn-linux-amd64.tar.gz`

The archive contains the same `.deb`, both binaries, the launcher and socket
group helpers, icons, and `install.sh` for systems where the package manager is
not used. The script fails immediately if GTK 4 development files are missing
or either build fails; it never ships a stale GUI binary.

## Install on another compatible PC

**Recommended:** copy the `.deb` to the other PC and double-click it. Ubuntu
opens App Center / Software Install; approve the password prompt and launch
Mysterium VPN from the application menu. No terminal, group change, or log-out
is required. The service launcher gives the control socket the primary group of
the desktop user, which that user already belongs to.

If the `.deb` route is unavailable, extract the archive and run `sudo
./install.sh`, then launch the app from the menu. The same no-log-out rule
applies.

This is a native amd64 Linux package, not a universal package for every Linux
distribution or CPU architecture. It requires systemd, GTK 4 runtime libraries,
and the VPN/network utilities used by the daemon (installed automatically as
package dependencies). Account data under `/var/lib/mystd` is preserved on
normal uninstall and deleted on purge.

## Honest limitations

The source bundle includes the packaging scripts, not precompiled executables.
Run the build script on the target Ubuntu build machine; it reports compiler
errors directly rather than claiming success if a build fails.
