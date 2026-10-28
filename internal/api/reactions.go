package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"

	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/store"
)

// maxReactionKey bounds a reaction key in bytes: one emoji, however many
// code points it joins, or a :shortcode: of a network's own emoji.
const maxReactionKey = 64

var shortcode = regexp.MustCompile(`^:[a-z0-9_+\-]{1,62}:$`)

// validReactionKey reports whether key is one grapheme (an emoji with its
// modifiers and joins) or a :shortcode:, and no longer than maxReactionKey.
func validReactionKey(key string) bool {
	if key == "" || len(key) > maxReactionKey || !utf8.ValidString(key) {
		return false
	}
	if shortcode.MatchString(key) {
		return true
	}
	if uniseg.GraphemeClusterCount(key) != 1 {
		return false
	}
	r, _ := utf8.DecodeRuneInString(key)
	return !unicode.IsSpace(r) && !unicode.IsControl(r)
}

func (s *Server) putReaction(w http.ResponseWriter, r *http.Request, me core.Principal) {
	s.reactionRequest(w, r, me, false)
}

func (s *Server) deleteReaction(w http.ResponseWriter, r *http.Request, me core.Principal) {
	s.reactionRequest(w, r, me, true)
}

// reactionRequest answers with the message and its reactions as they now
// are, whether or not anything changed.
func (s *Server) reactionRequest(w http.ResponseWriter, r *http.Request, me core.Principal, removed bool) {
	msg, _, err := s.react(r.Context(), me, r.PathValue("id"), r.PathValue("event_id"), r.PathValue("key"), removed, "", time.Time{})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toEvent(msg))
}

// react adds me's reaction key to message eventID, or takes it back, and
// returns the message as it now is, and whether it changed. Both are
// idempotent: a change that changes nothing logs nothing. id and at, when
// set, are the reaction event's, as for bridges.
func (s *Server) react(ctx context.Context, me core.Principal, roomID, eventID, key string, removed bool, id string, at time.Time) (core.Event, bool, error) {
	m, err := s.member(ctx, me, roomID)
	if err != nil {
		return core.Event{}, false, err
	}
	if err := s.checkVisitor(ctx, me); err != nil {
		return core.Event{}, false, err
	}
	if m.Role == core.RoleObserver {
		return core.Event{}, false, errorf(http.StatusForbidden, "forbidden", "observers cannot react")
	}
	if !validReactionKey(key) {
		return core.Event{}, false, errorf(http.StatusUnprocessableEntity, "invalid", "a reaction is one emoji or a :shortcode:, at most %d bytes", maxReactionKey)
	}
	room, err := s.store.GetRoom(ctx, roomID)
	if err != nil {
		return core.Event{}, false, roomErr(err)
	}
	policy := s.AgentPolicy(room)
	automated := me.Kind.Automated()
	if automated && policy.Paused {
		return core.Event{}, false, errorf(http.StatusConflict, "agents_paused", "agents are paused in this room")
	}
	_, changed, err := s.store.React(ctx, core.Event{
		ID:        id,
		RoomID:    roomID,
		SenderID:  me.ID,
		Reacts:    eventID,
		Reaction:  &core.ReactionChange{Key: key, Removed: removed},
		CreatedAt: at,
	}, store.AppendOptions{
		Automated: automated,
		Room:      store.Budget{Max: policy.TurnLimit, Window: policy.TurnWindow},
		Sender:    store.Budget{Max: policy.RateLimit, Window: time.Minute},
	})
	var be *store.BudgetError
	switch {
	case errors.As(err, &be):
		return core.Event{}, false, budgetErr(be)
	case errors.Is(err, store.ErrNotFound):
		return core.Event{}, false, errorf(http.StatusNotFound, "not_found", "no such message")
	case errors.Is(err, store.ErrNotReactable):
		return core.Event{}, false, errorf(http.StatusUnprocessableEntity, "invalid", "only messages take reactions")
	case errors.Is(err, store.ErrTooManyReactions):
		return core.Event{}, false, errorf(http.StatusUnprocessableEntity, "too_many", "a message has at most %d kinds of reaction", store.MaxReactionKeys)
	case err != nil:
		return core.Event{}, false, err
	}
	if changed {
		s.notify(ctx, roomID)
	}
	msg, err := s.store.GetEvent(ctx, eventID)
	return msg, changed, err
}
