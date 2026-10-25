// Package password hashes and checks login passwords with argon2id, in the
// PHC string format ($argon2id$v=19$m=…,t=…,p=…$salt$hash).
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// The OWASP minimum for argon2id: 19 MiB, two passes, one lane. Memory is
// the cost an attacker pays per guess and the server pays per login.
const (
	memory  = 19 * 1024
	time    = 2
	threads = 1
	keyLen  = 32
	saltLen = 16
)

var b64 = base64.RawStdEncoding

// Hash returns the PHC string for pw with a fresh salt.
func Hash(pw string) string {
	salt := make([]byte, saltLen)
	rand.Read(salt)
	key := argon2.IDKey([]byte(pw), salt, time, memory, threads, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memory, time, threads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// Check reports whether pw matches hash. It reads the parameters from hash,
// so hashes made with older parameters keep working.
func Check(hash, pw string) (bool, error) {
	f := strings.Split(hash, "$")
	if len(f) != 6 || f[1] != "argon2id" {
		return false, errors.New("password: not an argon2id hash")
	}
	var v int
	if _, err := fmt.Sscanf(f[2], "v=%d", &v); err != nil || v != argon2.Version {
		return false, errors.New("password: unsupported argon2 version")
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(f[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, fmt.Errorf("password: bad parameters: %w", err)
	}
	salt, err := b64.DecodeString(f[4])
	if err != nil {
		return false, fmt.Errorf("password: bad salt: %w", err)
	}
	want, err := b64.DecodeString(f[5])
	if err != nil {
		return false, fmt.Errorf("password: bad key: %w", err)
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummy is checked against when a username does not exist, so that a
// failed login takes as long either way.
var dummy = Hash("")

// CheckMissing spends the same time as Check and returns false.
func CheckMissing(pw string) {
	Check(dummy, pw)
}
