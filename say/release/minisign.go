package release

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// PublicKey is a minisign public key: an Ed25519 key and its 8-byte id.
type PublicKey struct {
	ID  [8]byte
	Key ed25519.PublicKey
}

// ParsePublicKey reads a minisign public key file, or just its base64
// line.
func ParsePublicKey(b []byte) (PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(lastLine(b))
	if err != nil || len(raw) != 2+8+ed25519.PublicKeySize || string(raw[:2]) != "Ed" {
		return PublicKey{}, errors.New("not a minisign public key")
	}
	var k PublicKey
	copy(k.ID[:], raw[2:10])
	k.Key = ed25519.PublicKey(raw[10:])
	return k, nil
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// Verify checks a minisign signature file over msg: the signature itself,
// and the global signature that binds the trusted comment to it. It takes
// both minisign's prehashed signatures (BLAKE2b-512, the default) and its
// legacy ones.
func (k PublicKey) Verify(msg, sigFile []byte) error {
	lines := strings.Split(strings.ReplaceAll(string(sigFile), "\r\n", "\n"), "\n")
	if len(lines) < 4 || !strings.HasPrefix(lines[0], "untrusted comment: ") {
		return errors.New("not a minisign signature")
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[1]))
	if err != nil || len(sig) != 2+8+ed25519.SignatureSize {
		return errors.New("not a minisign signature")
	}
	comment, ok := strings.CutPrefix(lines[2], "trusted comment: ")
	if !ok {
		return errors.New("the signature has no trusted comment")
	}
	global, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[3]))
	if err != nil || len(global) != ed25519.SignatureSize {
		return errors.New("the signature has no global signature")
	}
	if !bytes.Equal(sig[2:10], k.ID[:]) {
		return errors.New("signed by another key")
	}
	switch string(sig[:2]) {
	case "ED":
		h := blake2b.Sum512(msg)
		msg = h[:]
	case "Ed":
	default:
		return errors.New("unknown signature algorithm")
	}
	if !ed25519.Verify(k.Key, msg, sig[10:]) {
		return errors.New("bad signature")
	}
	if !ed25519.Verify(k.Key, append(sig[10:], comment...), global) {
		return errors.New("bad trusted comment")
	}
	return nil
}
