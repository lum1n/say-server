package api

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/store"
)

// pairingTTL is how long an "Add a device" code works
// (docs/adr/0018-devices.md).
const pairingTTL = 10 * time.Minute

// pairDevice signs a person in from one of their devices, such as a phone
// or a node syncing with this one (docs/adr/0015-sync.md), and returns a
// token for it that does not expire. The device proves who it is with the
// person's username and password, or with a pairing code from a device
// they are signed in on. A device pairing again under the same name
// replaces its earlier token.
func (s *Server) pairDevice(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		PairingCode string `json:"pairing_code"`
		Name        string `json:"name"`
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
	var p core.Principal
	var err error
	switch {
	case body.PairingCode != "" && body.Username == "" && body.Password == "":
		p, err = s.redeemPairing(r, body.PairingCode)
	case body.PairingCode == "":
		p, err = s.checkLogin(r, body.Username, body.Password)
	default:
		err = errorf(http.StatusUnprocessableEntity, "invalid", "send a username and password, or a pairing code, not both")
	}
	if err != nil {
		writeError(w, err)
		return
	}
	token := "device:" + name
	if err := s.store.RevokeTokensNamed(r.Context(), p.ID, token); err != nil {
		writeError(w, err)
		return
	}
	secret, _, err := s.store.IssueToken(r.Context(), store.NewToken{PrincipalID: p.ID, Name: token, UserAgent: r.UserAgent()})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"principal": toPrincipal(p), "token": secret})
}

var errBadPairing = errorf(http.StatusUnauthorized, "invalid_pairing_code", "this pairing code is wrong, used or expired; make a new one on a device you're signed in on")

func (s *Server) redeemPairing(r *http.Request, code string) (core.Principal, error) {
	id, err := s.store.RedeemPairing(r.Context(), code)
	if errors.Is(err, store.ErrNotFound) {
		return core.Principal{}, errBadPairing
	}
	if err != nil {
		return core.Principal{}, err
	}
	return s.store.GetPrincipal(r.Context(), id)
}

// createPairing makes a one-time code that adds another device for the
// caller: the device sends it to POST /devices.
func (s *Server) createPairing(w http.ResponseWriter, r *http.Request, me core.Principal) {
	if me.Kind != core.PrincipalHuman {
		writeError(w, errorf(http.StatusForbidden, "forbidden", "only people pair devices"))
		return
	}
	expires := time.Now().Add(pairingTTL).Truncate(time.Second)
	code, err := s.store.CreatePairing(r.Context(), me.ID, expires)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"code": code, "expires_at": expires.UTC()})
}

// wireToken is an access token as GET /principals/{id}/tokens lists it.
type wireToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	UserAgent  string     `json:"user_agent,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Current    bool       `json:"current"`
}

// tokenOwner is the principal whose tokens the caller may manage: their
// own, or anyone's for an instance admin.
func (s *Server) tokenOwner(r *http.Request, me core.Principal) (string, error) {
	id := r.PathValue("id")
	if id == me.ID {
		return id, nil
	}
	if !me.Admin {
		return "", errorf(http.StatusForbidden, "forbidden", "only instance admins manage other principals' tokens")
	}
	if _, err := s.store.GetPrincipal(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		return "", errorf(http.StatusNotFound, "not_found", "no such principal")
	} else if err != nil {
		return "", err
	}
	return id, nil
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request, me core.Principal) {
	id, err := s.tokenOwner(r, me)
	if err != nil {
		writeError(w, err)
		return
	}
	toks, err := s.store.Tokens(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	_, current, _ := s.store.Authenticate(r.Context(), requestToken(r))
	out := make([]wireToken, 0, len(toks))
	for _, t := range toks {
		wt := wireToken{ID: t.ID, Name: t.Name, UserAgent: t.UserAgent, CreatedAt: t.CreatedAt.UTC(), Current: t.ID == current}
		if !t.ExpiresAt.IsZero() {
			at := t.ExpiresAt.UTC()
			wt.ExpiresAt = &at
		}
		if !t.LastUsedAt.IsZero() {
			at := t.LastUsedAt.UTC()
			wt.LastUsedAt = &at
		}
		out = append(out, wt)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// revokeToken signs a session or device out. Its open connections close
// at their next heartbeat (tokenWatch).
func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request, me core.Principal) {
	id, err := s.tokenOwner(r, me)
	if err != nil {
		writeError(w, err)
		return
	}
	switch err := s.store.RevokeTokenID(r.Context(), id, r.PathValue("token_id")); {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, errorf(http.StatusNotFound, "not_found", "no such token"))
	case err != nil:
		writeError(w, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
