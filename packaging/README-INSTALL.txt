MYSTERIUM VPN — UBUNTU INSTALLATION
===================================

This folder contains the Mysterium VPN desktop app for 64-bit Ubuntu.
Install it with the included `install.sh`.


WHAT YOU NEED
-------------
A terminal and the runtime packages:

    sudo apt install -y wireguard-tools nftables iproute2 pkexec


WHAT TO DO
----------
1. Extract the archive, if you have not already:

       tar xzf mysterium-vpn-linux-amd64.tar.gz

2. Run the installer from inside the extracted folder:

       cd mysterium-vpn-linux-amd64
       sudo ./install.sh

   Enter your normal login password when asked.

3. Open "Mysterium VPN" from the application grid.

4. Log in with the email address of your Mysterium VPN account:
   - Type your email and click Continue.
   - Open the email on the same computer or your phone.
   - Copy the login link.
   - Back in the app, click Paste and then Log in.

You do not need to log out, restart, or add groups.
The installer starts the VPN service automatically.


WHAT GETS INSTALLED
-------------------
  /usr/local/bin/myst-gui                          the desktop app
  /usr/local/bin/mystd                             the privileged VPN service
  /usr/local/lib/mysterium-vpn/                    launcher and helpers
  /etc/systemd/system/mystd.service                the boot service
  /usr/local/share/applications/...                the application-menu entry
  /usr/local/share/icons/hicolor/...               the app icon

Account credentials are kept by the service in /var/lib/mystd.


IF THE APP SAYS "VPN SERVICE IS NOT RUNNING"
-------------------------------------------
Click "Start VPN service" in the app and enter your password.
The app asks the system to start its own service; you do not need a terminal.


IF CONNECTING SAYS "Operation not permitted"
--------------------------------------------
The service log will show wg-quick failing on /usr/bin/ip. That is the
NoNewPrivileges option in mystd.service colliding with Ubuntu's AppArmor
profile for wg-quick; it must be off. If you edited the unit yourself, remove
that line and run:

    sudo systemctl daemon-reload
    sudo systemctl restart mystd

Check the result with:

    systemctl show mystd -p NoNewPrivileges     # prints NoNewPrivileges=no


UNINSTALL
---------
From the extracted folder:

    sudo ./uninstall.sh

Removing keeps /var/lib/mystd so a reinstall does not force a new login.
Delete /var/lib/mystd manually only if you want to remove the saved login.


TROUBLESHOOTING
---------------
- The service log can be viewed with:
      journalctl -u mystd -n 100 --no-pager

- If the control socket cannot be reached after switching desktop users,
  restart the VPN service from the app's "Start VPN service" button. The
  service re-reads which user is at the desktop each time it starts.

- The app will not work on a non-systemd distribution without adapting
  mystd.service.
