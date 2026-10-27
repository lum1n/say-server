package api

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/password"
	"github.com/lum1n/say-server/internal/store"
)

// sessionTTL is how long a browser login lasts.
const sessionTTL = 30 * 24 * time.Hour

var (
	usernameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

	errBadCredentials = errorf(http.StatusUnauthorized, "invalid_credentials", "wrong username or password")
	errSetupDone      = errorf(http.StatusConflict, "setup_done", "this instance is already set up")
	errSetupClosed    = errorf(http.StatusForbidden, "setup_closed", "setup closed before anyone claimed this instance; restart it with a new setup code")
)

// account is the body of POST /setup and the login part of POST
// /principals.
type account struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
}

// login validates a and returns the principal and login to store.
func (a account) login(kind core.PrincipalKind) (core.Principal, store.Login, error) {
	username := strings.ToLower(strings.TrimSpace(a.Username))
	if !usernameRE.MatchString(username) {
		return core.Principal{}, store.Login{}, errorf(http.StatusUnprocessableEntity, "invalid",
			"username must be 1–32 of a-z, 0-9, '.', '_' or '-', starting with a letter or digit")
	}
	if n := utf8.RuneCountInString(a.Password); n < 8 || len(a.Password) > 1024 {
		return core.Principal{}, store.Login{}, errorf(http.StatusUnprocessableEntity, "invalid",
			"password must be at least 8 characters")
	}
	p, err := newPrincipal(kind, a.DisplayName, username)
	if err != nil {
		return core.Principal{}, store.Login{}, err
	}
	return p, store.Login{Username: username, PasswordHash: password.Hash(a.Password)}, nil
}

func newPrincipal(kind core.PrincipalKind, name, fallback string) (core.Principal, error) {
	if name = strings.TrimSpace(name); name == "" {
		name = fallback
	}
	if name == "" || len(name) > 100 {
		return core.Principal{}, errorf(http.StatusUnprocessableEntity, "invalid", "display_name must be 1–100 bytes")
	}
	return core.Principal{ID: core.NewID(), Kind: kind, DisplayName: name, CreatedAt: time.Now().Truncate(time.Millisecond)}, nil
}

func (s *Server) setupStatus(w http.ResponseWriter, r *http.Request) {
	done, err := s.store.SetupDone(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"needed": !done, "open": !done && s.setupOpen()})
}

// setupOpen reports whether POST /setup can still succeed: there is a
// code and its window, if any, hasn't closed.
func (s *Server) setupOpen() bool {
	return s.setupCode != "" && (s.setupUntil.IsZero() || time.Now().Before(s.setupUntil))
}

// setup creates the first admin and signs them in. It needs the setup code
// the node printed at startup, so that whoever reaches a fresh public
// instance first cannot claim it.
func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
		account
	}
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	if s.setupCode == "" {
		writeError(w, errSetupDone)
		return
	}
	if !s.setupOpen() {
		writeError(w, errSetupClosed)
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Code), []byte(s.setupCode)) != 1 {
		writeError(w, errorf(http.StatusForbidden, "bad_setup_code", "wrong setup code; it is in this server's log"))
		return
	}
	p, l, err := body.login(core.PrincipalHuman)
	if err != nil {
		writeError(w, err)
		return
	}
	switch err := s.store.Setup(r.Context(), p, l); {
	case errors.Is(err, store.ErrSetupDone):
		writeError(w, errSetupDone)
		return
	case err != nil:
		writeError(w, err)
		return
	}
	p.Admin = true
	if err := s.startSession(w, r, p); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toPrincipal(p))
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	p, err := s.checkLogin(r, body.Username, body.Password)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.startSession(w, r, p); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toPrincipal(p))
}

// pairDevice signs a person in from one of their devices, such as a node
// syncing with this one (docs/adr/0015-sync.md), and returns a token for
// it that does not expire. A device pairing again under the same name
// replaces its earlier token.
func (s *Server) pairDevice(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Name     string `json:"name"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || utf8.RuneCountInString(name) > 64 {
		writeError(w, errorf(http.StatusUnprocessableEntity, "invalid", "name must be 1–64 characters"))
		return
	}
	p, err := s.checkLogin(r, body.Username, body.Password)
	if err != nil {
		writeError(w, err)
		return
	}
	token := "device:" + name
	if err := s.store.RevokeTokensNamed(r.Context(), p.ID, token); err != nil {
		writeError(w, err)
		return
	}
	secret, err := s.store.CreateToken(r.Context(), p.ID, token, time.Time{})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"principal": toPrincipal(p), "token": secret})
}

// checkLogin returns the person whose username and password these are.
func (s *Server) checkLogin(r *http.Request, username, pw string) (core.Principal, error) {
	l, err := s.store.LoginByUsername(r.Context(), strings.ToLower(strings.TrimSpace(username)))
	if errors.Is(err, store.ErrNotFound) {
		password.CheckMissing(pw)
		return core.Principal{}, errBadCredentials
	}
	if err != nil {
		return core.Principal{}, err
	}
	if ok, err := password.Check(l.PasswordHash, pw); err != nil || !ok {
		return core.Principal{}, errBadCredentials
	}
	return s.store.GetPrincipal(r.Context(), l.PrincipalID)
}

// startSession issues a session token and sets it as the token cookie. The
// cookie is HttpOnly, so page scripts never see the secret.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	expires := time.Now().Add(sessionTTL)
	secret, err := s.store.CreateToken(r.Context(), p.ID, "session", expires)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     TokenCookie,
		Value:    secret,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(sessionTTL / time.Second),
		HttpOnly: true,
		Secure:   secure(r),
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// logout revokes the token the request was made with, whether it came
// from the cookie or the Authorization header, and clears the cookie.
func (s *Server) logout(w http.ResponseWriter, r *http.Request, _ core.Principal) {
	if err := s.store.RevokeToken(r.Context(), requestToken(r)); err != nil {
		writeError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: TokenCookie, Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// secure reports whether the client reached us over HTTPS, directly or
// through a TLS-terminating proxy.
func secure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// listPrincipals is the instance directory that clients resolve sender and
// member names from. Agents with an open bot session carry it as
// connector. Humans and agents see it; guests and bridged ghosts do
// not.
func (s *Server) listPrincipals(w http.ResponseWriter, r *http.Request, me core.Principal) {
	if me.Kind != core.PrincipalHuman && me.Kind != core.PrincipalAgent {
		writeError(w, errorf(http.StatusForbidden, "forbidden", "only people and agents list principals"))
		return
	}
	ps, err := s.store.Principals(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	live, err := s.liveSessions(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]wirePrincipal, len(ps))
	for i, p := range ps {
		out[i] = toPrincipal(p)
		if cs, ok := live[p.ID]; ok {
			out[i].Connector = toConnector(cs)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"principals": out})
}

// createPrincipal lets an admin add a human with a login, or an agent with
// an access token that is returned once.
func (s *Server) createPrincipal(w http.ResponseWriter, r *http.Request, me core.Principal) {
	if !me.Admin {
		writeError(w, errorf(http.StatusForbidden, "forbidden", "only instance admins create principals"))
		return
	}
	var body struct {
		Kind core.PrincipalKind `json:"kind"`
		account
	}
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	switch body.Kind {
	case core.PrincipalHuman:
		p, l, err := body.login(core.PrincipalHuman)
		if err != nil {
			writeError(w, err)
			return
		}
		switch err := s.store.CreateUser(r.Context(), p, l); {
		case errors.Is(err, store.ErrUsernameTaken):
			writeError(w, errorf(http.StatusConflict, "username_taken", "username %q is taken", l.Username))
			return
		case err != nil:
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"principal": toPrincipal(p)})
	case core.PrincipalAgent:
		if body.Username != "" || body.Password != "" {
			writeError(w, errorf(http.StatusUnprocessableEntity, "invalid", "agents sign in with a token, not a username and password"))
			return
		}
		p, err := newPrincipal(core.PrincipalAgent, body.DisplayName, "")
		if err != nil {
			writeError(w, err)
			return
		}
		if err := s.store.CreatePrincipal(r.Context(), p); err != nil {
			writeError(w, err)
			return
		}
		token, err := s.store.CreateToken(r.Context(), p.ID, "agent", time.Time{})
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"principal": toPrincipal(p), "token": token})
	default:
		writeError(w, errorf(http.StatusUnprocessableEntity, "invalid", "kind must be human or agent"))
	}
}
