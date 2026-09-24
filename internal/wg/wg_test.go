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
