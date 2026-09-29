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
	place  = regionlist.Place
	city   = regionlist.City
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

	citiesOf = regionlist.CitiesOf
	cityName = regionlist.CityName
)

// fromLocations turns the daemon's location answer into the menu's data. The
// conversion lives here, at the boundary, so the regions package does not have
// to import the client.
func fromLocations(in []daemonclient.LocationCountry) []place {
	raw := make([]regionlist.RawCountry, 0, len(in))
	for _, c := range in {
		rc := regionlist.RawCountry{Code: c.Code, Name: c.Name}
		for _, ct := range c.Cities {
			rc.Cities = append(rc.Cities, regionlist.RawCity{Slug: ct.Slug, Name: ct.Name})
		}
		raw = append(raw, rc)
	}
	return regionlist.FromLocations(raw)
}
