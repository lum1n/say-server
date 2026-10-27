package matrix

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lum1n/say-server/internal/bridge"
)

// fakeServer is a homeserver with just enough of the client-server API
// for the driver: password login, whoami, /sync over one stream of
// events, sends with transaction ids, and state lookups.
type fakeServer struct {
	*httptest.Server
	t *testing.T

	mu        sync.Mutex
	changed   *sync.Cond
	passwords map[string]string
	tokens    map[string]string // token: user id
	logins    int
	rooms     map[string]*fakeRoom
	stream    []fakeEntry
	txns      map[string]string // token txn: event id
	direct    map[string][]string
	lookups   []string // state paths looked up
	lastSince string
}

type fakeRoom struct {
	name    string
	members map[string]string // user id: display name
}

type fakeEntry struct {
	room string
	ev   map[string]any
}

func newFakeServer(t *testing.T) *fakeServer {
	f := &fakeServer{
		t:         t,
		passwords: map[string]string{"@ada:hs": "secret"},
		tokens:    map[string]string{},
		rooms:     map[string]*fakeRoom{},
		txns:      map[string]string{},
		direct:    map[string][]string{},
	}
	f.changed = sync.NewCond(&f.mu)
	mux := http.NewServeMux()
	const v3 = "/_matrix/client/v3"
	mux.HandleFunc("POST "+v3+"/login", f.login)
	mux.HandleFunc("POST "+v3+"/logout", f.auth(f.logout))
	mux.HandleFunc("GET "+v3+"/account/whoami", f.auth(f.whoami))
	mux.HandleFunc("GET "+v3+"/sync", f.auth(f.sync))
	mux.HandleFunc("PUT "+v3+"/rooms/{room}/send/{type}/{txn}", f.auth(f.send))
	mux.HandleFunc("GET "+v3+"/rooms/{room}/state/{type}", f.auth(f.state))
	mux.HandleFunc("GET "+v3+"/rooms/{room}/state/{type}/{key}", f.auth(f.state))
	mux.HandleFunc("GET "+v3+"/rooms/{room}/event/{event}", f.auth(f.event))
	mux.HandleFunc("GET "+v3+"/rooms/{room}/joined_members", f.auth(f.joinedMembers))
	mux.HandleFunc("GET "+v3+"/user/{user}/account_data/{type}", f.auth(f.accountData))
	f.Server = httptest.NewServer(mux)
	t.Cleanup(func() {
		f.mu.Lock()
		f.changed.Broadcast()
		f.mu.Unlock()
		f.Close()
	})
	return f
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, code string) {
	reply(w, status, map[string]string{"errcode": code, "error": code})
}

func (f *fakeServer) auth(h func(w http.ResponseWriter, r *http.Request, user, token string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		user, ok := f.tokens[token]
		f.mu.Unlock()
		if !ok {
			fail(w, http.StatusUnauthorized, "M_UNKNOWN_TOKEN")
			return
		}
		h(w, r, user, token)
	}
}

func (f *fakeServer) logout(w http.ResponseWriter, r *http.Request, user, token string) {
	f.mu.Lock()
	delete(f.tokens, token)
	f.mu.Unlock()
	reply(w, 200, map[string]any{})
}

func (f *fakeServer) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type       string `json:"type"`
		Identifier struct {
			User string `json:"user"`
		} `json:"identifier"`
		Password string `json:"password"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Type != "m.login.password" || f.passwords[req.Identifier.User] == "" || f.passwords[req.Identifier.User] != req.Password {
		fail(w, http.StatusForbidden, "M_FORBIDDEN")
		return
	}
	f.logins++
	token := fmt.Sprintf("tok%d", f.logins)
	f.tokens[token] = req.Identifier.User
	reply(w, 200, map[string]string{"user_id": req.Identifier.User, "access_token": token, "device_id": fmt.Sprintf("DEV%d", f.logins)})
}

func (f *fakeServer) whoami(w http.ResponseWriter, r *http.Request, user, token string) {
	reply(w, 200, map[string]string{"user_id": user, "device_id": "TOKENDEV"})
}

// add puts an event in a room's timeline and returns its id.
func (f *fakeServer) add(room, sender string, at time.Time, content map[string]any) string {
	return f.addType(room, "m.room.message", sender, at, content)
}

func (f *fakeServer) addType(room, typ, sender string, at time.Time, content map[string]any) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addLocked(room, typ, sender, at, content)
}

func (f *fakeServer) addLocked(room, typ, sender string, at time.Time, content map[string]any) string {
	id := fmt.Sprintf("$e%d", len(f.stream)+1)
	f.stream = append(f.stream, fakeEntry{room: room, ev: map[string]any{
		"type": typ, "event_id": id, "sender": sender, "origin_server_ts": at.UnixMilli(), "content": content,
	}})
	f.changed.Broadcast()
	return id
}

func (f *fakeServer) sync(w http.ResponseWriter, r *http.Request, user, token string) {
	since := r.URL.Query().Get("since")
	timeout, _ := strconv.Atoi(r.URL.Query().Get("timeout"))
	var filter struct {
		Room struct {
			Timeline struct {
				Limit int `json:"limit"`
			} `json:"timeline"`
		} `json:"room"`
	}
	if err := json.Unmarshal([]byte(r.URL.Query().Get("filter")), &filter); err != nil {
		f.t.Errorf("sync filter: %v", err)
	}
	from := 0
	if since != "" {
		from, _ = strconv.Atoi(strings.TrimPrefix(since, "s"))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSince = since
	if since != "" && timeout > 0 && len(f.stream) == from {
		// Wait for news, or the timeout.
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-time.After(time.Duration(timeout) * time.Millisecond):
			case <-done:
			case <-r.Context().Done():
			}
			f.mu.Lock()
			f.changed.Broadcast()
			f.mu.Unlock()
		}()
		f.changed.Wait()
	}
	join := map[string]any{}
	timelines := map[string][]map[string]any{}
	for _, e := range f.stream[from:] {
		timelines[e.room] = append(timelines[e.room], e.ev)
	}
	for id, tl := range timelines {
		jr := map[string]any{}
		if since == "" {
			if n := filter.Room.Timeline.Limit; len(tl) > n {
				tl = tl[len(tl)-n:]
			}
			// A first sync has the state and summary.
			room := f.rooms[id]
			var state []map[string]any
			var heroes []string
			if room.name != "" {
				state = append(state, map[string]any{"type": "m.room.name", "state_key": "", "event_id": "$name" + id, "sender": "@bob:hs", "content": map[string]string{"name": room.name}})
			}
			for uid, name := range room.members {
				state = append(state, map[string]any{"type": "m.room.member", "state_key": uid, "event_id": "$m" + uid, "sender": uid, "content": map[string]string{"membership": "join", "displayname": name}})
				if uid != user {
					heroes = append(heroes, uid)
				}
			}
			jr["state"] = map[string]any{"events": state}
			jr["summary"] = map[string]any{"m.heroes": heroes, "m.joined_member_count": len(room.members)}
		}
		jr["timeline"] = map[string]any{"events": tl}
		join[id] = jr
	}
	resp := map[string]any{"next_batch": fmt.Sprintf("s%d", len(f.stream)), "rooms": map[string]any{"join": join}}
	if since == "" {
		resp["account_data"] = map[string]any{"events": []map[string]any{{"type": "m.direct", "content": f.direct}}}
	}
	reply(w, 200, resp)
}

func (f *fakeServer) send(w http.ResponseWriter, r *http.Request, user, token string) {
	var content map[string]any
	json.NewDecoder(r.Body).Decode(&content)
	f.mu.Lock()
	defer f.mu.Unlock()
	room := f.rooms[r.PathValue("room")]
	if room == nil || room.members[user] == "" {
		fail(w, http.StatusForbidden, "M_FORBIDDEN")
		return
	}
	key := token + " " + r.PathValue("txn")
	if id, ok := f.txns[key]; ok {
		reply(w, 200, map[string]string{"event_id": id})
		return
	}
	id := f.addLocked(r.PathValue("room"), r.PathValue("type"), user, time.Now(), content)
	f.stream[len(f.stream)-1].ev["unsigned"] = map[string]string{"transaction_id": r.PathValue("txn")}
	f.txns[key] = id
	reply(w, 200, map[string]string{"event_id": id})
}

func (f *fakeServer) state(w http.ResponseWriter, r *http.Request, user, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups = append(f.lookups, r.PathValue("type")+"/"+r.PathValue("key"))
	room := f.rooms[r.PathValue("room")]
	if room == nil {
		fail(w, http.StatusForbidden, "M_FORBIDDEN")
		return
	}
	switch r.PathValue("type") {
	case "m.room.name":
		if room.name != "" {
			reply(w, 200, map[string]string{"name": room.name})
			return
		}
	case "m.room.member":
		if name, ok := room.members[r.PathValue("key")]; ok {
			reply(w, 200, map[string]string{"membership": "join", "displayname": name})
			return
		}
	}
	fail(w, http.StatusNotFound, "M_NOT_FOUND")
}

func (f *fakeServer) event(w http.ResponseWriter, r *http.Request, user, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups = append(f.lookups, "event/"+r.PathValue("event"))
	for _, e := range f.stream {
		if e.room == r.PathValue("room") && e.ev["event_id"] == r.PathValue("event") {
			reply(w, 200, e.ev)
			return
		}
	}
	fail(w, http.StatusNotFound, "M_NOT_FOUND")
}

func (f *fakeServer) joinedMembers(w http.ResponseWriter, r *http.Request, user, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups = append(f.lookups, "joined_members")
	joined := map[string]any{}
	for uid, name := range f.rooms[r.PathValue("room")].members {
		joined[uid] = map[string]string{"display_name": name}
	}
	reply(w, 200, map[string]any{"joined": joined})
}

func (f *fakeServer) accountData(w http.ResponseWriter, r *http.Request, user, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.PathValue("type") != "m.direct" || len(f.direct) == 0 {
		fail(w, http.StatusNotFound, "M_NOT_FOUND")
		return
	}
	reply(w, 200, f.direct)
}

// fakeHost is the core as a test sees it.
type fakeHost struct {
	mu        sync.Mutex
	received  []bridge.Incoming
	state     json.RawMessage
	connected chan struct{}
	asked     []bridge.Prompt
	answer    string
}

func newFakeHost() *fakeHost { return &fakeHost{connected: make(chan struct{}, 8)} }

func (h *fakeHost) Receive(ctx context.Context, m bridge.Incoming) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.received = append(h.received, m)
	return nil
}

func (h *fakeHost) Ask(ctx context.Context, p bridge.Prompt) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.asked = append(h.asked, p)
	return h.answer, nil
}

func (h *fakeHost) SaveState(ctx context.Context, state json.RawMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state = append(json.RawMessage(nil), state...)
	return nil
}

func (h *fakeHost) Chats(ctx context.Context) ([]string, error) { return nil, nil }

func (h *fakeHost) Connected(ctx context.Context) { h.connected <- struct{}{} }

func (h *fakeHost) messages() []bridge.Incoming {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]bridge.Incoming(nil), h.received...)
}

func (h *fakeHost) stateJSON() json.RawMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state
}

func (h *fakeHost) saved() state {
	h.mu.Lock()
	defer h.mu.Unlock()
	var st state
	json.Unmarshal(h.state, &st)
	return st
}

// waitFor waits until the host received a message for which ok holds.
func (h *fakeHost) waitFor(t *testing.T, what string, ok func(bridge.Incoming) bool) bridge.Incoming {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range h.messages() {
			if ok(m) {
				return m
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; got %+v", what, h.messages())
	return bridge.Incoming{}
}
