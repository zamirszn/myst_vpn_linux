// Command myst is the unprivileged control CLI. It talks to mystd over a unix
// socket and never touches the network or the tunnel directly.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zamirszn/mysterium-linux-client/internal/auth"
	"github.com/zamirszn/mysterium-linux-client/internal/daemon"
	"github.com/zamirszn/mysterium-linux-client/internal/gui"
	"github.com/zamirszn/mysterium-linux-client/internal/ipc"
)

var version = "0.1.0"

const usage = `myst — Mysterium VPN 2.0 client for Linux

usage: myst <command> [flags]

commands:
  connect      bring the tunnel up
  disconnect   tear the tunnel down
  status       report the current state
  countries    list the countries this account can connect to
  locations    list the countries and cities this account can connect to
  login        log in with an emailed link, or import a captured token
  logout       log out: close any tunnel and delete stored credentials
  version      print the version

flags:
  -socket PATH   control socket (default %s)

internal:
  --daemon       run the privileged daemon (normally started automatically)
`

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--daemon" {
		if err := daemon.RunServer(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "myst daemon:", err)
			os.Exit(1)
		}
		return
	}

	if len(os.Args) < 2 {
		os.Exit(gui.Run(nil))
	}

	// The desktop application is the default experience. The CLI commands are
	// retained for automation/backwards compatibility, but login/logout are no
	// longer required for normal desktop use.
	if os.Args[1] == "--gui" {
		os.Exit(gui.Run(os.Args[2:]))
	}

	cmd, args := os.Args[1], os.Args[2:]
	if cmd != "help" && cmd != "-h" && cmd != "--help" && cmd != "version" && cmd != "-v" && cmd != "--version" {
		if err := ensureDaemon(args); err != nil {
			fmt.Fprintln(os.Stderr, "myst:", err)
			os.Exit(1)
		}
	}

	var err error

	switch cmd {
	case "connect":
		err = cmdConnect(args)
	case "disconnect":
		err = cmdDisconnect(args)
	case "status":
		err = cmdStatus(args)
	case "countries":
		err = cmdCountries(args)
	case "locations":
		err = cmdLocations(args)
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

func ensureDaemon(args []string) error {
	// A caller can still explicitly point the CLI at another daemon. In that
	// case ownership/lifecycle of the custom socket remains with the caller.
	if customSocket(args) != "" {
		return nil
	}

	client := ipc.Client{Path: ipc.DefaultSocketPath}
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	err := client.Ping(ctx)
	cancel()
	if err == nil {
		return nil
	}

	if os.Geteuid() == 0 {
		return startDaemonDirect()
	}
	return startDaemonWithSudo()
}

func customSocket(args []string) string {
	for i := range args {
		if args[i] == "-socket" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(args[i], "-socket=") {
			return strings.TrimPrefix(args[i], "-socket=")
		}
	}
	return ""
}

func startDaemonDirect() error {
	return spawnDaemon([]string{"--daemon"})
}

func startDaemonWithSudo() error {
	// Let sudo handle the interactive password prompt once, then start the
	// actual daemon non-interactively and detached. This keeps the password out
	// of the application and works with normal sudoers installations.
	check := exec.Command("sudo", "-v")
	check.Stdin = os.Stdin
	check.Stdout = os.Stdout
	check.Stderr = os.Stderr
	if err := check.Run(); err != nil {
		return fmt.Errorf("administrator authentication failed: %w", err)
	}

	group := primaryGroup()
	args := []string{"--daemon"}
	if group != "" {
		args = append(args, "-socket-group", group)
	}
	return spawnDaemon([]string{"sudo", "-n"}, args...)
}

func spawnDaemon(prefix []string, daemonArgs ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find myst executable: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return fmt.Errorf("resolve myst executable: %w", err)
	}

	cmdArgs := append([]string{exe}, daemonArgs...)
	var cmd *exec.Cmd
	if len(prefix) == 0 {
		cmd = exec.Command(exe, daemonArgs...)
	} else {
		cmd = exec.Command(prefix[0], append(prefix[1:], cmdArgs...)...)
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open daemon stdio: %w", err)
	}
	defer devNull.Close()
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for {
		if err := (ipc.Client{Path: ipc.DefaultSocketPath}).Ping(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("daemon did not become ready; run './myst --daemon -v' to inspect startup errors")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func primaryGroup() string {
	current, err := user.Current()
	if err != nil {
		return ""
	}
	group, err := user.LookupGroupId(current.Gid)
	if err == nil && group.Name != "" {
		return group.Name
	}
	return current.Gid
}

func cmdConnect(args []string) error {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	socket := fs.String("socket", "", "control socket path")
	country := fs.String("country", "", "exit country code, e.g. gb (default: let the API choose)")
	city := fs.String("city", "", "exit city slug, e.g. london (default: any city in the country)")
	ipType := fs.String("ip-type", "", "ip type preference, e.g. hosting or residential")
	fs.Parse(args)

	c := ipc.Client{Path: *socket}
	st, err := c.Connect(context.Background(), ipc.Target{
		Country: *country,
		City:    *city,
		IPType:  *ipType,
	})
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

// cmdCountries prints the countries the account can connect to, one code per
// line so the output pipes.
//
// The list comes from the daemon, not from here: it needs the access token, and
// the token belongs to the daemon. It is also the account's own list, which is
// about a hundred countries — far more than the desktop front ends carry as a
// fallback for when the daemon cannot be reached.
func cmdCountries(args []string) error {
	fs := flag.NewFlagSet("countries", flag.ExitOnError)
	socket := fs.String("socket", "", "control socket path")
	asJSON := fs.Bool("json", false, "emit the raw list as JSON")
	fs.Parse(args)

	c := ipc.Client{Path: *socket}
	codes, err := c.Countries(context.Background())
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(codes)
	}
	for _, code := range codes {
		fmt.Println(code)
	}
	return nil
}

// cmdLocations prints the countries and cities the account can exit through.
//
// The API serves this as a single list of countries, each carrying its cities,
// which is what the picker needs — a country with no cities is still a valid
// choice, so the two lists cannot be flattened into one.
func cmdLocations(args []string) error {
	fs := flag.NewFlagSet("locations", flag.ExitOnError)
	socket := fs.String("socket", "", "control socket path")
	ipType := fs.String("ip-type", "", "residential (default) or hosting")
	asJSON := fs.Bool("json", false, "emit the raw list as JSON")
	citiesOnly := fs.Bool("cities", false, "print country and city slugs only, one per line")
	fs.Parse(args)

	c := ipc.Client{Path: *socket}
	locs, err := c.Locations(context.Background(), *ipType)
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(locs)
	}

	for _, country := range locs {
		if *citiesOnly {
			for _, city := range country.Cities {
				fmt.Printf("%s\t%s\n", country.Code, city.Slug)
			}
			continue
		}
		fmt.Printf("%s  %s", country.Code, country.Name)
		if n := len(country.Cities); n > 0 {
			fmt.Printf("  (%d cities)", n)
		}
		fmt.Println()
	}
	return nil
}

// cmdLogin logs in, by default the way the official app does: an email address,
// a link sent to it, and the link pasted back. The daemon runs the exchange and
// keeps the secret that makes the link redeemable; this only carries words.
//
// --from imports a token captured some other way, and --token writes the file
// directly instead of going through the daemon. Both predate the emailed link
// and are kept for the cases it does not cover: no mailbox to hand, or a daemon
// that is not running.
func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	socket := fs.String("socket", "", "control socket path")
	email := fs.String("email", "", "address to send the login link to (default: ask)")
	from := fs.String("from", "", "import a token from this JSON file, or - for stdin, instead of emailing a link")
	tokenPath := fs.String("token", "", "write the credentials to this file directly instead of through the daemon (needs write access to it)")
	fs.Parse(args)

	if *from != "" {
		return loginFromFile(*from, *tokenPath, *socket)
	}
	if *tokenPath != "" {
		return errors.New("-token only applies with -from: an emailed link is redeemed by the daemon, which stores it itself")
	}
	return loginByEmail(*email, *socket)
}

// loginByEmail is the emailed-link flow.
func loginByEmail(email, socket string) error {
	in := bufio.NewReader(os.Stdin)

	if strings.TrimSpace(email) == "" {
		fmt.Fprint(os.Stderr, "email: ")
		line, err := readLine(in)
		if err != nil {
			return fmt.Errorf("read email: %w", err)
		}
		email = line
	}
	email = strings.TrimSpace(email)

	c := ipc.Client{Path: socket}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if _, err := c.LoginStart(ctx, email); err != nil {
		return err
	}
	fmt.Printf("A login link is on its way to %s.\n\n", email)
	fmt.Println("Open the email and paste its link here (or just the code from it).")
	fmt.Println("The link only works for a few minutes.")
	fmt.Println()

	// A few tries at the paste, because the likeliest mistake is pasting the
	// wrong line, and the daemon keeps the login waiting through it. Anything
	// else — a rejected or expired link — ends it, since asking again with the
	// same link cannot help.
	const tries = 3
	for attempt := 1; ; attempt++ {
		fmt.Fprint(os.Stderr, "link: ")
		pasted, err := readLine(in)
		if err != nil {
			return fmt.Errorf("read link: %w", err)
		}

		st, err := c.LoginFinish(ctx, pasted)
		if err == nil {
			if st != nil && st.Account != "" {
				fmt.Printf("logged in as %s\n", st.Account)
			} else {
				fmt.Println("logged in")
			}
			return nil
		}
		if attempt < tries && strings.Contains(err.Error(), "did not contain a login code") {
			fmt.Fprintln(os.Stderr, "myst:", err)
			continue
		}
		return err
	}
}

// loginFromFile imports a token captured elsewhere.
func loginFromFile(from, tokenPath, socket string) error {
	var raw []byte
	var err error
	if from == "-" {
		raw, err = readAllStdin()
	} else {
		raw, err = os.ReadFile(from)
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

	if tokenPath != "" {
		if err := (auth.Store{Path: tokenPath}).Save(&tok); err != nil {
			return fmt.Errorf("store credentials: %w", err)
		}
		fmt.Printf("credentials saved to %s\n", tokenPath)
		return nil
	}

	// Through the daemon, which owns the credential file and closes any tunnel
	// that was opened on the account being replaced.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := (ipc.Client{Path: socket}).Login(ctx, raw); err != nil {
		return err
	}
	fmt.Println("credentials stored")
	return nil
}

func cmdLogout(args []string) error {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	socket := fs.String("socket", "", "control socket path")
	tokenPath := fs.String("token", "", "delete this credential file directly instead of asking the daemon (needs write access to it)")
	fs.Parse(args)

	if *tokenPath != "" {
		if err := (auth.Store{Path: *tokenPath}).Delete(); err != nil {
			return err
		}
		fmt.Println("credentials removed")
		return nil
	}

	// Through the daemon: it closes a live tunnel while the credentials that
	// opened it are still on disk, so the account's connection is released
	// instead of left to time out, and it drops its cached country lists.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := (ipc.Client{Path: *socket}).Logout(ctx); err != nil {
		return err
	}
	fmt.Println("logged out")
	return nil
}

// readLine reads one line of input. A final line with no newline is still a
// line; only an empty read at end of input is an error.
func readLine(in *bufio.Reader) (string, error) {
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
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
	switch {
	case !st.LoggedIn:
		fmt.Println("  account     not logged in — run: myst login")
	case st.Account != "":
		fmt.Printf("  account     %s\n", st.Account)
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
