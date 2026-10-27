package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/lum1n/say-server/internal/bridge"
	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/store"
)

// screenTTL is how long a screen code shows a bridge's login step.
const screenTTL = 10 * time.Minute

// createBridgeScreen makes a code that shows the caller's bridge's login
// step, such as Signal's QR code, on another screen: the say's /link page,
// opened on a laptop or tablet without signing in, for when the phone that
// must scan the code is the one the person uses say on.
func (s *Server) createBridgeScreen(w http.ResponseWriter, r *http.Request, me core.Principal) {
	b, err := s.bridgeFor(r.Context(), me, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if b.OwnerID != me.ID {
		writeError(w, errorf(http.StatusForbidden, "forbidden", "only the bridge's owner logs it in"))
		return
	}
	expires := time.Now().Add(screenTTL).Truncate(time.Second)
	code, err := s.store.CreateBridgeScreen(r.Context(), b.ID, expires)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"code": code, "expires_at": expires.UTC()})
}

// wireScreen is what a screen code shows: the network, how the bridge is
// doing, and its login step. Nothing else about the bridge or its owner.
type wireScreen struct {
	Network string         `json:"network"`
	Title   string         `json:"title"`
	State   string         `json:"state"`
	Prompt  *bridge.Prompt `json:"prompt,omitempty"`
}

// bridgeScreen answers GET /screens/{code}, without a token.
func (s *Server) bridgeScreen(w http.ResponseWriter, r *http.Request) {
	id, err := s.store.BridgeScreen(r.Context(), r.PathValue("code"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, errorf(http.StatusNotFound, "not_found", "that code is wrong or has run out"))
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	b, err := s.store.GetBridge(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	wb := toBridge(b)
	out := wireScreen{Network: b.Network, Title: b.Network, State: wb.State, Prompt: wb.Prompt}
	if n, ok := bridge.Lookup(b.Network); ok {
		out.Title = n.Title
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}
