module github.com/zamirszn/mysterium-linux-client/desktop

go 1.23.2

require (
	github.com/zamirszn/mysterium-linux-client v0.0.0
	github.com/diamondburned/gotk4/pkg v0.3.1
	github.com/godbus/dbus/v5 v5.2.2
)

require (
	github.com/KarpelesLab/weak v0.1.1 // indirect
	go4.org/unsafe/assume-no-moving-gc v0.0.0-20231121144256-b99613f794c // indirect
	golang.org/x/sync v0.0.0-20210220032951-036812b2e83c // indirect
	golang.org/x/sys v0.27.0 // indirect
)

replace github.com/zamirszn/mysterium-linux-client => ..
