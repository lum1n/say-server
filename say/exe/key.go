package exe

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Key is the app's own SSH key for exe.dev, in OpenSSH's formats. It
// never leaves the device: exe.dev learns the public half when the person
// opens the link in exe.dev's sign-in mail.
type Key struct {
	PrivateKey string // an OPENSSH PRIVATE KEY PEM block
	PublicKey  string // "ssh-ed25519 AAAA… say", as exe.dev lists it
}

// NewKey makes an Ed25519 key.
func NewKey() (Key, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Key{}, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "say")
	if err != nil {
		return Key{}, err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return Key{}, err
	}
	return Key{
		PrivateKey: string(pem.EncodeToMemory(block)),
		PublicKey:  strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " say",
	}, nil
}

// SignatureNamespace is the SSHSIG namespace exe.dev checks tokens in.
const SignatureNamespace = "v0@exe.dev"

// MintToken makes an exe0 token for the HTTPS API from privateKey, as
// exe.dev's "local key creation" describes: the permissions as JSON and
// their SSH signature, each base64url. It is only accepted once the key
// is on the person's account. A zero expires gives a token that doesn't
// expire.
func MintToken(privateKey string, cmds []string, expires time.Time) (string, error) {
	signer, err := ssh.ParsePrivateKey([]byte(privateKey))
	if err != nil {
		return "", fmt.Errorf("exe.dev key: %w", err)
	}
	perms := Permissions{Cmds: cmds}
	if !expires.IsZero() {
		perms.Exp = expires.Unix()
	}
	payload, err := json.Marshal(perms)
	if err != nil {
		return "", err
	}
	sig, err := SignSSHSIG(signer, SignatureNamespace, payload)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	return "exe0." + enc.EncodeToString(payload) + "." + enc.EncodeToString(sig), nil
}

// Permissions is what an exe0 token allows.
type Permissions struct {
	Cmds []string `json:"cmds"`
	Exp  int64    `json:"exp,omitempty"` // Unix seconds; 0 never expires
}

// ParseToken splits an exe0 token into its permissions, the exact
// payload bytes that were signed, and the SSHSIG blob. It doesn't check
// the signature.
func ParseToken(token string) (Permissions, []byte, []byte, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 || parts[0] != "exe0" {
		return Permissions{}, nil, nil, errors.New("not an exe0 token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Permissions{}, nil, nil, fmt.Errorf("exe0 token payload: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Permissions{}, nil, nil, fmt.Errorf("exe0 token signature: %w", err)
	}
	var p Permissions
	if err := json.Unmarshal(payload, &p); err != nil {
		return Permissions{}, nil, nil, fmt.Errorf("exe0 token payload: %w", err)
	}
	return p, payload, sig, nil
}

// NeedsRemint reports whether token should be minted again: it expires
// within RemintBefore of now, or can't be read. A pasted token that
// doesn't expire never needs it.
func NeedsRemint(token string, now time.Time) bool {
	p, _, _, err := ParseToken(token)
	if err != nil {
		return true
	}
	return p.Exp != 0 && time.Unix(p.Exp, 0).Before(now.Add(RemintBefore))
}

// The SSHSIG format (OpenSSH's PROTOCOL.sshsig), with SHA-512.
const sshsigMagic = "SSHSIG"

type sshsigSigned struct {
	Namespace string
	Reserved  string
	Hash      string
	Digest    []byte
}

type sshsigBlob struct {
	Version   uint32
	PublicKey []byte
	Namespace string
	Reserved  string
	Hash      string
	Signature []byte
}

func sshsigSignedData(namespace string, msg []byte) []byte {
	digest := sha512.Sum512(msg)
	return append([]byte(sshsigMagic), ssh.Marshal(sshsigSigned{
		Namespace: namespace, Hash: "sha512", Digest: digest[:],
	})...)
}

// SignSSHSIG signs msg as `ssh-keygen -Y sign -n namespace` would, and
// returns the signature blob (the PEM's body, decoded).
func SignSSHSIG(signer ssh.Signer, namespace string, msg []byte) ([]byte, error) {
	sig, err := signer.Sign(rand.Reader, sshsigSignedData(namespace, msg))
	if err != nil {
		return nil, err
	}
	return append([]byte(sshsigMagic), ssh.Marshal(sshsigBlob{
		Version:   1,
		PublicKey: signer.PublicKey().Marshal(),
		Namespace: namespace,
		Hash:      "sha512",
		Signature: ssh.Marshal(*sig),
	})...), nil
}

// VerifySSHSIG checks an SSHSIG blob over msg in namespace and returns
// the key that made it.
func VerifySSHSIG(blob []byte, namespace string, msg []byte) (ssh.PublicKey, error) {
	rest, ok := strings.CutPrefix(string(blob), sshsigMagic)
	if !ok {
		return nil, errors.New("not an SSHSIG")
	}
	var b sshsigBlob
	if err := ssh.Unmarshal([]byte(rest), &b); err != nil {
		return nil, fmt.Errorf("SSHSIG: %w", err)
	}
	if b.Version != 1 || b.Namespace != namespace || b.Hash != "sha512" {
		return nil, errors.New("SSHSIG: wrong version, namespace or hash")
	}
	pub, err := ssh.ParsePublicKey(b.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("SSHSIG key: %w", err)
	}
	var sig ssh.Signature
	if err := ssh.Unmarshal(b.Signature, &sig); err != nil {
		return nil, fmt.Errorf("SSHSIG signature: %w", err)
	}
	if err := pub.Verify(sshsigSignedData(namespace, msg), &sig); err != nil {
		return nil, err
	}
	return pub, nil
}
