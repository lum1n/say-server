package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/lum1n/say-server/internal/bridge"
	"github.com/lum1n/say-server/internal/bus"
	"github.com/lum1n/say-server/internal/connector"
	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/store"
)

// Bridges connect rooms to other networks (docs/adr/0014-bridges.md). A
// person adds their own accounts; an instance admin adds relay bots and
// links them to rooms.

var (
	errBridgeNotFound = errorf(http.StatusNotFound, "not_found", "no such bridge")
	errDeleting       = errorf(http.StatusConflict, "deleting", "the bridge is being deleted")
)

type wireNetwork struct {
	Name         string         `json:"name"`
	Title        string         `json:"title"`
	Modes        []string       `json:"modes"`
	Fields       []bridge.Field `json:"fields"`
	Capabilities []string       `json:"capabilities"`
	// Hook says the network takes messages posted to
	// /api/v1/hooks/{bridge id}.
	Hook bool `json:"hook"`
	// HookSecret is the config field holding the hook's secret, which
	// POST /api/v1/bridges/{id}/hook-secret replaces.
	HookSecret string `json:"hook_secret,omitempty"`
}

type wireBridge struct {
	ID      string `json:"id"`
	OwnerID string `json:"owner_id"`
	Mode    string `json:"mode"`
	Network string `json:"network"`
	Name    string `json:"name"`
	// Config without its secrets; SecretsSet names the secrets that are
	// set.
	Config     map[string]any `json:"config"`
	SecretsSet []string       `json:"secrets_set"`
	Enabled    bool           `json:"enabled"`
	// State is deleting, disabled, login, live, error or offline.
	State  string         `json:"state"`
	Error  string         `json:"error,omitempty"`
	Prompt *bridge.Prompt `json:"prompt,omitempty"`
	// SeenAt is when the node running it last saw it connected.
	SeenAt    *time.Time `json:"seen_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// bridgeState is how a bridge is doing, as shown to people.
func bridgeState(b store.Bridge, now time.Time) string {
	switch {
	case !b.DeletedAt.IsZero():
		return "deleting"
	case !b.Enabled:
		return "disabled"
	case b.Prompt != nil:
		return "login"
	case !b.SeenAt.IsZero() && now.Sub(b.SeenAt) < bridge.LiveWithin:
		return "live"
	case b.LastError != "":
		return "error"
	}
	return "offline"
}

func toBridge(b store.Bridge) wireBridge {
	out := wireBridge{
		ID: b.ID, OwnerID: b.OwnerID, Mode: b.Mode, Network: b.Network, Name: b.Name,
		Enabled: b.Enabled, State: bridgeState(b, time.Now()), Error: b.LastError,
		SeenAt: optTime(b.SeenAt), CreatedAt: b.CreatedAt.UTC(),
		Config: map[string]any{}, SecretsSet: []string{},
	}
	if n, ok := bridge.Lookup(b.Network); ok {
		out.Config, out.SecretsSet = n.PublicConfig(b.Mode, b.Config)
		if out.SecretsSet == nil {
			out.SecretsSet = []string{}
		}
	}
	if b.Prompt != nil {
		var p bridge.Prompt
		if json.Unmarshal(b.Prompt, &p) == nil {
			out.Prompt = &p
		}
	}
	return out
}

func (s *Server) listNetworks(w http.ResponseWriter, r *http.Request, me core.Principal) {
	out := []wireNetwork{}
	for _, n := range bridge.Networks() {
		out = append(out, wireNetwork{Name: n.Name, Title: n.Title, Modes: n.Modes, Fields: n.Fields, Capabilities: capNames(n.Capabilities), Hook: n.Hook != nil, HookSecret: n.HookSecret})
	}
	writeJSON(w, http.StatusOK, map[string]any{"networks": out})
}

func capNames(cs []connector.Capability) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = string(c)
	}
	return out
}

// manages reports whether me may see and change bridge b: its owner, or
// an instance admin for relay bots.
func manages(me core.Principal, b store.Bridge) bool {
	return b.OwnerID == me.ID || me.Admin && b.Mode == store.ModeBot
}

func (s *Server) listBridges(w http.ResponseWriter, r *http.Request, me core.Principal) {
	all, err := s.store.Bridges(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	out := []wireBridge{}
	for _, b := range all {
		if manages(me, b) {
			out = append(out, toBridge(b))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"bridges": out})
}

type bridgeSettings struct {
	Network string          `json:"network"`
	Mode    string          `json:"mode"`
	Name    *string         `json:"name"`
	Config  json.RawMessage `json:"config"`
	Enabled *bool           `json:"enabled"`
}

func (s *Server) createBridge(w http.ResponseWriter, r *http.Request, me core.Principal) {
	var body bridgeSettings
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	if body.Mode == "" {
		body.Mode = store.ModeAccount
	}
	switch {
	case body.Mode == store.ModeAccount && me.Kind != core.PrincipalHuman:
		writeError(w, errorf(http.StatusForbidden, "forbidden", "only people connect their accounts"))
		return
	case body.Mode == store.ModeBot && !me.Admin:
		writeError(w, errorf(http.StatusForbidden, "forbidden", "only instance admins add relay bots"))
		return
	case body.Mode != store.ModeAccount && body.Mode != store.ModeBot:
		writeError(w, errorf(http.StatusUnprocessableEntity, "invalid", "mode is account or bot"))
		return
	}
	n, ok := bridge.Lookup(body.Network)
	if !ok {
		writeError(w, errorf(http.StatusUnprocessableEntity, "invalid", "unknown network %q", body.Network))
		return
	}
	if !n.Supports(body.Mode) {
		writeError(w, errorf(http.StatusUnprocessableEntity, "invalid", "%s has no %s mode", n.Title, body.Mode))
		return
	}
	now := time.Now().Truncate(time.Millisecond)
	b := store.Bridge{ID: core.NewID(), OwnerID: me.ID, Mode: body.Mode, Network: n.Name, Name: n.Title, Enabled: true, CreatedAt: now, UpdatedAt: now}
	if err := applyBridge(n, body, &b); err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.CreateBridge(r.Context(), b); err != nil {
		writeError(w, err)
		return
	}
	s.publish(r.Context(), bus.BridgesSubject)
	writeJSON(w, http.StatusCreated, toBridge(b))
}

// applyBridge sets b's name, config and switch from body.
func applyBridge(n bridge.Network, body bridgeSettings, b *store.Bridge) error {
	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if name == "" || len(name) > 100 {
			return errorf(http.StatusUnprocessableEntity, "invalid", "name is 1 to 100 bytes")
		}
		b.Name = name
	}
	config, err := n.MergeConfig(b.Mode, b.Config, body.Config)
	if err != nil {
		return errorf(http.StatusUnprocessableEntity, "invalid", "%v", err)
	}
	b.Config = config
	if body.Enabled != nil {
		b.Enabled = *body.Enabled
	}
	return nil
}

// bridgeFor returns the bridge in the path, if me manages it.
func (s *Server) bridgeFor(ctx context.Context, me core.Principal, id string) (store.Bridge, error) {
	b, err := s.store.GetBridge(ctx, id)
	if errors.Is(err, store.ErrNotFound) || err == nil && !manages(me, b) {
		return store.Bridge{}, errBridgeNotFound
	}
	return b, err
}

func (s *Server) getBridge(w http.ResponseWriter, r *http.Request, me core.Principal) {
	b, err := s.bridgeFor(r.Context(), me, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toBridge(b))
}

// updateBridge changes a bridge's name, config or switch, which restarts
// it.
func (s *Server) updateBridge(w http.ResponseWriter, r *http.Request, me core.Principal) {
	var body bridgeSettings
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	b, err := s.bridgeFor(r.Context(), me, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if !b.DeletedAt.IsZero() {
		writeError(w, errDeleting)
		return
	}
	if body.Network != "" && body.Network != b.Network || body.Mode != "" && body.Mode != b.Mode {
		writeError(w, errorf(http.StatusUnprocessableEntity, "invalid", "a bridge's network and mode can't change"))
		return
	}
	n, ok := bridge.Lookup(b.Network)
	if !ok {
		writeError(w, errorf(http.StatusConflict, "conflict", "this server has no %s bridge", b.Network))
		return
	}
	if err := applyBridge(n, body, &b); err != nil {
		writeError(w, err)
		return
	}
	b.UpdatedAt = time.Now().Truncate(time.Millisecond)
	if err := s.store.UpdateBridge(r.Context(), b); err != nil {
		writeError(w, err)
		return
	}
	s.publish(r.Context(), bus.BridgesSubject)
	writeJSON(w, http.StatusOK, toBridge(b))
}

// deleteBridge asks for a bridge to be deleted. The node that takes its
// lease stops it, logs the account out on the network and removes it with
// its links; portal rooms and linked rooms stay, unlinked, with their
// logs. Until then it shows as deleting.
func (s *Server) deleteBridge(w http.ResponseWriter, r *http.Request, me core.Principal) {
	b, err := s.bridgeFor(r.Context(), me, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if b.DeletedAt.IsZero() {
		b.DeletedAt = time.Now().Truncate(time.Millisecond)
		err = s.store.MarkBridgeDeleted(r.Context(), b.ID, b.DeletedAt)
		if errors.Is(err, store.ErrNotFound) {
			err = errBridgeNotFound
		}
		if err != nil {
			writeError(w, err)
			return
		}
	}
	s.publish(r.Context(), bus.BridgesSubject)
	writeJSON(w, http.StatusAccepted, toBridge(b))
}

// answerBridge passes the owner's answer to a login prompt to the node
// running the bridge.
func (s *Server) answerBridge(w http.ResponseWriter, r *http.Request, me core.Principal) {
	var body struct {
		Value string `json:"value"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	b, err := s.bridgeFor(r.Context(), me, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if b.OwnerID != me.ID {
		writeError(w, errorf(http.StatusForbidden, "forbidden", "only the bridge's owner logs it in"))
		return
	}
	if b.Prompt == nil {
		writeError(w, errorf(http.StatusConflict, "no_prompt", "the bridge is not waiting for an answer"))
		return
	}
	if len(body.Value) > 4096 {
		writeError(w, errorf(http.StatusUnprocessableEntity, "invalid", "the answer is too long"))
		return
	}
	if err := s.bus.Publish(r.Context(), bus.BridgeAnswerSubject(b.ID), []byte(body.Value)); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// hook takes messages posted to a bridge's hook URL, for networks that
// receive over HTTP. The network checks the request's secret; any node
// may take it.
func (s *Server) hook(w http.ResponseWriter, r *http.Request) {
	notFound := errorf(http.StatusNotFound, "not_found", "no such hook")
	b, err := s.store.GetBridge(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		err = notFound
	}
	if err != nil {
		writeError(w, err)
		return
	}
	n, ok := bridge.Lookup(b.Network)
	if !ok || n.Hook == nil || !b.Enabled || !b.DeletedAt.IsZero() {
		writeError(w, notFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.hookBody())
	msgs, err := n.Hook(bridge.Info{ID: b.ID, Mode: b.Mode, Config: b.Config, State: b.State}, r)
	switch {
	case errors.Is(err, bridge.ErrUnauthorized):
		writeError(w, errorf(http.StatusUnauthorized, "unauthorized", "the hook's secret is missing or wrong"))
		return
	case err != nil:
		writeError(w, errorf(http.StatusBadRequest, "invalid", "%v", err))
		return
	}
	if wait, ok := s.hooks.take(b.ID, time.Now()); !ok {
		retry := max(time.Duration(math.Ceil(wait.Seconds()))*time.Second, time.Second)
		ae := errorf(http.StatusTooManyRequests, "rate_limited",
			"this hook takes %d messages a minute; try again in %s", s.hooks.perMinute, retry)
		ae.RetryAfter = retry
		writeError(w, ae)
		return
	}
	err = bridge.ReceiveHooked(r.Context(), bridge.Config{Store: s.store, Bus: s.bus, Chat: s}, b, msgs)
	if errors.Is(err, bridge.ErrNotLinked) {
		err = errorf(http.StatusNotFound, "not_linked", "%v", err)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// rotateHookSecret replaces the secret programs post to the bridge's hook
// with, and returns the new one, which is never shown again. Posts with
// the old secret are refused from then on.
func (s *Server) rotateHookSecret(w http.ResponseWriter, r *http.Request, me core.Principal) {
	b, err := s.bridgeFor(r.Context(), me, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if !b.DeletedAt.IsZero() {
		writeError(w, errDeleting)
		return
	}
	n, ok := bridge.Lookup(b.Network)
	if !ok || n.Hook == nil || n.HookSecret == "" {
		writeError(w, errorf(http.StatusConflict, "no_hook_secret", "this bridge has no hook secret"))
		return
	}
	var raw [32]byte
	rand.Read(raw[:])
	secret := base64.RawURLEncoding.EncodeToString(raw[:])
	patch, _ := json.Marshal(map[string]string{n.HookSecret: secret})
	if err := applyBridge(n, bridgeSettings{Config: patch}, &b); err != nil {
		writeError(w, err)
		return
	}
	b.UpdatedAt = time.Now().Truncate(time.Millisecond)
	if err := s.store.UpdateBridge(r.Context(), b); err != nil {
		writeError(w, err)
		return
	}
	s.hooks.reset(b.ID)
	s.publish(r.Context(), bus.BridgesSubject)
	writeJSON(w, http.StatusOK, map[string]any{"secret": secret, "bridge": toBridge(b)})
}

type wireLink struct {
	ID         string    `json:"id"`
	BridgeID   string    `json:"bridge_id"`
	RoomID     string    `json:"room_id"`
	RoomName   string    `json:"room_name"`
	RemoteID   string    `json:"remote_id"`
	RemoteName string    `json:"remote_name"`
	CreatedAt  time.Time `json:"created_at"`
}

func (s *Server) toLink(ctx context.Context, l store.BridgeLink) wireLink {
	out := wireLink{ID: l.ID, BridgeID: l.BridgeID, RoomID: l.RoomID, RemoteID: l.RemoteID, RemoteName: l.RemoteName, CreatedAt: l.CreatedAt.UTC()}
	if room, err := s.store.GetRoom(ctx, l.RoomID); err == nil {
		out.RoomName = room.Name
	}
	return out
}

func (s *Server) listLinks(w http.ResponseWriter, r *http.Request, me core.Principal) {
	b, err := s.bridgeFor(r.Context(), me, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	links, err := s.store.BridgeLinks(r.Context(), b.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]wireLink, len(links))
	for i, l := range links {
		out[i] = s.toLink(r.Context(), l)
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": out})
}

// createLink links a room to a remote channel of a relay bot. Delivery
// starts from now: the room's history is not sent.
func (s *Server) createLink(w http.ResponseWriter, r *http.Request, me core.Principal) {
	var body struct {
		RoomID     string `json:"room_id"`
		RemoteID   string `json:"remote_id"`
		RemoteName string `json:"remote_name"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	b, err := s.bridgeFor(r.Context(), me, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if b.Mode != store.ModeBot {
		writeError(w, errorf(http.StatusConflict, "conflict", "an account's rooms come from its chats"))
		return
	}
	if !b.DeletedAt.IsZero() {
		writeError(w, errDeleting)
		return
	}
	body.RemoteID = strings.TrimSpace(body.RemoteID)
	if body.RemoteID == "" || len(body.RemoteID) > 500 || len(body.RemoteName) > 200 {
		writeError(w, errorf(http.StatusUnprocessableEntity, "invalid", "remote_id is 1 to 500 bytes and remote_name at most 200"))
		return
	}
	m, err := s.member(r.Context(), me, body.RoomID)
	if err != nil {
		writeError(w, err)
		return
	}
	if m.Role != core.RoleOwner && m.Role != core.RoleAdmin {
		writeError(w, errorf(http.StatusForbidden, "forbidden", "only the room's owners and admins link it"))
		return
	}
	last, err := s.store.EventsBefore(r.Context(), body.RoomID, math.MaxInt64, 1)
	if err != nil {
		writeError(w, err)
		return
	}
	l := store.BridgeLink{ID: core.NewID(), BridgeID: b.ID, RoomID: body.RoomID, RemoteID: body.RemoteID, RemoteName: body.RemoteName, CreatedAt: time.Now()}
	if len(last) > 0 {
		l.Cursor = last[0].Seq
	}
	err = s.store.CreateBridgeLink(r.Context(), l)
	if errors.Is(err, store.ErrLinkExists) {
		err = errorf(http.StatusConflict, "conflict", "that channel is already linked")
	}
	if err != nil {
		writeError(w, err)
		return
	}
	s.publish(r.Context(), bus.BridgesSubject)
	s.notifyMembers(r.Context(), body.RoomID)
	writeJSON(w, http.StatusCreated, s.toLink(r.Context(), l))
}

func (s *Server) deleteLink(w http.ResponseWriter, r *http.Request, me core.Principal) {
	b, err := s.bridgeFor(r.Context(), me, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	l, err := s.store.GetBridgeLink(r.Context(), r.PathValue("link_id"))
	if errors.Is(err, store.ErrNotFound) || err == nil && l.BridgeID != b.ID {
		err = errorf(http.StatusNotFound, "not_found", "no such link")
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.DeleteBridgeLink(r.Context(), b.ID, l.ID); err != nil {
		writeError(w, err)
		return
	}
	s.publish(r.Context(), bus.BridgesSubject)
	s.notifyMembers(r.Context(), l.RoomID)
	w.WriteHeader(http.StatusNoContent)
}

// notifyMembers tells every member of room to reread their rooms.
func (s *Server) notifyMembers(ctx context.Context, roomID string) {
	ms, err := s.store.Members(ctx, roomID)
	if err != nil {
		slog.Warn("notify members failed", "room", roomID, "err", err)
		return
	}
	for _, m := range ms {
		s.notifyRooms(ctx, m.PrincipalID)
	}
}

// wireRoomBridge is a bridge as a room's members see it.
type wireRoomBridge struct {
	BridgeID   string `json:"bridge_id"`
	Network    string `json:"network"`
	Name       string `json:"name"`
	Mode       string `json:"mode"`
	RemoteName string `json:"remote_name"`
	State      string `json:"state"`
	// Capabilities are what the network carries, so clients offer only
	// what reaches it: replies, reactions, attachments.
	Capabilities []string `json:"capabilities"`
}

// roomBridges returns the bridges linked to room.
func (s *Server) roomBridges(ctx context.Context, roomID string) ([]wireRoomBridge, error) {
	links, err := s.store.RoomBridgeLinks(ctx, roomID)
	if err != nil || len(links) == 0 {
		return nil, err
	}
	now := time.Now()
	out := make([]wireRoomBridge, 0, len(links))
	for _, l := range links {
		b, err := s.store.GetBridge(ctx, l.BridgeID)
		if err != nil {
			return nil, err
		}
		name := l.RemoteName
		if name == "" {
			name = l.RemoteID
		}
		n, _ := bridge.Lookup(b.Network)
		caps := n.Capabilities
		if b.Mode == store.ModeBot {
			// A relay bot reacts as itself, not for each person, so it
			// doesn't send reactions out.
			caps = slices.DeleteFunc(slices.Clone(caps), func(c connector.Capability) bool { return c == connector.CapReactions })
		}
		out = append(out, wireRoomBridge{BridgeID: b.ID, Network: b.Network, Name: b.Name, Mode: b.Mode, RemoteName: name, State: bridgeState(b, now), Capabilities: capNames(caps)})
	}
	return out, nil
}

// wireDelivery is how a message's delivery to a bridge went: sending,
// sent, unknown, failed or expired.
type wireDelivery struct {
	EventID  string `json:"event_id"`
	BridgeID string `json:"bridge_id"`
	State    string `json:"state"`
	Error    string `json:"error,omitempty"`
}

// deliveries returns the delivery of room's messages with seq in [from,
// to] to its bridges.
func (s *Server) deliveries(ctx context.Context, roomID string, from, to int64) ([]wireDelivery, error) {
	links, err := s.store.RoomBridgeLinks(ctx, roomID)
	if err != nil || len(links) == 0 {
		return []wireDelivery{}, err
	}
	bridgeOf := map[string]string{}
	for _, l := range links {
		bridgeOf[l.ID] = l.BridgeID
	}
	rows, err := s.store.OutgoingBridgeMessages(ctx, roomID, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]wireDelivery, 0, len(rows))
	for _, m := range rows {
		out = append(out, wireDelivery{EventID: m.EventID, BridgeID: bridgeOf[m.LinkID], State: m.State, Error: m.Error})
	}
	return out, nil
}

// SendAs is how bridges write: the same send path as every client, with
// the event's id and time chosen by the bridge.
// ReactAs is how bridges react: the same path as clients. A reaction say
// does not take, such as a key it does not allow, is dropped and logged.
func (s *Server) ReactAs(ctx context.Context, as core.Principal, roomID, id, target, key string, removed bool, at time.Time) (bool, error) {
	_, changed, err := s.react(ctx, as, roomID, target, key, removed, id, at)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status < 500 && ae.Status != http.StatusTooManyRequests {
		slog.Info("bridged reaction dropped", "room", roomID, "key", key, "err", err)
		return false, nil
	}
	return changed, err
}

func (s *Server) SendAs(ctx context.Context, as core.Principal, roomID, eventID string, parts []core.Part, replyTo, idempotencyKey string, at time.Time) (core.Event, error) {
	e, _, err := s.sendAt(ctx, as, roomID, sendMessage{Parts: toWireParts(parts), ReplyTo: replyTo, Stream: core.StreamFinal}, idempotencyKey, eventID, at)
	return e, err
}

// EditAs changes as's final message and returns the edit event.
func (s *Server) EditAs(ctx context.Context, as core.Principal, roomID, eventID string, parts []core.Part) (core.Event, error) {
	if _, err := s.member(ctx, as, roomID); err != nil {
		return core.Event{}, err
	}
	edit, _, err := s.store.AppendEdit(ctx, core.Event{RoomID: roomID, SenderID: as.ID, Edits: eventID, Parts: parts})
	if err != nil {
		return core.Event{}, err
	}
	s.notify(ctx, roomID)
	return edit, nil
}

// RoomsChanged tells principalID's clients to reread their rooms, and
// with them the state of their bridges.
func (s *Server) RoomsChanged(ctx context.Context, principalID string) {
	s.notifyRooms(ctx, principalID)
}

// DeliveryChanged pushes a message's delivery to the room's clients.
func (s *Server) DeliveryChanged(ctx context.Context, roomID, eventID string) {
	e, err := s.store.GetEvent(ctx, eventID)
	if err != nil {
		return
	}
	ds, err := s.deliveries(ctx, roomID, e.Seq, e.Seq)
	if err != nil {
		slog.Warn("delivery push failed", "room", roomID, "err", err)
		return
	}
	data, err := json.Marshal(map[string]any{"room_id": roomID, "event_id": eventID, "deliveries": ds})
	if err != nil {
		return
	}
	s.publishData(ctx, bus.RoomDeliverySubject(roomID), data)
}

// hookBody bounds a hook request: files come as base64 in the body, so a
// file at the media limit fits.
func (s *Server) hookBody() int64 { return s.mediaMax*4/3 + maxBody }
