package killswitch

import (
	"os/exec"
	"strings"
	"testing"
)

// validateWithNft runs a ruleset through the real nft parser.
//
// nft cannot *load* a ruleset without root, but it still parses and reports
// syntax errors, which is the part worth checking: a ruleset that only fails at
// apply time would fail on a user's machine at the worst possible moment.
func validateWithNft(t *testing.T, ruleset string) {
	t.Helper()
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft is not installed")
	}

	cmd := exec.Command("nft", "-c", "-f", "-")
	cmd.Stdin = strings.NewReader(ruleset)
	out, _ := cmd.CombinedOutput()

	var errs []string
	for _, line := range strings.Split(string(out), "\n") {
		// The netlink cache failure is an artefact of running unprivileged; it
		// says nothing about the ruleset.
		if strings.Contains(line, "netlink") {
			continue
		}
		if strings.Contains(line, "Error:") {
			errs = append(errs, line)
		}
	}
	if len(errs) > 0 {
		t.Errorf("nft rejected the generated ruleset:\n  %s\n\nruleset:\n%s",
			strings.Join(errs, "\n  "), ruleset)
	}
}

func spec() Spec {
	return Spec{
		Interface: "myst0",
		Endpoints: []Endpoint{{IP: "91.98.228.210", Port: 56666}},
	}
}

func mustRuleset(t *testing.T, s Spec) string {
	t.Helper()
	r, err := Ruleset(s)
	if err != nil {
		t.Fatalf("Ruleset: %v", err)
	}
	return r
}

func TestRulesetParsesCleanly(t *testing.T) {
	validateWithNft(t, mustRuleset(t, spec()))
}

func TestRulesetParsesCleanlyForIPv6Endpoint(t *testing.T) {
	validateWithNft(t, mustRuleset(t, Spec{
		Interface: "myst0",
		Endpoints: []Endpoint{{IP: "2001:db8::1", Port: 51820}},
	}))
}

func TestRulesetParsesCleanlyWithMultipleEndpoints(t *testing.T) {
	validateWithNft(t, mustRuleset(t, Spec{
		Interface: "myst0",
		Endpoints: []Endpoint{
			{IP: "91.98.228.210", Port: 56666},
			{IP: "203.0.113.9", Port: 51820},
			{IP: "2001:db8::1", Port: 56666},
		},
	}))
}

func TestRulesetAllowsTheTunnelAndLoopback(t *testing.T) {
	r := mustRuleset(t, spec())
	for _, want := range []string{`oifname "lo" accept`, `oifname "myst0" accept`} {
		if !strings.Contains(r, want) {
			t.Errorf("ruleset does not contain %q:\n%s", want, r)
		}
	}
}

func TestRulesetAllowsTheHandshakeEndpoint(t *testing.T) {
	r := mustRuleset(t, spec())
	if !strings.Contains(r, "ip daddr 91.98.228.210 udp dport 56666 accept") {
		t.Errorf("ruleset does not permit the handshake endpoint:\n%s", r)
	}
}

func TestRulesetUsesTheRightFamilyForEachEndpoint(t *testing.T) {
	// The bug this guards against is emitting `ip daddr` for a v6 address,
	// which nft accepts as a hostname and then fails to resolve at apply time.
	r := mustRuleset(t, Spec{
		Interface: "myst0",
		Endpoints: []Endpoint{
			{IP: "91.98.228.210", Port: 1},
			{IP: "2001:db8::1", Port: 2},
		},
	})
	if !strings.Contains(r, "ip daddr 91.98.228.210 udp dport 1 accept") {
		t.Errorf("IPv4 endpoint rule is missing or wrong:\n%s", r)
	}
	if !strings.Contains(r, "ip6 daddr 2001:db8::1 udp dport 2 accept") {
		t.Errorf("IPv6 endpoint rule is missing or wrong:\n%s", r)
	}
}

func TestRulesetHonoursEachEndpointsOwnPort(t *testing.T) {
	r := mustRuleset(t, Spec{
		Interface: "myst0",
		Endpoints: []Endpoint{
			{IP: "91.98.228.210", Port: 56666},
			{IP: "203.0.113.9", Port: 51820},
		},
	})
	if !strings.Contains(r, "udp dport 56666") || !strings.Contains(r, "udp dport 51820") {
		t.Errorf("endpoints did not keep their own ports:\n%s", r)
	}
}

func TestRulesetDropsByDefault(t *testing.T) {
	r := mustRuleset(t, spec())
	if !strings.Contains(r, "counter drop") {
		t.Errorf("ruleset has no default drop:\n%s", r)
	}
}

func TestRulesetDeliberatelyOmitsEstablishedAccept(t *testing.T) {
	// A blanket `ct state established,related accept` would let connections
	// opened before the switch was armed keep flowing out of the physical
	// interface — exactly the leak the kill switch exists to prevent. If this
	// test ever fails, someone has reintroduced it.
	//
	// Comments are stripped first, so this asserts about the rules rather than
	// about the prose that explains them.
	r := stripComments(mustRuleset(t, spec()))
	if strings.Contains(r, "ct ") {
		t.Errorf("ruleset accepts established connections, which reopens the leak:\n%s", r)
	}
}

// stripComments removes nft comments so assertions test the rules themselves.
func stripComments(ruleset string) string {
	var b strings.Builder
	for _, line := range strings.Split(ruleset, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func TestRulesetRefusesEmptyEndpoints(t *testing.T) {
	// Without an endpoint rule the handshake would be blocked too, so the
	// tunnel could never come back and the machine would be left offline.
	if _, err := Ruleset(Spec{Interface: "myst0"}); err == nil {
		t.Fatal("expected an error when arming with no allowed endpoints")
	}
}

func TestRulesetRefusesEmptyInterface(t *testing.T) {
	if _, err := Ruleset(Spec{Endpoints: []Endpoint{{IP: "91.98.228.210", Port: 1}}}); err == nil {
		t.Fatal("expected an error when no tunnel interface is given")
	}
}

func TestRulesetRejectsABadAddress(t *testing.T) {
	_, err := Ruleset(Spec{
		Interface: "myst0",
		Endpoints: []Endpoint{{IP: "not-an-ip", Port: 1}},
	})
	if err == nil {
		t.Fatal("expected an error for a non-IP endpoint")
	}
}

func TestRulesetRejectsABadPort(t *testing.T) {
	for _, port := range []int{0, -1, 70000} {
		_, err := Ruleset(Spec{
			Interface: "myst0",
			Endpoints: []Endpoint{{IP: "91.98.228.210", Port: port}},
		})
		if err == nil {
			t.Errorf("expected an error for port %d", port)
		}
	}
}
