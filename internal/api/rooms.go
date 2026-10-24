package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vegard/say-v2/internal/core"
	"github.com/vegard/say-v2/internal/store"
)

const (
	defaultPage = 50
	maxPage     = 500
	// maxWait bounds a long poll on GET /rooms/{id}/events.
	maxWait = 60 * time.Second
)

func (s *Server) me(w http.ResponseWriter, _ *http.Request, me core.Principal) {
	writeJSON(w, http.StatusOK, toPrincipal(me))
}

func (s *Server) listRooms(w http.ResponseWriter, r *http.Request, me core.Principal) {
	rooms, err := s.store.RoomsFor(r.Context(), me.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]wireRoom, len(rooms))
	for i, room := range rooms {
		out[i] = s.toRoom(room)
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": out})
}

// Rooms created through the API. Widget threads are created by the widget.
var creatableRooms = map[core.RoomKind]bool{core.RoomDM: true, core.RoomGroup: true, core.RoomChannel: true}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request, me core.Principal) {
	var body struct {
		Kind core.RoomKind `json:"kind"`
		Name string        `json:"name"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	room, err := s.newRoom(r.Context(), me, body.Kind, body.Name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.toRoom(room))
}

// newRoom creates a room owned by me.
func (s *Server) newRoom(ctx context.Context, me core.Principal, kind core.RoomKind, name string) (core.Room, error) {
	name = strings.TrimSpace(name)
	if !creatableRooms[kind] {
		return core.Room{}, errorf(http.StatusUnprocessableEntity, "invalid", "kind must be dm, group or channel")
	}
	if len(name) > 200 {
		return core.Room{}, errorf(http.StatusUnprocessableEntity, "invalid", "name is longer than 200 bytes")
	}
	room := core.Room{ID: core.NewID(), Kind: kind, Name: name, CreatedAt: time.Now().Truncate(time.Millisecond)}
	owner := core.Membership{RoomID: room.ID, PrincipalID: me.ID, Role: core.RoleOwner}
	if err := s.store.CreateRoom(ctx, room, owner); err != nil {
		return core.Room{}, err
	}
	s.notifyRooms(ctx, me.ID)
	return room, nil
}

func (s *Server) getRoom(w http.ResponseWriter, r *http.Request, me core.Principal) {
	id := r.PathValue("id")
	if _, err := s.member(r.Context(), me, id); err != nil {
		writeError(w, err)
		return
	}
	room, err := s.store.GetRoom(r.Context(), id)
	if err != nil {
		writeError(w, roomErr(err))
		return
	}
	writeJSON(w, http.StatusOK, s.toRoom(room))
}

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request, me core.Principal) {
	id := r.PathValue("id")
	if _, err := s.member(r.Context(), me, id); err != nil {
		writeError(w, err)
		return
	}
	ms, err := s.store.Members(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]wireMember, len(ms))
	for i, m := range ms {
		out[i] = toMember(m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": out})
}

func (s *Server) addMember(w http.ResponseWriter, r *http.Request, me core.Principal) {
	id := r.PathValue("id")
	mine, err := s.member(r.Context(), me, id)
	if err != nil {
		writeError(w, err)
		return
	}
	if mine.Role != core.RoleOwner && mine.Role != core.RoleAdmin {
		writeError(w, errorf(http.StatusForbidden, "forbidden", "only owners and admins add members"))
		return
	}
	var body struct {
		PrincipalID string    `json:"principal_id"`
		Role        core.Role `json:"role"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	if body.Role == "" {
		body.Role = core.RoleMember
	}
	if body.Role != core.RoleAdmin && body.Role != core.RoleMember && body.Role != core.RoleObserver {
		writeError(w, errorf(http.StatusUnprocessableEntity, "invalid", "role must be admin, member or observer"))
		return
	}
	if _, err := s.store.GetPrincipal(r.Context(), body.PrincipalID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			err = errorf(http.StatusNotFound, "not_found", "no such principal")
		}
		writeError(w, err)
		return
	}
	if _, err := s.store.GetMember(r.Context(), id, body.PrincipalID); err == nil {
		writeError(w, errorf(http.StatusConflict, "conflict", "already a member"))
		return
	}
	m := core.Membership{RoomID: id, PrincipalID: body.PrincipalID, Role: body.Role}
	if err := s.store.AddMember(r.Context(), m); err != nil {
		writeError(w, err)
		return
	}
	s.notifyRooms(r.Context(), m.PrincipalID)
	writeJSON(w, http.StatusCreated, toMember(m))
}

// listEvents pages through a room's log. With ?after=N it reads forward from
// cursor N, and with ?wait=S it holds the request up to S seconds until
// something arrives (the long-poll fallback). Without after it reads the
// page that ends before ?before=N, or the latest page.
func (s *Server) listEvents(w http.ResponseWriter, r *http.Request, me core.Principal) {
	id := r.PathValue("id")
	if _, err := s.member(r.Context(), me, id); err != nil {
		writeError(w, err)
		return
	}
	q := r.URL.Query()
	limit, err := intParam(q.Get("limit"), defaultPage, 1, maxPage)
	if err != nil {
		writeError(w, errorf(http.StatusBadRequest, "bad_request", "limit: %v", err))
		return
	}
	var events []core.Event
	if q.Has("after") {
		after, err := intParam(q.Get("after"), 0, 0, math.MaxInt64)
		if err != nil {
			writeError(w, errorf(http.StatusBadRequest, "bad_request", "after: %v", err))
			return
		}
		wait, err := intParam(q.Get("wait"), 0, 0, int64(maxWait/time.Second))
		if err != nil {
			writeError(w, errorf(http.StatusBadRequest, "bad_request", "wait: %v", err))
			return
		}
		events, err = s.poll(r.Context(), id, after, int(limit), time.Duration(wait)*time.Second)
		if err != nil {
			writeError(w, err)
			return
		}
	} else {
		before, err := intParam(q.Get("before"), math.MaxInt64, 1, math.MaxInt64)
		if err != nil {
			writeError(w, errorf(http.StatusBadRequest, "bad_request", "before: %v", err))
			return
		}
		if events, err = s.store.EventsBefore(r.Context(), id, before, int(limit)); err != nil {
			writeError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": toEvents(events)})
}

func (s *Server) postMessage(w http.ResponseWriter, r *http.Request, me core.Principal) {
	var body sendMessage
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	e, created, err := s.send(r.Context(), me, r.PathValue("id"), body, r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, toEvent(e))
}

// send appends a message from me to room and announces it on the bus.
// created is false when idempotencyKey repeats an earlier send, in which
// case the earlier event is returned.
func (s *Server) send(ctx context.Context, me core.Principal, roomID string, msg sendMessage, idempotencyKey string) (e core.Event, created bool, err error) {
	m, err := s.member(ctx, me, roomID)
	if err != nil {
		return core.Event{}, false, err
	}
	if m.Role == core.RoleObserver {
		return core.Event{}, false, errorf(http.StatusForbidden, "forbidden", "observers cannot send")
	}
	if len(idempotencyKey) > 200 {
		return core.Event{}, false, errorf(http.StatusBadRequest, "bad_request", "idempotency key is longer than 200 bytes")
	}
	parts, err := msg.toParts()
	if err != nil {
		return core.Event{}, false, errorf(http.StatusUnprocessableEntity, "invalid", "%v", err)
	}
	room, err := s.store.GetRoom(ctx, roomID)
	if err != nil {
		return core.Event{}, false, roomErr(err)
	}
	policy := s.AgentPolicy(room)
	automated := me.Kind.Automated()
	// The switch is read outside the append's transaction, so a send
	// already under way when someone pauses the room may still land.
	if automated && policy.Paused {
		return core.Event{}, false, errorf(http.StatusConflict, "agents_paused", "agents are paused in this room")
	}
	var cause *core.Event
	if msg.CausedBy != "" {
		c, err := s.store.GetEvent(ctx, msg.CausedBy)
		if errors.Is(err, store.ErrNotFound) || err == nil && c.RoomID != roomID {
			return core.Event{}, false, errorf(http.StatusUnprocessableEntity, "invalid", "caused_by is not an event in this room")
		}
		if err != nil {
			return core.Event{}, false, err
		}
		cause = &c
	}
	causation, err := core.Next(cause, me.Kind, policy.MaxHops)
	if errors.Is(err, core.ErrHopLimit) {
		return core.Event{}, false, errorf(http.StatusUnprocessableEntity, "hop_limit", "the causation chain reached this room's limit of %d automated hops", policy.MaxHops)
	}
	id := core.NewID()
	e, err = s.store.AppendEvent(ctx, core.Event{
		ID:        id,
		RoomID:    roomID,
		Type:      core.EventMessage,
		SenderID:  me.ID,
		Causation: causation,
		Parts:     parts,
		Stream:    core.StreamFinal,
	}, store.AppendOptions{
		IdempotencyKey: idempotencyKey,
		Automated:      automated,
		Room:           store.Budget{Max: policy.TurnLimit, Window: policy.TurnWindow},
		Sender:         store.Budget{Max: policy.RateLimit, Window: time.Minute},
	})
	var be *store.BudgetError
	if errors.As(err, &be) {
		return core.Event{}, false, budgetErr(be)
	}
	if err != nil {
		return core.Event{}, false, roomErr(err)
	}
	s.notify(ctx, roomID)
	return e, e.ID == id, nil
}

// member returns me's membership of room. Rooms me is not in look the same
// as rooms that do not exist.
func (s *Server) member(ctx context.Context, me core.Principal, roomID string) (core.Membership, error) {
	m, err := s.store.GetMember(ctx, roomID, me.ID)
	return m, roomErr(err)
}

func roomErr(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return errRoomNotFound
	}
	return err
}

func intParam(v string, fallback, lo, hi int64) (int64, error) {
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, errors.New("not an integer")
	}
	if n < lo || n > hi {
		return 0, errors.New("out of range")
	}
	return n, nil
}
