// Package wg handles WireGuard key material, config assembly, and tunnel lifecycle.
//
// The design constraint that shapes this package: the API never receives a private
// key. We generate the keypair locally, send only the public half, and substitute
// the private half into the template the server returns. The server's own template
// carries a %private_key% placeholder precisely so this is possible.
package wg

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
)

// KeyPair is a Curve25519 keypair in WireGuard's base64 encoding.
type KeyPair struct {
	PrivateKey string `json:"private_key"`
	PublicKey  string `json:"public_key"`
}

// GenerateKeyPair produces a WireGuard keypair entirely on this machine.
//
// The private scalar is clamped exactly as `wg genkey` does. X25519 clamps again
// when the key is used, so the clamped form is canonical and derives the same
// public key we transmit to the API.
func GenerateKeyPair() (KeyPair, error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return KeyPair{}, fmt.Errorf("read random: %w", err)
	}
	raw[0] &= 248
	raw[31] &= 127
	raw[31] |= 64

	priv, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return KeyPair{}, fmt.Errorf("x25519 private key: %w", err)
	}

	return KeyPair{
		PrivateKey: base64.StdEncoding.EncodeToString(priv.Bytes()),
		PublicKey:  base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes()),
	}, nil
}

// PublicKeyFrom derives the WireGuard public key for a base64 private key.
// Used to confirm a stored keypair is internally consistent before we send it.
func PublicKeyFrom(privateKey string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(privateKey)
	if err != nil {
		return "", fmt.Errorf("decode private key: %w", err)
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("private key is %d bytes, want 32", len(raw))
	}
	priv, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return "", fmt.Errorf("x25519 private key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes()), nil
}
