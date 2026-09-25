package main

// The applet shares its control-socket client and its region list with the GTK
// window, so those live in desktop/internal and are imported here.
//
// These aliases let the applet keep reading in its own vocabulary. The
// alternative — rewriting every mention across a thousand lines of tested D-Bus
// and menu code — would be churn with no behaviour to show for it, and would
// put working code at risk for the sake of a rename. The types are aliases
// rather than new definitions, so a *client here is a *daemonclient.Client and
// nothing needs converting at the boundary.

import (
	daemonclient "github.com/zamirszn/mysterium-linux-client/desktop/internal/daemonclient"
	regionlist "github.com/zamirszn/mysterium-linux-client/desktop/internal/regions"
)

type (
	client = daemonclient.Client
	status = daemonclient.Status

	region = regionlist.Region
)

const (
	defaultSocketPath = daemonclient.DefaultSocketPath

	stateDisconnected = daemonclient.StateDisconnected
	stateConnecting   = daemonclient.StateConnecting
	stateConnected    = daemonclient.StateConnected
	stateReconnecting = daemonclient.StateReconnecting
	stateError        = daemonclient.StateError
)

var (
	// fallbackRegions is the built-in list, shown only until the daemon supplies
	// the account's real one. It is named for what it is rather than "regions":
	// the applet's live list is app.choices, and a package-level `regions` that
	// silently stopped being the list on screen would be a trap.
	fallbackRegions = regionlist.All

	fromCodes  = regionlist.FromCodes
	regionName = regionlist.Name
	titleCase  = regionlist.TitleCase
)
