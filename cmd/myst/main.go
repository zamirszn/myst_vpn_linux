// Command myst is the unprivileged control CLI. It talks to mystd over a unix
// socket and never touches the network or the tunnel directly.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/zamirszn/mysterium-linux-client/internal/auth"
	"github.com/zamirszn/mysterium-linux-client/internal/ipc"
)

const version = "0.1.0"

const usage = `myst — Mysterium VPN 2.0 client for Linux

usage: myst <command> [flags]

commands:
  connect      bring the tunnel up
  disconnect   tear the tunnel down
  status       report the current state
  login        import credentials from a captured token
  logout       delete stored credentials
  version      print the version

flags:
  -socket PATH   control socket (default %s)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Printf(usage, ipc.DefaultSocketPath)
		os.Exit(2)
	}

	cmd, args := os.Args[1], os.Args[2:]
	var err error

	switch cmd {
	case "connect":
		err = cmdConnect(args)
	case "disconnect":
		err = cmdDisconnect(args)
	case "status":
		err = cmdStatus(args)
	case "login":
		err = cmdLogin(args)
	case "logout":
		err = cmdLogout(args)
	case "version", "-v", "--version":
		fmt.Println("myst", version)
	case "help", "-h", "--help":
		fmt.Printf(usage, ipc.DefaultSocketPath)
	default:
		fmt.Fprintf(os.Stderr, "myst: unknown command %q\n\n", cmd)
		fmt.Printf(usage, ipc.DefaultSocketPath)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "myst:", err)
		os.Exit(1)
	}
}

func cmdConnect(args []string) error {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	socket := fs.String("socket", "", "control socket path")
	country := fs.String("country", "", "exit country code, e.g. gb (default: let the API choose)")
	ipType := fs.String("ip-type", "", "ip type preference, e.g. hosting or residential")
	fs.Parse(args)

	c := ipc.Client{Path: *socket}
	st, err := c.Connect(context.Background(), *country, *ipType)
	if err != nil {
		if st != nil {
			printStatus(st)
		}
		return err
	}
	printStatus(st)
	return nil
}

func cmdDisconnect(args []string) error {
	fs := flag.NewFlagSet("disconnect", flag.ExitOnError)
	socket := fs.String("socket", "", "control socket path")
	fs.Parse(args)

	c := ipc.Client{Path: *socket}
	st, err := c.Disconnect(context.Background())
	if st != nil {
		printStatus(st)
	}
	return err
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	socket := fs.String("socket", "", "control socket path")
	asJSON := fs.Bool("json", false, "emit the raw status as JSON")
	fs.Parse(args)

	c := ipc.Client{Path: *socket}
	st, err := c.Status(context.Background())
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	printStatus(st)
	return nil
}

func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	from := fs.String("from", "-", "file holding a token JSON, or - for stdin")
	tokenPath := fs.String("token", auth.DefaultPath, "where to store credentials")
	fs.Parse(args)

	var raw []byte
	var err error
	if *from == "-" {
		raw, err = readAllStdin()
	} else {
		raw, err = os.ReadFile(*from)
	}
	if err != nil {
		return fmt.Errorf("read token: %w", err)
	}

	var tok auth.Token
	if err := json.Unmarshal(raw, &tok); err != nil {
		return fmt.Errorf("parse token: %w", err)
	}
	if tok.AccessToken == "" {
		return errors.New("that file has no access_token field")
	}
	if tok.RefreshToken == "" {
		// Without a refresh token the client dies when the access token expires,
		// which is about 30 days out and easy to miss. Say so now.
		fmt.Fprintln(os.Stderr, "warning: no refresh_token — this client will stop working when the access token expires")
	}

	store := auth.Store{Path: *tokenPath}
	if err := store.Save(&tok); err != nil {
		return fmt.Errorf("store credentials: %w", err)
	}
	fmt.Printf("credentials saved to %s\n", *tokenPath)
	return nil
}

func cmdLogout(args []string) error {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	tokenPath := fs.String("token", auth.DefaultPath, "credential store path")
	fs.Parse(args)

	if err := (auth.Store{Path: *tokenPath}).Delete(); err != nil {
		return err
	}
	fmt.Println("credentials removed")
	return nil
}

func printStatus(st *ipc.Status) {
	if st == nil {
		fmt.Println("unknown — the daemon returned no status")
		return
	}

	fmt.Println(st.State)

	if !st.Since.IsZero() && st.State == ipc.StateConnected {
		fmt.Printf("  up since    %s\n", time.Since(st.Since).Round(time.Second))
	}
	if st.ExitIP != "" {
		region := strings.Trim(strings.Join([]string{st.Country, st.City}, ", "), ", ")
		if region != "" {
			fmt.Printf("  exit ip     %s (%s)\n", st.ExitIP, region)
		} else {
			fmt.Printf("  exit ip     %s\n", st.ExitIP)
		}
	}
	if st.Interface != "" && st.State == ipc.StateConnected {
		fmt.Printf("  interface   %s\n", st.Interface)
	}
	if st.State == ipc.StateConnected {
		fmt.Printf("  ipv6        %s\n", yesNo(st.IPv6Routed, "routed", "NOT routed — IPv6 bypasses the tunnel"))
		fmt.Printf("  wake lock   %s\n", yesNo(st.WakeLock, "held", "not held — the machine can suspend"))
		fmt.Printf("  kill switch %s\n", yesNo(st.KillSwitch, "active", "not installed"))
	}
	if st.DeviceSlots > 0 {
		fmt.Printf("  device slots %d in use\n", st.DeviceSlots)
	}
	if st.LastError != "" {
		fmt.Printf("  last error  %s\n", st.LastError)
	}
}

func yesNo(v bool, yes, no string) string {
	if v {
		return yes
	}
	return no
}

func readAllStdin() ([]byte, error) {
	return io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
}
