package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/lum1n/say-v2/internal/core"
	"github.com/lum1n/say-v2/internal/store"
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
	if err != nil {
		return core.Principal{}, err
	}
	if err := s.checkVisitor(r.Context(), me); err != nil {
		return core.Principal{}, err
	}
	return me, nil
}

// TokenProtocol prefixes the access token offered as a WebSocket
// subprotocol, next to WSProtocol. Browsers cannot set headers on a
// WebSocket, and the widget's iframe cannot count on cookies, which
// browsers block in third-party frames.
const (
	WSProtocol    = "say.v1"
	TokenProtocol = "say.token."
)

// requestToken returns the secret from the Authorization header, or else
// from a WebSocket subprotocol, or else from the token cookie.
func requestToken(r *http.Request) string {
	if secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(secret)
	}
	for _, h := range r.Header.Values("Sec-WebSocket-Protocol") {
		for p := range strings.SplitSeq(h, ",") {
			if secret, ok := strings.CutPrefix(strings.TrimSpace(p), TokenProtocol); ok {
				return secret
			}
		}
	}
	if c, err := r.Cookie(TokenCookie); err == nil {
		return strings.TrimSpace(c.Value)
	}
	return ""
}
