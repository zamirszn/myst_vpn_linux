# Build a portable Mysterium Linux installation package

This project contains two Go modules: the daemon at the repository root and the GTK desktop GUI under `desktop/`. Build both from the same source tree to avoid GUI/daemon protocol mismatches.

## Build on Ubuntu/Debian amd64

Install build prerequisites:

```bash
sudo apt update
sudo apt install -y build-essential pkg-config libgtk-4-dev policykit-1 zip golang-go
```

Then run from the repository root:

```bash
./build-portable-package.sh
```

The script compiles `./cmd/mystd` and `./desktop/cmd/myst-gui`, then creates `dist/mysterium-linux-amd64.zip` and `dist/mysterium-linux-amd64.tar.gz`. It fails immediately if GTK 4 development files are missing or either build fails. It does not ship an old prebuilt GUI binary.

## Install on another compatible PC

Transfer `dist/mysterium-linux-amd64.zip`, extract it, and run from the extracted directory:

```bash
sudo ./install.sh
```

Then log out of the desktop session and back in so the new `myst` group membership applies. Launch Mysterium VPN from the app menu.

This is a native amd64 Linux package, not a universal package for every Linux distribution or CPU architecture. It requires systemd, GTK 4 runtime libraries, and the usual VPN/network utilities used by the daemon. The package installs a system daemon service and preserves account data under `/var/lib/mystd` on uninstall.

## Honest limitations

The source bundle includes the packaging scripts, not precompiled executables. The current build environment does not have GTK 4 development files installed, so a final binary package cannot be built or validated here. Run the build script on the target Ubuntu build machine; the script reports compiler errors directly rather than claiming success if a build fails.
