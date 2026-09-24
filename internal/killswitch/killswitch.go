// Package killswitch installs an nftables egress filter so that traffic cannot
// escape outside the tunnel while the tunnel is down.
//
// Design notes that matter more than the code:
//
//   - It lives in its own table, `inet myst_killswitch`. Nothing the user
//     already has — ufw, Docker, hand-written rules — is read or modified. The
//     whole thing is created and destroyed as a unit.
//   - It filters the *output* hook only, so it governs host-originated traffic.
//     Traffic forwarded on behalf of containers does not traverse that hook and
//     is not covered; see the README.
//   - It fails closed. The rules go in before the interface comes up, so there
//     is no window in which traffic could slip out, and they come off only
//     after the interface is down.
//
// nftables rather than iptables because the ruleset is one atomic object that
// can be deleted in a single command — which is also the documented escape
// hatch if a daemon is killed while the switch is armed.
package killswitch

import (
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
)

// TableName is the nftables table this package owns. Deleting it by hand is the
// escape hatch: `sudo nft delete table inet myst_killswitch`.
const TableName = "myst_killswitch"

// ToolMissingError reports that nft is not installed.
type ToolMissingError struct{}

func (e *ToolMissingError) Error() string {
	return "nft not found — install nftables, or run the daemon with -no-killswitch"
}

// Endpoint is a destination that must stay reachable while the switch is armed,
// so the tunnel can always re-establish itself.
type Endpoint struct {
	IP   string
	Port int
}

// Spec describes the filter to install.
type Spec struct {
	// Interface is the tunnel interface, which is always allowed.
	Interface string

	// Endpoints are the WireGuard handshake destinations, allowed over any
	// interface.
	Endpoints []Endpoint
}

// Ruleset renders the nftables ruleset for a spec.
//
// Exported and pure so it can be tested, and checked against the real nft
// parser rather than only against expectations about its text.
func Ruleset(spec Spec) (string, error) {
	if spec.Interface == "" {
		return "", fmt.Errorf("killswitch: no tunnel interface given")
	}
	// Refusing here matters: a ruleset with no endpoint rule would block the
	// WireGuard handshake too, so the switch could never be turned back off by
	// reconnecting, and the machine would sit with no network until someone
	// found the escape hatch.
	if len(spec.Endpoints) == 0 {
		return "", fmt.Errorf("killswitch: refusing to arm with no allowed endpoints — that would block the handshake itself")
	}

	// Validate everything before emitting, so a bad value cannot reach the
	// parser as part of an otherwise-valid ruleset.
	type rule struct {
		family string
		ip     string
		port   int
	}
	rules := make([]rule, 0, len(spec.Endpoints))

	for _, ep := range spec.Endpoints {
		addr, err := netip.ParseAddr(ep.IP)
		if err != nil {
			return "", fmt.Errorf("killswitch: endpoint %q is not an IP address: %w", ep.IP, err)
		}
		if ep.Port < 1 || ep.Port > 65535 {
			return "", fmt.Errorf("killswitch: endpoint port %d is out of range", ep.Port)
		}
		family := "ip6"
		if addr.Is4() {
			family = "ip"
		}
		rules = append(rules, rule{family: family, ip: addr.String(), port: ep.Port})
	}

	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s {\n", TableName)
	fmt.Fprintf(&b, "\tchain output {\n")
	fmt.Fprintf(&b, "\t\ttype filter hook output priority 0; policy accept;\n\n")

	fmt.Fprintf(&b, "\t\t# Loopback and the tunnel itself.\n")
	fmt.Fprintf(&b, "\t\toifname \"lo\" accept\n")
	fmt.Fprintf(&b, "\t\toifname %q accept\n\n", spec.Interface)

	fmt.Fprintf(&b, "\t\t# The WireGuard handshake has to stay reachable, or the tunnel\n")
	fmt.Fprintf(&b, "\t\t# could never come back after a drop.\n")
	for _, r := range rules {
		fmt.Fprintf(&b, "\t\t%s daddr %s udp dport %d accept\n", r.family, r.ip, r.port)
	}
	fmt.Fprintf(&b, "\n")

	fmt.Fprintf(&b, "\t\t# DHCP renewal. Without a lease the machine loses its address and\n")
	fmt.Fprintf(&b, "\t\t# the endpoint becomes unreachable.\n")
	fmt.Fprintf(&b, "\t\tudp sport 68 udp dport 67 accept\n")
	fmt.Fprintf(&b, "\t\tudp sport 546 udp dport 547 accept\n\n")

	fmt.Fprintf(&b, "\t\t# ICMPv6 carries neighbour discovery and router advertisements;\n")
	fmt.Fprintf(&b, "\t\t# dropping it breaks IPv6 on the physical link entirely.\n")
	fmt.Fprintf(&b, "\t\tmeta l4proto ipv6-icmp accept\n\n")

	fmt.Fprintf(&b, "\t\t# Deliberately no `ct state established,related accept`.\n")
	fmt.Fprintf(&b, "\t\t# Connections opened before the switch was armed would otherwise\n")
	fmt.Fprintf(&b, "\t\t# keep flowing out of the physical interface, which is precisely\n")
	fmt.Fprintf(&b, "\t\t# the leak this exists to prevent.\n")
	fmt.Fprintf(&b, "\t\tcounter drop\n")
	fmt.Fprintf(&b, "\t}\n}\n")

	return b.String(), nil
}

// Nft is the nftables backend.
type Nft struct{}

// Install replaces any existing filter with the one described by spec.
func (n Nft) Install(ctx context.Context, spec Spec) error {
	if _, err := exec.LookPath("nft"); err != nil {
		return &ToolMissingError{}
	}
	ruleset, err := Ruleset(spec)
	if err != nil {
		return err
	}

	// Clear first so the result does not depend on what a previous run left
	// behind, and so a ruleset that fails to parse cannot leave a half-applied
	// mixture of old and new rules.
	if err := n.Remove(ctx); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(ruleset)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft apply: %s", describe(out, err))
	}
	return nil
}

// Remove deletes the table, which is the whole filter.
func (n Nft) Remove(ctx context.Context) error {
	if _, err := exec.LookPath("nft"); err != nil {
		// Without nft we cannot have armed anything, so the desired end state
		// already holds. Returning nil keeps teardown robust on hosts that
		// never had the kill switch enabled.
		return nil
	}

	cmd := exec.CommandContext(ctx, "nft", "delete", "table", "inet", TableName)
	out, err := cmd.CombinedOutput()
	if err != nil && !isAbsent(out) {
		return fmt.Errorf("nft delete table: %s", describe(out, err))
	}
	return nil
}

// Active reports whether the filter is currently installed.
func (n Nft) Active(ctx context.Context) (bool, error) {
	if _, err := exec.LookPath("nft"); err != nil {
		return false, nil
	}

	cmd := exec.CommandContext(ctx, "nft", "list", "table", "inet", TableName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if isAbsent(out) {
			return false, nil
		}
		// Anything else — permission denied, most likely — must not be
		// reported as "not installed", because a caller would then believe
		// traffic is unprotected when it may well be filtered.
		return false, fmt.Errorf("nft list table: %s", describe(out, err))
	}
	return true, nil
}

// isAbsent reports whether nft failed because the table does not exist, which
// is a normal state rather than an error.
func isAbsent(out []byte) bool {
	s := strings.ToLower(string(out))
	return strings.Contains(s, "no such file or directory") || strings.Contains(s, "does not exist")
}

func describe(out []byte, err error) string {
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return msg
	}
	return err.Error()
}
