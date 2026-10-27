// Package releasetest signs release manifests with a throwaway key, and
// serves them, for tests of what reads them.
package releasetest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/lum1n/say-v2/say/release"
	"golang.org/x/crypto/blake2b"
)

// Signer is a minisign key pair.
type Signer struct {
	Public release.PublicKey
	key    ed25519.PrivateKey
}

func NewSigner() Signer {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	s := Signer{key: key}
	rand.Read(s.Public.ID[:])
	s.Public.Key = pub
	return s
}

// PublicKeyFile is the public key as minisign writes it.
func (s Signer) PublicKeyFile() []byte {
	raw := append(append([]byte("Ed"), s.Public.ID[:]...), s.Public.Key...)
	return fmt.Appendf(nil, "untrusted comment: minisign public key\n%s\n", base64.StdEncoding.EncodeToString(raw))
}

// Sign makes a prehashed minisign signature file over msg.
func (s Signer) Sign(msg []byte, comment string) []byte {
	h := blake2b.Sum512(msg)
	sig := ed25519.Sign(s.key, h[:])
	global := ed25519.Sign(s.key, append(append([]byte{}, sig...), comment...))
	raw := append(append([]byte("ED"), s.Public.ID[:]...), sig...)
	return fmt.Appendf(nil, "untrusted comment: signature\n%s\ntrusted comment: %s\n%s\n",
		base64.StdEncoding.EncodeToString(raw), comment, base64.StdEncoding.EncodeToString(global))
}

// Server serves a signed manifest at /manifest.json and files at their
// paths.
type Server struct {
	*httptest.Server
	signer Signer
	mu     sync.Mutex
	files  map[string][]byte
}

func NewServer(t *testing.T, s Signer) *Server {
	srv := &Server{signer: s, files: map[string][]byte{}}
	srv.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.mu.Lock()
		b, ok := srv.files[r.URL.Path]
		srv.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ManifestURL is where the manifest is served.
func (srv *Server) ManifestURL() string { return srv.URL + "/manifest.json" }

// File serves b at path, such as /say-linux-amd64, and returns its URL.
func (srv *Server) File(path string, b []byte) string {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.files[path] = b
	return srv.URL + path
}

// Publish serves m, signed.
func (srv *Server) Publish(m release.Manifest) {
	b, _ := json.Marshal(m)
	srv.File("/manifest.json", b)
	srv.File("/manifest.json.minisig", srv.signer.Sign(b, "say v"+m.Version+" manifest"))
}
