package wg

import "testing"

func TestEndpointParsesBareIPv4(t *testing.T) {
	conf := `[Peer]
PublicKey = abc
Endpoint = 91.98.228.210:56666
AllowedIPs = 0.0.0.0/0
`
	host, port, err := Endpoint(conf)
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if host != "91.98.228.210" {
		t.Errorf("host = %q, want 91.98.228.210", host)
	}
	if port != 56666 {
		t.Errorf("port = %d, want 56666", port)
	}
}

func TestEndpointParsesBracketedIPv6(t *testing.T) {
	conf := "[Peer]\nEndpoint = [2001:db8::1]:51820\n"
	host, port, err := Endpoint(conf)
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if host != "2001:db8::1" {
		t.Errorf("host = %q, want 2001:db8::1", host)
	}
	if port != 51820 {
		t.Errorf("port = %d, want 51820", port)
	}
}

func TestEndpointAcceptsAHostname(t *testing.T) {
	// Mysterium hands back a bare IP, but the caller resolves whatever it gets,
	// so a hostname must parse rather than fail here.
	host, port, err := Endpoint("[Peer]\nEndpoint = node.example.com:51820\n")
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if host != "node.example.com" || port != 51820 {
		t.Errorf("got %q:%d, want node.example.com:51820", host, port)
	}
}

func TestEndpointRejectsMissingPort(t *testing.T) {
	if _, _, err := Endpoint("[Peer]\nEndpoint = 91.98.228.210\n"); err == nil {
		t.Fatal("expected an error for an endpoint with no port")
	}
}

func TestEndpointRejectsOutOfRangePort(t *testing.T) {
	if _, _, err := Endpoint("[Peer]\nEndpoint = 91.98.228.210:99999\n"); err == nil {
		t.Fatal("expected an error for an out-of-range port")
	}
}

func TestEndpointErrorsWhenAbsent(t *testing.T) {
	if _, _, err := Endpoint("[Peer]\nPublicKey = abc\n"); err == nil {
		t.Fatal("expected an error when the config has no Endpoint")
	}
}

func TestEndpointReadsTheRealTemplate(t *testing.T) {
	// The placeholder template the live API returns, as recorded in reference/.
	conf := `[Interface]
PrivateKey = %private_key%
Address = 10.181.86.246/32
DNS = 1.1.1.1

[Peer]
PublicKey = BWIx1BlaS5iZ5C+eYlONhfL6H749GebmdcEPLaphJzY=
AllowedIPs = 0.0.0.0/0
Endpoint = 91.98.228.210:56666
PersistentKeepalive = 25
`
	host, port, err := Endpoint(conf)
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if host != "91.98.228.210" || port != 56666 {
		t.Errorf("got %q:%d, want 91.98.228.210:56666", host, port)
	}
}
