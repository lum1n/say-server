package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/vegard/say-v2/internal/core"
	"github.com/vegard/say-v2/internal/store"
)

// TokenCookie carries the access token for browsers, which cannot set an
// Authorization header on a WebSocket or EventSource.
const TokenCookie = "say_token"

// authed resolves the caller from an "Authorization: Bearer" header or the
// token cookie and passes it to h.
func (s *Server) authed(h func(w http.ResponseWriter, r *http.Request, me core.Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, err := s.authenticate(r)
		if err != nil {
			writeError(w, err)
			return
		}
		h(w, r, me)
	}
}

func (s *Server) authenticate(r *http.Request) (core.Principal, error) {
	secret := requestToken(r)
	if secret == "" {
		return core.Principal{}, errUnauthorized
	}
	me, err := s.store.PrincipalByToken(r.Context(), secret)
	if errors.Is(err, store.ErrNotFound) {
		return core.Principal{}, errUnauthorized
	}
	return me, err
}

// requestToken returns the secret from the Authorization header, or else
// from the token cookie.
func requestToken(r *http.Request) string {
	secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		if c, err := r.Cookie(TokenCookie); err == nil {
			secret = c.Value
		}
	}
	return strings.TrimSpace(secret)
}
