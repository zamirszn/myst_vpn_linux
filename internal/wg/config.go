package wg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PrivateKeyPlaceholder is the marker the API leaves in wg_config where the
// caller is expected to insert its own private key.
const PrivateKeyPlaceholder = "%private_key%"

// Assemble inserts the locally generated private key into the server's template.
//
// We refuse to proceed if the placeholder is absent rather than writing a config
// whose PrivateKey silently does not match the public key we registered — that
// failure mode is a tunnel that handshakes never complete on, with no clear cause.
func Assemble(template string, kp KeyPair) (string, error) {
	if !strings.Contains(template, PrivateKeyPlaceholder) {
		return "", fmt.Errorf(
			"template contains no %s placeholder: the server may have generated the key itself, "+
				"in which case the public key we registered would not match",
			PrivateKeyPlaceholder)
	}
	return strings.Replace(template, PrivateKeyPlaceholder, kp.PrivateKey, 1), nil
}

// Validate checks the assembled config for the fields wg-quick requires.
func Validate(conf string) error {
	var iface, peer bool
	for _, line := range strings.Split(conf, "\n") {
		switch strings.TrimSpace(line) {
		case "[Interface]":
			iface = true
		case "[Peer]":
			peer = true
		}
	}
	if !iface {
		return fmt.Errorf("config has no [Interface] section")
	}
	if !peer {
		return fmt.Errorf("config has no [Peer] section")
	}

	required := map[string]string{
		"PrivateKey": "PrivateKey",
		"Address":    "Address",
		"PublicKey":  "PublicKey",
		"AllowedIPs": "AllowedIPs",
		"Endpoint":   "Endpoint",
	}
	for field, desc := range required {
		if !hasField(conf, field) {
			return fmt.Errorf("config is missing %s", desc)
		}
	}
	return nil
}

// RoutesIPv6 reports whether the config routes IPv6 through the tunnel.
//
// A Mysterium WireGuard config assigns an IPv6 address but routes no IPv6. On a
// host with IPv6 connectivity that traffic bypasses the tunnel entirely — a silent
// leak. Callers should treat false as something to warn about or correct.
func RoutesIPv6(conf string) bool {
	return strings.Contains(conf, "::/0")
}

// EnsureIPv6Route appends ::/0 to the peer's AllowedIPs so IPv6 is carried too.
func EnsureIPv6Route(conf string) string {
	if RoutesIPv6(conf) {
		return conf
	}
	lines := strings.Split(conf, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "AllowedIPs") {
			lines[i] = strings.TrimRight(line, " \t") + ", ::/0"
			return strings.Join(lines, "\n")
		}
	}
	return conf
}

func hasField(conf, field string) bool {
	for _, line := range strings.Split(conf, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, field) {
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, field))
			if strings.HasPrefix(rest, "=") && strings.TrimSpace(strings.TrimPrefix(rest, "=")) != "" {
				return true
			}
		}
	}
	return false
}

// WriteConfig writes a config file with 0600 permissions, creating parent
// directories as needed. wg-quick refuses world-readable configs, and rightly so.
func WriteConfig(path, conf string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	// WriteFile only applies the mode on create; enforce it on overwrite too.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod config: %w", err)
	}
	return nil
}

// Redact masks key material so a config can be logged or printed safely.
func Redact(conf string) string {
	out := make([]string, 0, 64)
	for _, line := range strings.Split(conf, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "PrivateKey"),
			strings.HasPrefix(trimmed, "PresharedKey"):
			eq := strings.Index(line, "=")
			if eq >= 0 {
				out = append(out, line[:eq+1]+" <redacted>")
				continue
			}
		case strings.HasPrefix(trimmed, "PublicKey"):
			eq := strings.Index(line, "=")
			if eq >= 0 {
				val := strings.TrimSpace(line[eq+1:])
				if len(val) > 8 {
					val = val[:8] + "...<redacted>"
				}
				out = append(out, line[:eq+1]+" "+val)
				continue
			}
		case strings.HasPrefix(trimmed, "AllowedIPs"):
			eq := strings.Index(line, "=")
			if eq >= 0 {
				out = append(out, fmt.Sprintf("%s <%d CIDRs>", line[:eq+1], strings.Count(line, ",")+1))
				continue
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
