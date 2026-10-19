MYSTERIUM VPN — UBUNTU INSTALLATION (NO TERMINAL NEEDED)
=========================================================

This folder contains the Mysterium VPN desktop app for 64-bit Ubuntu.
The recommended way to install it is the .deb package.


WHAT TO DO
----------
1. Copy `mysterium-vpn_1.0.0_amd64.deb` to the other Ubuntu PC.

2. Double-click the .deb file.
   Ubuntu opens it in App Center (or "Software Install" on older releases).
   If double-click opens the archive viewer instead, right-click the file,
   choose "Open With", and pick App Center / Software Install.

3. Click Install, then enter your normal login password when asked.

4. Open "Mysterium VPN" from the application grid.

5. Log in with the email address of your Mysterium VPN account:
   - Type your email and click Continue.
   - Open the email on the same computer or your phone.
   - Copy the login link.
   - Back in the app, click Paste and then Log in.

You do not need to log out, restart, add groups, or use a terminal.
The installer starts the VPN service automatically.


WHAT GETS INSTALLED
-------------------
  /usr/bin/myst-gui                     the desktop app
  /usr/lib/mysterium-vpn/mystd          the privileged VPN service
  /usr/lib/systemd/system/mystd.service the boot service
  /usr/share/applications/...           the application-menu entry
  /usr/share/icons/hicolor/...          the app icon

Account credentials are kept by the service in /var/lib/mystd.


IF THE APP SAYS "VPN SERVICE IS NOT RUNNING"
-------------------------------------------
Click "Start VPN service" in the app and enter your password.
The app asks the system to start its own service; you do not need a terminal.


IF YOU PREFER THE PORTABLE ARCHIVE
----------------------------------
The zip also contains `install.sh` for systems without a Debian package
manager. That path does use a terminal:

    sudo ./install.sh

It installs to /usr/local and starts the same service.


UNINSTALL
---------
Remove "Mysterium VPN" from App Center / Software, or run:

    sudo apt remove mysterium-vpn

Removing keeps /var/lib/mystd so a reinstall does not force a new login.
"sudo apt purge mysterium-vpn" also deletes the saved login and local settings.


TROUBLESHOOTING
---------------
- The service log can be viewed with:
      journalctl -u mystd -n 100 --no-pager

- If the control socket cannot be reached after switching desktop users,
  restart the VPN service from the app's "Start VPN service" button. The
  service re-reads which user is at the desktop each time it starts.

- WireGuard, nftables, iproute2 and pkexec are installed automatically as
  package dependencies. The app will not work on a non-systemd distribution
  without adapting mystd.service.
