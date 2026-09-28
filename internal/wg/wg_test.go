package wg

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestGenerateKeyPairIsConsistentAndClamped(t *testing.T) {
	kp, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}

	raw, err := base64.StdEncoding.DecodeString(kp.PrivateKey)
	if err != nil {
		t.Fatalf("private key is not valid base64: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("private key is %d bytes, want 32", len(raw))
	}

	// X25519 clamping, exactly as `wg genkey` produces. If this drifts, the
	// public key we register with the API stops matching the private key
	// wg-quick loads, and the handshake fails with no obvious cause.
	if raw[0]&7 != 0 {
		t.Errorf("low 3 bits of byte 0 not cleared: %#x", raw[0])
	}
	if raw[31]&128 != 0 {
		t.Errorf("high bit of byte 31 not cleared: %#x", raw[31])
	}
	if raw[31]&64 != 64 {
		t.Errorf("bit 6 of byte 31 not set: %#x", raw[31])
	}

	derived, err := PublicKeyFrom(kp.PrivateKey)
	if err != nil {
		t.Fatalf("PublicKeyFrom: %v", err)
	}
	if derived != kp.PublicKey {
		t.Errorf("public key does not derive from private key:\n got %s\nwant %s", derived, kp.PublicKey)
	}
}

func TestGenerateKeyPairIsNotReused(t *testing.T) {
	a, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if a.PrivateKey == b.PrivateKey || a.PublicKey == b.PublicKey {
		t.Fatal("two generated keypairs are identical — entropy source is broken")
	}
}

func TestPublicKeyFromRejectsBadInput(t *testing.T) {
	for name, in := range map[string]string{
		"not base64": "!!!!",
		"too short":  base64.StdEncoding.EncodeToString([]byte("short")),
		"empty":      "",
	} {
		if _, err := PublicKeyFrom(in); err == nil {
			t.Errorf("%s: expected an error, got nil", name)
		}
	}
}

const template = `[Interface]
PrivateKey = %private_key%
Address = 10.181.73.237, fe80::1234:5678:90ab:cdef/128
DNS = 1.1.1.1

[Peer]
PublicKey = BWIx1BlaS5iZ5C+eYlONhfL6H749GebmdcEPLaphJzY=
AllowedIPs = 0.0.0.0/1, 128.0.0.0/1
Endpoint = 91.98.228.210:56666
PersistentKeepalive = 25
`

func TestAssembleSubstitutesLocally(t *testing.T) {
	kp, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Assemble(template, kp)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if strings.Contains(got, PrivateKeyPlaceholder) {
		t.Error("placeholder survived assembly")
	}
	if !strings.Contains(got, kp.PrivateKey) {
		t.Error("private key was not substituted into the config")
	}
	if err := Validate(got); err != nil {
		t.Errorf("assembled config failed validation: %v", err)
	}
}

func TestAssembleRefusesWithoutPlaceholder(t *testing.T) {
	// A template with no placeholder means the server generated the key itself,
	// so the public key we registered would not match. Fail loudly.
	kp, _ := GenerateKeyPair()
	if _, err := Assemble("[Interface]\nPrivateKey = abc\n", kp); err == nil {
		t.Fatal("expected an error when the placeholder is absent, got nil")
	}
}

func TestValidateCatchesMissingPieces(t *testing.T) {
	cases := map[string]string{
		"no interface":  "[Peer]\nPublicKey = x\nAllowedIPs = 0.0.0.0/1\nEndpoint = 1.2.3.4:1\n",
		"no peer":       "[Interface]\nPrivateKey = x\nAddress = 10.0.0.1\n",
		"no endpoint":   "[Interface]\nPrivateKey = x\nAddress = 10.0.0.1\n\n[Peer]\nPublicKey = y\nAllowedIPs = 0.0.0.0/1\n",
		"empty private": "[Interface]\nPrivateKey =\nAddress = 10.0.0.1\n\n[Peer]\nPublicKey = y\nAllowedIPs = 0.0.0.0/1\nEndpoint = 1.2.3.4:1\n",
	}
	for name, conf := range cases {
		if err := Validate(conf); err == nil {
			t.Errorf("%s: expected a validation error, got nil", name)
		}
	}
	if err := Validate(template); err != nil {
		t.Errorf("the reference template should validate: %v", err)
	}
}

func TestEnsureIPv6Route(t *testing.T) {
	if RoutesIPv6(template) {
		t.Fatal("reference config should report no IPv6 route")
	}
	fixed := EnsureIPv6Route(template)
	if !RoutesIPv6(fixed) {
		t.Error("EnsureIPv6Route did not add ::/0")
	}
	if !strings.Contains(fixed, "AllowedIPs = 0.0.0.0/1, 128.0.0.0/1, ::/0") {
		t.Errorf("::/0 was not appended to AllowedIPs:\n%s", fixed)
	}
	// Idempotent, so calling it on every start is safe.
	if twice := EnsureIPv6Route(fixed); twice != fixed {
		t.Error("EnsureIPv6Route is not idempotent")
	}
}

func TestRedactRemovesKeyMaterial(t *testing.T) {
	kp, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	assembled, err := Assemble(template, kp)
	if err != nil {
		t.Fatal(err)
	}

	redacted := Redact(assembled)
	if strings.Contains(redacted, kp.PrivateKey) {
		t.Error("redacted output contains the private key")
	}
	// The peer's public key is not secret, but it is key material and there is
	// no reason to print it in full.
	if strings.Contains(redacted, "BWIx1BlaS5iZ5C+eYlONhfL6H749GebmdcEPLaphJzY=") {
		t.Error("redacted output contains the peer public key in full")
	}
	// Structure must survive — a redacted config still has to be diagnosable.
	for _, want := range []string{"[Interface]", "[Peer]", "Endpoint = 91.98.228.210:56666", "DNS = 1.1.1.1"} {
		if !strings.Contains(redacted, want) {
			t.Errorf("redaction removed structural line %q", want)
		}
	}
	if !strings.Contains(redacted, "2 CIDRs") {
		t.Errorf("AllowedIPs should be summarised, got:\n%s", redacted)
	}
}

// TestEnsureKeepaliveAddsOneWhenTheTemplateHasNone covers the second half of
// the bug that made the client disconnect itself every couple of minutes.
//
// The server's template does not always carry a PersistentKeepalive, and the
// original code passed it through untouched. Without one, a client behind NAT
// stops being reachable the moment the mapping goes idle: the peer has no
// reason to send anything, so no new handshake is ever initiated, and the
// health monitor's stale-handshake check eventually fires on a tunnel that was
// working until the silence began.
func TestEnsureKeepaliveAddsOneWhenTheTemplateHasNone(t *testing.T) {
	without := strings.Replace(template, "PersistentKeepalive = 25\n", "", 1)
	if HasKeepalive(without) {
		t.Fatal("the fixture still has a keepalive, so this proves nothing")
	}

	got := EnsureKeepalive(without)
	if !HasKeepalive(got) {
		t.Fatal("EnsureKeepalive did not add one")
	}
	if n := strings.Count(got, "PersistentKeepalive"); n != 1 {
		t.Errorf("PersistentKeepalive appears %d times, want exactly 1", n)
	}

	// Inside the [Peer] section, not after it. wg-quick parses this file by
	// section, so a line that lands past the end of [Peer] is not a peer setting
	// at all — it is silently ignored, and the tunnel behaves exactly as if it
	// had never been written.
	//
	// The fixture has a trailing comment so that "inside the section" and "at
	// the end of the file" are different places; with [Peer] last they are the
	// same place and this would pass either way.
	withTail := without + "\n# end\n"
	got = EnsureKeepalive(withTail)
	if !strings.HasSuffix(got, "# end\n") {
		t.Errorf("the trailing content moved, so the line was not inserted in place:\n%s", got)
	}
	lines := strings.Split(got, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "PersistentKeepalive") {
			if i == 0 || strings.TrimSpace(lines[i-1]) != "[Peer]" {
				t.Errorf("PersistentKeepalive is at line %d, not directly inside the [Peer] section:\n%s", i, got)
			}
		}
	}

	// And the result still validates, which is the property that actually
	// matters: an injected line must not break the config.
	if err := Validate(got); err != nil {
		t.Errorf("the config no longer validates after injection: %v", err)
	}
}

// TestEnsureKeepaliveLeavesAnExistingOneAlone keeps the function idempotent.
// Two PersistentKeepalive lines in one [Peer] is a config wg-quick rejects, so
// a second pass over an already-patched config must be a no-op.
func TestEnsureKeepaliveLeavesAnExistingOneAlone(t *testing.T) {
	if !HasKeepalive(template) {
		t.Fatal("the fixture has no keepalive to preserve")
	}
	got := EnsureKeepalive(template)
	if got != template {
		t.Errorf("a config that already sets PersistentKeepalive was rewritten:\n%s", got)
	}
	if n := strings.Count(got, "PersistentKeepalive"); n != 1 {
		t.Errorf("PersistentKeepalive appears %d times, want 1", n)
	}
}

// TestEnsureKeepaliveDoesNotInventAPeer guards the case where there is no
// [Peer] to put it in. Appending the line to such a config would produce
// something wg-quick reads as a stray key, which is worse than leaving it.
func TestEnsureKeepaliveDoesNotInventAPeer(t *testing.T) {
	noPeer := "[Interface]\nPrivateKey = aGVsbG8=\nAddress = 10.0.0.2/32\n"
	got := EnsureKeepalive(noPeer)
	if got != noPeer {
		t.Errorf("EnsureKeepalive changed a config with no [Peer] section:\n%s", got)
	}
}
