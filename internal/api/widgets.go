package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vegard/say-v2/internal/core"
	"github.com/vegard/say-v2/internal/store"
)

// Widgets put a chat on other sites (docs/adr/0012-widget.md). An admin
// creates one; each visitor gets a guest principal and a widget thread
// with the widget's team and agent.

const (
	// visitorTTL is how long a visitor's token lasts. An anonymous visitor
	// whose token expires, or whose browser forgets it, starts over.
	visitorTTL = 90 * 24 * time.Hour
	// maxIdentityTTL bounds how far ahead an identity token may expire, so
	// one that leaks from a page is not good for long.
	maxIdentityTTL = 24 * time.Hour
	clockSkew      = time.Minute
	maxOrigins     = 20
	// threadIdleAfter is how long a widget thread goes without a message
	// before the inbox calls it idle.
	threadIdleAfter = 30 * time.Minute
)

var errWidgetNotFound = errorf(http.StatusNotFound, "not_found", "no such widget")

// wireWidget is a widget as admins see it, secret included: they paste it
// into the host site's server.
type wireWidget struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	PublicKey string    `json:"public_key"`
	Secret    string    `json:"secret"`
	Origins   []string  `json:"origins"`
	AgentID   string    `json:"agent_id,omitempty"`
	Team      []string  `json:"team"`
	Greeting  string    `json:"greeting"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedAt time.Time `json:"created_at"`
}

func toWidget(w store.Widget) wireWidget {
	return wireWidget{
		ID: w.ID, Name: w.Name, PublicKey: w.PublicKey, Secret: w.Secret,
		Origins: nonNil(w.Origins), AgentID: w.AgentID, Team: nonNil(w.Team),
		Greeting: w.Greeting, UpdatedAt: w.UpdatedAt, CreatedAt: w.CreatedAt,
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// widgetSettings is the body of POST /widgets and PATCH /widgets/{id}.
// On PATCH absent fields stay as they are.
type widgetSettings struct {
	Name         *string   `json:"name"`
	Origins      *[]string `json:"origins"`
	AgentID      *string   `json:"agent_id"`
	Team         *[]string `json:"team"`
	Greeting     *string   `json:"greeting"`
	RotateSecret bool      `json:"rotate_secret"`
}

// applyWidget validates b and writes it to w.
func (s *Server) applyWidget(ctx context.Context, b widgetSettings, w *store.Widget) error {
	if b.Name != nil {
		w.Name = strings.TrimSpace(*b.Name)
	}
	if w.Name == "" || len(w.Name) > 100 {
		return errorf(http.StatusUnprocessableEntity, "invalid", "name must be 1–100 bytes")
	}
	if b.Greeting != nil {
		w.Greeting = strings.TrimSpace(*b.Greeting)
	}
	if len(w.Greeting) > 1000 {
		return errorf(http.StatusUnprocessableEntity, "invalid", "greeting is longer than 1000 bytes")
	}
	if b.Origins != nil {
		if len(*b.Origins) > maxOrigins {
			return errorf(http.StatusUnprocessableEntity, "invalid", "a widget has at most %d origins", maxOrigins)
		}
		w.Origins = nil
		for _, o := range *b.Origins {
			origin, err := normalizeOrigin(o)
			if err != nil {
				return errorf(http.StatusUnprocessableEntity, "invalid", "origin %q: %v", o, err)
			}
			if !slices.Contains(w.Origins, origin) {
				w.Origins = append(w.Origins, origin)
			}
		}
	}
	if len(w.Origins) == 0 {
		return errorf(http.StatusUnprocessableEntity, "invalid", "list at least one origin the widget is embedded on")
	}
	if b.AgentID != nil {
		w.AgentID = *b.AgentID
	}
	if w.AgentID != "" {
		p, err := s.store.GetPrincipal(ctx, w.AgentID)
		if errors.Is(err, store.ErrNotFound) || err == nil && p.Kind != core.PrincipalAgent {
			return errorf(http.StatusUnprocessableEntity, "invalid", "agent_id is not an agent")
		}
		if err != nil {
			return err
		}
	}
	if b.Team != nil {
		w.Team = nil
		for _, id := range *b.Team {
			if slices.Contains(w.Team, id) {
				continue
			}
			p, err := s.store.GetPrincipal(ctx, id)
			if errors.Is(err, store.ErrNotFound) || err == nil && p.Kind != core.PrincipalHuman {
				return errorf(http.StatusUnprocessableEntity, "invalid", "team member %q is not a person", id)
			}
			if err != nil {
				return err
			}
			w.Team = append(w.Team, id)
		}
	}
	if w.AgentID == "" && len(w.Team) == 0 {
		return errorf(http.StatusUnprocessableEntity, "invalid", "a widget needs an agent, a team, or both to answer visitors")
	}
	if b.RotateSecret || w.Secret == "" {
		w.Secret = "sk_" + rand.Text()
	}
	return nil
}

// normalizeOrigin turns "https://Shop.example/" into "https://shop.example".
func normalizeOrigin(o string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(o))
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", errors.New("must start with https:// or http://")
	}
	if u.Host == "" || u.User != nil || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("must be a scheme and host only, like https://example.com")
	}
	if strings.ContainsAny(u.Host, "*") {
		return "", errors.New("wildcards are not allowed")
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

func (s *Server) listWidgets(w http.ResponseWriter, r *http.Request, me core.Principal) {
	if !me.Admin {
		writeError(w, errorf(http.StatusForbidden, "forbidden", "only instance admins manage widgets"))
		return
	}
	ws, err := s.store.Widgets(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]wireWidget, len(ws))
	for i, wg := range ws {
		out[i] = toWidget(wg)
	}
	writeJSON(w, http.StatusOK, map[string]any{"widgets": out})
}

func (s *Server) createWidget(w http.ResponseWriter, r *http.Request, me core.Principal) {
	if !me.Admin {
		writeError(w, errorf(http.StatusForbidden, "forbidden", "only instance admins manage widgets"))
		return
	}
	var body widgetSettings
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	now := time.Now().Truncate(time.Millisecond)
	wg := store.Widget{ID: core.NewID(), PublicKey: "pk_" + rand.Text(), CreatedAt: now, UpdatedAt: now}
	if err := s.applyWidget(r.Context(), body, &wg); err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.CreateWidget(r.Context(), wg); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toWidget(wg))
}

// updateWidget changes a widget's settings. Threads that exist keep the
// members they were created with.
func (s *Server) updateWidget(w http.ResponseWriter, r *http.Request, me core.Principal) {
	if !me.Admin {
		writeError(w, errorf(http.StatusForbidden, "forbidden", "only instance admins manage widgets"))
		return
	}
	var body widgetSettings
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	wg, err := s.store.GetWidget(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		err = errWidgetNotFound
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.applyWidget(r.Context(), body, &wg); err != nil {
		writeError(w, err)
		return
	}
	wg.UpdatedAt = time.Now().Truncate(time.Millisecond)
	if err := s.store.UpdateWidget(r.Context(), wg); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toWidget(wg))
}

// embedConfig is what the /embed page reads before anyone has a session.
func (s *Server) embedConfig(w http.ResponseWriter, r *http.Request) {
	wg, err := s.store.WidgetByKey(r.Context(), r.PathValue("key"))
	if errors.Is(err, store.ErrNotFound) {
		err = errWidgetNotFound
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":     wg.Name,
		"greeting": wg.Greeting,
		"origins":  nonNil(wg.Origins),
	})
}

// embedSession signs a visitor in to the widget and returns its thread.
// A token from an earlier session of the same visitor is returned as is;
// a valid identity token finds the verified visitor with that subject,
// else makes the anonymous visitor of the request's token that visitor (so
// it keeps its thread), else creates one; otherwise a new anonymous
// visitor is created. The embed page calls it only when it has a token or
// an identity, or on the first message, so page views alone create
// nothing.
func (s *Server) embedSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Identity string `json:"identity"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	wg, err := s.store.WidgetByKey(ctx, r.PathValue("key"))
	if errors.Is(err, store.ErrNotFound) {
		err = errWidgetNotFound
	}
	if err != nil {
		writeError(w, err)
		return
	}
	var id identity
	if body.Identity != "" {
		if id, err = verifyIdentity(wg.Secret, body.Identity, time.Now()); err != nil {
			writeError(w, errorf(http.StatusUnauthorized, "bad_identity", "identity: %v", err))
			return
		}
	}

	// The same visitor again, or an anonymous one who has signed in.
	var anon core.Principal
	if token := requestToken(r); token != "" {
		if p, err := s.store.PrincipalByToken(ctx, token); err == nil && p.Kind == core.PrincipalGuest {
			v, err := s.store.VisitorByPrincipal(ctx, p.ID)
			if err == nil && v.WidgetID == wg.ID && v.Subject == id.Subject {
				s.writeSession(w, ctx, http.StatusOK, p, v, token)
				return
			}
			if err == nil && v.WidgetID == wg.ID && v.Subject == "" {
				anon = p
			}
		}
	}

	status := http.StatusOK
	var v store.Visitor
	if id.Subject != "" {
		v, err = s.store.VisitorBySubject(ctx, wg.ID, id.Subject)
	}
	if anon.ID != "" && errors.Is(err, store.ErrNotFound) {
		v, err = s.verifyVisitor(ctx, wg, anon, id)
	}
	if id.Subject == "" || errors.Is(err, store.ErrNotFound) {
		v, err = s.newVisitor(ctx, wg, id)
		status = http.StatusCreated
	}
	if err != nil {
		writeError(w, err)
		return
	}
	p, err := s.store.GetPrincipal(ctx, v.PrincipalID)
	if err != nil {
		writeError(w, err)
		return
	}
	token, err := s.store.CreateToken(ctx, p.ID, "widget", time.Now().Add(visitorTTL))
	if err != nil {
		writeError(w, err)
		return
	}
	s.writeSession(w, ctx, status, p, v, token)
}

func (s *Server) writeSession(w http.ResponseWriter, ctx context.Context, status int, p core.Principal, v store.Visitor, token string) {
	room, err := s.store.GetRoom(ctx, v.RoomID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, status, map[string]any{"principal": toPrincipal(p), "room": s.toRoom(room), "token": token})
}

// newVisitor creates a visitor with its thread. The team join as admins
// and the agent as the thread's responder.
func (s *Server) newVisitor(ctx context.Context, wg store.Widget, id identity) (store.Visitor, error) {
	now := time.Now().Truncate(time.Millisecond)
	guest := core.Principal{ID: core.NewID(), Kind: core.PrincipalGuest, DisplayName: id.Name, CreatedAt: now}
	if guest.DisplayName == "" {
		guest.DisplayName = "Visitor " + strings.ToUpper(guest.ID[len(guest.ID)-4:])
	}
	room := core.Room{ID: core.NewID(), Kind: core.RoomWidgetThread, Name: wg.Name + " · " + guest.DisplayName, CreatedAt: now}
	members := []core.Membership{{RoomID: room.ID, PrincipalID: guest.ID, Role: core.RoleMember}}
	// People removed since the widget was saved are skipped.
	for _, pid := range wg.Team {
		if p, err := s.store.GetPrincipal(ctx, pid); err == nil && p.Kind == core.PrincipalHuman {
			members = append(members, core.Membership{RoomID: room.ID, PrincipalID: pid, Role: core.RoleAdmin})
		}
	}
	if wg.AgentID != "" {
		members = append(members, core.Membership{RoomID: room.ID, PrincipalID: wg.AgentID, Role: core.RoleMember, Scopes: []string{core.ScopeResponder}})
	}
	v := store.Visitor{PrincipalID: guest.ID, WidgetID: wg.ID, Subject: id.Subject, RoomID: room.ID, CreatedAt: now}
	err := s.store.CreateVisitor(ctx, guest, room, members, v)
	if errors.Is(err, store.ErrVisitorExists) {
		// A concurrent first visit by the same verified visitor won.
		return s.store.VisitorBySubject(ctx, wg.ID, id.Subject)
	}
	if err != nil {
		return store.Visitor{}, err
	}
	for _, m := range members[1:] {
		s.notifyRooms(ctx, m.PrincipalID)
	}
	return v, nil
}

// verifyVisitor makes anonymous visitor anon the verified visitor id,
// when the widget has none yet, so a visitor who signs in on the host site
// keeps the thread they started. The guest takes the identity's name, and
// its anonymous tokens stop working: the thread now holds what the
// verified visitor writes on any device.
func (s *Server) verifyVisitor(ctx context.Context, wg store.Widget, anon core.Principal, id identity) (store.Visitor, error) {
	old, err := s.store.VisitorByPrincipal(ctx, anon.ID)
	if err != nil {
		return store.Visitor{}, err
	}
	room, err := s.store.GetRoom(ctx, old.RoomID)
	if err != nil {
		return store.Visitor{}, err
	}
	roomName := ""
	if prefix, ok := strings.CutSuffix(room.Name, " · "+anon.DisplayName); ok && id.Name != "" {
		roomName = prefix + " · " + id.Name
	}
	v, err := s.store.VerifyVisitor(ctx, anon.ID, id.Subject, id.Name, roomName)
	if errors.Is(err, store.ErrVisitorExists) {
		// A concurrent first visit by the same verified visitor won; the
		// anonymous thread stays as it is.
		return s.store.VisitorBySubject(ctx, wg.ID, id.Subject)
	}
	if err != nil {
		return store.Visitor{}, err
	}
	if err := s.store.RevokeTokensNamed(ctx, anon.ID, "widget"); err != nil {
		return store.Visitor{}, err
	}
	if id.Name != "" {
		// The team's room lists and member panels show the new name.
		if ms, err := s.store.Members(ctx, v.RoomID); err == nil {
			for _, m := range ms {
				s.notifyRooms(ctx, m.PrincipalID)
			}
		}
	}
	return v, nil
}

// deleteEmbedSession is a visitor deleting its own chat: the thread,
// what it wrote and the visitor itself. Its token stops working.
func (s *Server) deleteEmbedSession(w http.ResponseWriter, r *http.Request, me core.Principal) {
	ctx := r.Context()
	wg, err := s.store.WidgetByKey(ctx, r.PathValue("key"))
	if errors.Is(err, store.ErrNotFound) {
		err = errWidgetNotFound
	}
	if err != nil {
		writeError(w, err)
		return
	}
	v, err := s.store.VisitorByPrincipal(ctx, me.ID)
	if errors.Is(err, store.ErrNotFound) || err == nil && v.WidgetID != wg.ID {
		err = errorf(http.StatusForbidden, "forbidden", "only the widget's visitors delete their chat")
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.eraseVisitor(ctx, v); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteVisitor is an instance admin erasing a visitor and its thread,
// such as when a visitor asks for their data to be deleted.
func (s *Server) deleteVisitor(w http.ResponseWriter, r *http.Request, me core.Principal) {
	if !me.Admin {
		writeError(w, errorf(http.StatusForbidden, "forbidden", "only instance admins delete visitors"))
		return
	}
	v, err := s.store.VisitorByPrincipal(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		err = errorf(http.StatusNotFound, "not_found", "no such visitor")
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.eraseVisitor(r.Context(), v); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// eraseVisitor deletes v with its thread, then tells everyone who shared a
// room with it to reread their rooms and members.
func (s *Server) eraseVisitor(ctx context.Context, v store.Visitor) error {
	rooms, err := s.store.RoomsFor(ctx, v.PrincipalID)
	if err != nil {
		return err
	}
	var others []string
	for _, room := range rooms {
		ms, err := s.store.Members(ctx, room.ID)
		if err != nil {
			return err
		}
		for _, m := range ms {
			if m.PrincipalID != v.PrincipalID && !slices.Contains(others, m.PrincipalID) {
				others = append(others, m.PrincipalID)
			}
		}
	}
	if err := s.store.DeleteVisitor(ctx, v.PrincipalID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// A concurrent delete won.
			return nil
		}
		return err
	}
	slog.Info("widget visitor deleted", "widget", v.WidgetID, "visitor", v.PrincipalID)
	for _, id := range others {
		s.notifyRooms(ctx, id)
	}
	return nil
}

// handoffRoom hands a widget thread from its agent to the people in it.
func (s *Server) handoffRoom(w http.ResponseWriter, r *http.Request, me core.Principal) {
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
	if room.Kind != core.RoomWidgetThread || me.Kind != core.PrincipalGuest && me.Kind != core.PrincipalHuman {
		writeError(w, errorf(http.StatusForbidden, "forbidden", "only visitors and people hand widget threads over"))
		return
	}
	if err := s.handoff(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handoff stops the agents in a widget thread from answering every
// message: they lose the responder scope and, like anywhere else, answer
// only when mentioned. A room owner can make one the responder again.
func (s *Server) handoff(ctx context.Context, roomID string) error {
	ms, err := s.store.Members(ctx, roomID)
	if err != nil {
		return err
	}
	changed := false
	for _, m := range ms {
		if !slices.Contains(m.Scopes, core.ScopeResponder) {
			continue
		}
		scopes := slices.DeleteFunc(slices.Clone(m.Scopes), func(s string) bool { return s == core.ScopeResponder })
		if err := s.store.SetMemberScopes(ctx, roomID, m.PrincipalID, scopes); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		// Everyone's member list shows who answers.
		for _, m := range ms {
			s.notifyRooms(ctx, m.PrincipalID)
		}
	}
	return nil
}

// handoffOnReply hands a widget thread to its people once one of them
// writes in it. send calls it before appending the message.
func (s *Server) handoffOnReply(ctx context.Context, me core.Principal, room core.Room) {
	if room.Kind != core.RoomWidgetThread || me.Kind != core.PrincipalHuman {
		return
	}
	if err := s.handoff(ctx, room.ID); err != nil {
		slog.Warn("widget handoff failed", "room", room.ID, "err", err)
	}
}

// The states of a widget thread in the inbox.
const (
	// threadOpen: an agent answers the visitor.
	threadOpen = "open"
	// threadHandedOver: the thread is the team's, and an agent answers
	// only when mentioned. Threads of widgets without an agent start here.
	threadHandedOver = "handed_over"
	// threadIdle: nobody has written for threadIdleAfter, and no visitor
	// waits for a person.
	threadIdle = "idle"
)

// wireWidgetThread is a widget thread in the team's inbox.
type wireWidgetThread struct {
	RoomID      string `json:"room_id"`
	WidgetID    string `json:"widget_id"`
	WidgetName  string `json:"widget_name"`
	VisitorID   string `json:"visitor_id"`
	VisitorName string `json:"visitor_name"`
	// Verified is whether the host site said who the visitor is.
	Verified bool   `json:"verified"`
	State    string `json:"state"`
	// Waiting is whether the visitor wrote last, so nobody has answered.
	Waiting       bool       `json:"waiting"`
	LastMessageAt *time.Time `json:"last_message_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

func toWidgetThread(th store.WidgetThread, now time.Time) wireWidgetThread {
	out := wireWidgetThread{
		RoomID: th.RoomID, WidgetID: th.WidgetID, WidgetName: th.WidgetName,
		VisitorID: th.PrincipalID, VisitorName: th.VisitorName, Verified: th.Subject != "",
		Waiting: th.LastSenderID == th.PrincipalID, CreatedAt: th.CreatedAt.UTC(),
	}
	active := th.CreatedAt
	if !th.LastMessageAt.IsZero() {
		at := th.LastMessageAt.UTC()
		out.LastMessageAt, active = &at, at
	}
	switch {
	case now.Sub(active) >= threadIdleAfter && !(out.Waiting && !th.AgentAnswers):
		out.State = threadIdle
	case th.AgentAnswers:
		out.State = threadOpen
	default:
		out.State = threadHandedOver
	}
	return out
}

// listWidgetThreads is the inbox: the widget threads I am in, latest
// activity first, so a busy widget need not crowd the room list.
func (s *Server) listWidgetThreads(w http.ResponseWriter, r *http.Request, me core.Principal) {
	if me.Kind != core.PrincipalHuman && me.Kind != core.PrincipalAgent {
		writeError(w, errorf(http.StatusForbidden, "forbidden", "only people and agents have an inbox"))
		return
	}
	limit, err := intParam(r.URL.Query().Get("limit"), 200, 1, maxPage)
	if err != nil {
		writeError(w, errorf(http.StatusBadRequest, "bad_request", "limit: %v", err))
		return
	}
	threads, err := s.store.WidgetThreads(r.Context(), me.ID, int(limit))
	if err != nil {
		writeError(w, err)
		return
	}
	now := time.Now()
	out := make([]wireWidgetThread, len(threads))
	for i, th := range threads {
		out[i] = toWidgetThread(th, now)
	}
	writeJSON(w, http.StatusOK, map[string]any{"threads": out})
}

// frameAncestors is the Content-Security-Policy for /embed/{key}: only the
// widget's origins, and say itself, may frame it.
func (s *Server) frameAncestors(r *http.Request, path string) string {
	sources := []string{"'self'"}
	key, _, _ := strings.Cut(strings.TrimPrefix(path, "embed/"), "/")
	if s.store != nil && key != "" {
		if wg, err := s.store.WidgetByKey(r.Context(), key); err == nil {
			sources = append(sources, wg.Origins...)
		}
	}
	return "frame-ancestors " + strings.Join(sources, " ")
}

// identity is who the host site says the visitor is.
type identity struct {
	Subject string
	Name    string
}

// verifyIdentity checks an identity token: a JWT signed with HS256 and
// the widget secret. sub is required, exp is required and at most
// maxIdentityTTL away, and name is optional.
func verifyIdentity(secret, token string, now time.Time) (identity, error) {
	head, rest, ok := strings.Cut(token, ".")
	payload, sig, ok2 := strings.Cut(rest, ".")
	if !ok || !ok2 || strings.Contains(sig, ".") {
		return identity{}, errors.New("not a JWT")
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := decodeSegment(head, &header); err != nil {
		return identity{}, fmt.Errorf("header: %w", err)
	}
	if header.Alg != "HS256" {
		return identity{}, fmt.Errorf("alg is %q, not HS256", header.Alg)
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return identity{}, errors.New("signature is not base64url")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(head + "." + payload))
	if !hmac.Equal(got, mac.Sum(nil)) {
		return identity{}, errors.New("bad signature")
	}
	var claims struct {
		Sub  string   `json:"sub"`
		Name string   `json:"name"`
		Exp  *float64 `json:"exp"`
		Nbf  *float64 `json:"nbf"`
	}
	if err := decodeSegment(payload, &claims); err != nil {
		return identity{}, fmt.Errorf("claims: %w", err)
	}
	if claims.Sub == "" || len(claims.Sub) > 200 {
		return identity{}, errors.New("sub must be 1–200 bytes")
	}
	if claims.Exp == nil {
		return identity{}, errors.New("exp is required")
	}
	exp := time.Unix(int64(*claims.Exp), 0)
	if !now.Before(exp.Add(clockSkew)) {
		return identity{}, errors.New("expired")
	}
	if exp.After(now.Add(maxIdentityTTL + clockSkew)) {
		return identity{}, fmt.Errorf("exp is more than %s away", maxIdentityTTL)
	}
	if claims.Nbf != nil && now.Add(clockSkew).Before(time.Unix(int64(*claims.Nbf), 0)) {
		return identity{}, errors.New("not valid yet")
	}
	name := strings.TrimSpace(claims.Name)
	if len(name) > 100 || !utf8.ValidString(name) {
		return identity{}, errors.New("name must be at most 100 bytes of UTF-8")
	}
	return identity{Subject: claims.Sub, Name: name}, nil
}

func decodeSegment(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return errors.New("not base64url")
	}
	return json.Unmarshal(b, v)
}
