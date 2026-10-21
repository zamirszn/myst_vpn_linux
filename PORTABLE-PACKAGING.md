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

- `dist/mysterium-vpn-linux-amd64.tar.gz`
- `dist/mysterium-vpn-linux-amd64.zip`

The archive contains both binaries, the launcher and socket group helpers,
icons, `install.sh`, and `uninstall.sh`. The script fails immediately if GTK 4
development files are missing or either build fails; it never ships a stale GUI
binary.

The Debian packaging has been deliberately removed. A `.deb` and the portable
installer put the systemd unit in different directories (`/usr/lib` and
`/etc`), and systemd prefers the `/etc` copy. Once both installs have been
used, the leftover unit silently takes over and package upgrades stop having
any effect.

## Install on another compatible PC

Copy the archive to the other PC, extract it, and run the installer:

```bash
sudo apt install -y wireguard-tools nftables iproute2 pkexec
tar xzf mysterium-vpn-linux-amd64.tar.gz
cd mysterium-vpn-linux-amd64
sudo ./install.sh
```

Then launch Mysterium VPN from the application menu. No group change or log-out
is required: the service launcher gives the control socket the primary group of
the desktop user, which that user already belongs to.

The portable installer refuses to run while an old `mysterium-vpn` package is
installed. Remove it first with `sudo apt remove mysterium-vpn`; the saved login
under `/var/lib/mystd` survives.

This is a native amd64 Linux package, not a universal package for every Linux
distribution or CPU architecture. It requires systemd, GTK 4 runtime libraries,
and the VPN/network utilities used by the daemon (installed with the `apt`
command above). Account data under `/var/lib/mystd` is preserved on a normal
uninstall; `uninstall.sh` never deletes it.

## Honest limitations

The source bundle includes the packaging scripts, not precompiled executables.
Run the build script on the target Ubuntu build machine; it reports compiler
errors directly rather than claiming success if a build fails.
