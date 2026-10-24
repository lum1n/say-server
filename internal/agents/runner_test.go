package agents

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vegard/say-v2/internal/api"
	"github.com/vegard/say-v2/internal/bus"
	"github.com/vegard/say-v2/internal/core"
	"github.com/vegard/say-v2/internal/llm"
	"github.com/vegard/say-v2/internal/store"
)

func TestMain(m *testing.M) {
	scanEvery = 50 * time.Millisecond
	leaseTTL = 300 * time.Millisecond
	resync = 200 * time.Millisecond
	os.Exit(m.Run())
}

// fakeLLM answers every request with a numbered line naming the agent,
// and remembers what it was asked.
type fakeLLM struct {
	mu    sync.Mutex
	calls []llm.Request
}

func (f *fakeLLM) provider(c llm.Config) (llm.Provider, error) {
	return fakeProvider{f: f, model: c.Model}, nil
}

type fakeProvider struct {
	f     *fakeLLM
	model string
}

func (p fakeProvider) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.calls = append(p.f.calls, req)
	return llm.Response{Text: fmt.Sprintf("%s says %d", p.model, len(p.f.calls)), InputTokens: 10, OutputTokens: 5}, nil
}

func (f *fakeLLM) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeLLM) last() llm.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

type env struct {
	t     *testing.T
	store store.Store
	bus   bus.Bus
	srv   *api.Server
	llm   *fakeLLM
	ada   core.Principal
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "say.db"))
	if err != nil {
		t.Fatal(err)
	}
	b := bus.NewInProc()
	t.Cleanup(func() {
		b.Close()
		st.Close()
	})
	e := &env{t: t, store: st, bus: b, llm: &fakeLLM{}}
	e.srv = api.New(api.Config{Version: "test", Store: st, Bus: b})
	e.ada = core.Principal{ID: core.NewID(), Kind: core.PrincipalHuman, DisplayName: "Ada"}
	if err := st.CreatePrincipal(context.Background(), e.ada); err != nil {
		t.Fatal(err)
	}
	return e
}

// runner starts a runner for holder that stops when the test ends or
// stop is called.
func (e *env) runner(holder string) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r := New(Config{Store: e.store, Bus: e.bus, Chat: e.srv, Holder: holder, NewProvider: e.llm.provider})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	e.t.Cleanup(stop)
	return stop
}

// agent creates a built-in agent whose replies start with its name.
func (e *env) agent(name string, budget int64) core.Principal {
	e.t.Helper()
	p := core.Principal{ID: core.NewID(), Kind: core.PrincipalAgent, DisplayName: name, CreatedAt: time.Now()}
	a := store.BuiltinAgent{Provider: llm.ProviderAnthropic, Model: name, MaxTokens: 100, DailyTokenBudget: budget, Enabled: true, SystemPrompt: "Be " + name + "."}
	if err := e.store.CreateBuiltinAgent(context.Background(), p, a); err != nil {
		e.t.Fatal(err)
	}
	e.bus.Publish(context.Background(), bus.BuiltinAgentsSubject, nil)
	return p
}

func (e *env) room(kind core.RoomKind, members ...core.Principal) core.Room {
	e.t.Helper()
	ctx := context.Background()
	r := core.Room{ID: core.NewID(), Kind: kind, Name: "lab"}
	if err := e.store.CreateRoom(ctx, r, core.Membership{RoomID: r.ID, PrincipalID: e.ada.ID, Role: core.RoleOwner}); err != nil {
		e.t.Fatal(err)
	}
	for _, m := range members {
		if err := e.store.AddMember(ctx, core.Membership{RoomID: r.ID, PrincipalID: m.ID, Role: core.RoleMember}); err != nil {
			e.t.Fatal(err)
		}
		e.bus.Publish(ctx, bus.RoomsSubject(m.ID), nil)
	}
	return r
}

func (e *env) responder(r core.Room, p core.Principal) {
	e.t.Helper()
	if err := e.store.SetMemberScopes(context.Background(), r.ID, p.ID, []string{core.ScopeResponder}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) say(r core.Room, text string) core.Event {
	e.t.Helper()
	ev, err := e.srv.SendText(context.Background(), e.ada, r.ID, text, "", "")
	if err != nil {
		e.t.Fatal(err)
	}
	return ev
}

func (e *env) events(r core.Room) []core.Event {
	e.t.Helper()
	evs, err := e.store.EventsAfter(context.Background(), r.ID, 0, 1000)
	if err != nil {
		e.t.Fatal(err)
	}
	return evs
}

// waitFor polls until ok holds for the room's events.
func (e *env) waitFor(r core.Room, what string, ok func([]core.Event) bool) []core.Event {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		evs := e.events(r)
		if ok(evs) {
			return evs
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s; events: %s", what, dump(evs))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// quiet waits until the room has had no new event for a while.
func (e *env) quiet(r core.Room) []core.Event {
	e.t.Helper()
	n, since := -1, time.Now()
	deadline := time.Now().Add(10 * time.Second)
	for time.Since(since) < 500*time.Millisecond {
		if time.Now().After(deadline) {
			e.t.Fatal("the room never went quiet")
		}
		if evs := e.events(r); len(evs) != n {
			n, since = len(evs), time.Now()
		}
		time.Sleep(20 * time.Millisecond)
	}
	return e.events(r)
}

func dump(evs []core.Event) string {
	var b strings.Builder
	for _, e := range evs {
		fmt.Fprintf(&b, "\n  #%d hop %d %q", e.Seq, e.Causation.HopCount, e.Text())
	}
	return b.String()
}

func setPolicy(t *testing.T, st store.Store, r core.Room, p core.AgentPolicy) {
	t.Helper()
	if err := st.SetAgentPolicy(context.Background(), r.ID, p); err != nil {
		t.Fatal(err)
	}
}

// The M1 check: two built-in agents that answer everything converse, and
// stop when their causation chain reaches the room's max_hop_count.
func TestTwoAgentsStopAtHopLimit(t *testing.T) {
	e := newEnv(t)
	e.runner("node-a")
	a, b := e.agent("Alpha", 0), e.agent("Beta", 0)
	r := e.room(core.RoomGroup, a, b)
	e.responder(r, a)
	e.responder(r, b)
	setPolicy(t, e.store, r, core.AgentPolicy{MaxHops: 4})
	time.Sleep(200 * time.Millisecond) // let both agents follow the room

	start := e.say(r, "Alpha and Beta, discuss.")
	evs := e.quiet(r)
	maxHop, by := 0, map[string]int{}
	for _, ev := range evs[1:] {
		if ev.Causation.CausedBy == "" {
			t.Fatalf("agent message without caused_by: %s", dump(evs))
		}
		maxHop = max(maxHop, ev.Causation.HopCount)
		by[ev.SenderID]++
	}
	if maxHop != 4 {
		t.Fatalf("longest chain = %d hops, want 4: %s", maxHop, dump(evs))
	}
	if by[a.ID] == 0 || by[b.ID] == 0 {
		t.Fatalf("both agents should have spoken: %v %s", by, dump(evs))
	}
	if evs[1].Causation.CausedBy != start.ID {
		t.Fatalf("first reply is not to the human: %s", dump(evs))
	}
	// The model saw the room as a transcript under its instructions.
	req := e.llm.last()
	if !strings.Contains(req.System, "Be Alpha.") && !strings.Contains(req.System, "Be Beta.") {
		t.Errorf("system prompt = %q", req.System)
	}
	if req.Messages[0].Role != "user" || !strings.HasPrefix(req.Messages[0].Text, "Ada: Alpha and Beta, discuss.") {
		t.Errorf("first turn = %+v", req.Messages[0])
	}
	if req.MaxTokens != 100 {
		t.Errorf("max tokens = %d", req.MaxTokens)
	}
	// No model call was made past the limit.
	if got := e.llm.count(); got != len(evs)-1 {
		t.Errorf("%d model calls for %d replies", got, len(evs)-1)
	}
}

func TestMentionsAndDMs(t *testing.T) {
	e := newEnv(t)
	e.runner("node-a")
	bot := e.agent("Bot", 0)
	r := e.room(core.RoomGroup, bot)
	dm := e.room(core.RoomDM, bot)
	time.Sleep(200 * time.Millisecond)

	e.say(r, "nobody asked the bot")
	ask := e.say(r, "@bot what is 2+2?")
	evs := e.waitFor(r, "a reply to the mention", func(evs []core.Event) bool { return len(evs) >= 3 })
	if evs[2].SenderID != bot.ID || evs[2].Causation.CausedBy != ask.ID || evs[2].Causation.HopCount != 1 {
		t.Fatalf("reply = %+v", evs[2])
	}
	// The earlier message is context, not a second trigger.
	if evs := e.quiet(r); len(evs) != 3 {
		t.Fatalf("want one reply: %s", dump(evs))
	}
	if got := e.llm.last().Messages[0].Text; got != "Ada: nobody asked the bot\n\nAda: @bot what is 2+2?" {
		t.Errorf("transcript = %q", got)
	}

	e.say(dm, "hi")
	e.waitFor(dm, "a reply in the DM", func(evs []core.Event) bool { return len(evs) == 2 && evs[1].SenderID == bot.ID })
}

func TestPausedRoomAndTokenBudget(t *testing.T) {
	e := newEnv(t)
	e.runner("node-a")
	bot := e.agent("Bot", 15) // one call uses 15 tokens
	r := e.room(core.RoomGroup, bot)
	time.Sleep(200 * time.Millisecond)

	setPolicy(t, e.store, r, core.AgentPolicy{Paused: true})
	e.say(r, "@Bot are you there?")
	if evs := e.quiet(r); len(evs) != 1 || e.llm.count() != 0 {
		t.Fatalf("a paused room got a reply or a model call: %d calls %s", e.llm.count(), dump(evs))
	}

	setPolicy(t, e.store, r, core.AgentPolicy{})
	e.say(r, "@Bot now?")
	e.waitFor(r, "a reply", func(evs []core.Event) bool { return len(evs) == 3 })
	e.say(r, "@Bot again?")
	if evs := e.quiet(r); len(evs) != 4 || e.llm.count() != 1 {
		t.Fatalf("the budget did not stop the agent: %d calls %s", e.llm.count(), dump(evs))
	}
	a, err := e.store.GetBuiltinAgent(context.Background(), bot.ID)
	if err != nil || !strings.Contains(a.LastError, "daily budget of 15 tokens") {
		t.Fatalf("last error = %q, %v", a.LastError, err)
	}
	if u, _ := e.store.AgentUsage(context.Background(), bot.ID, time.Now()); u.Total() != 15 {
		t.Fatalf("usage = %+v", u)
	}
}

// Two nodes: one holds the agent and replies once; when it stops, the
// other takes over.
func TestLeaseFailover(t *testing.T) {
	e := newEnv(t)
	stopA := e.runner("node-a")
	bot := e.agent("Bot", 0)
	time.Sleep(150 * time.Millisecond) // node-a takes the lease first
	e.runner("node-b")
	r := e.room(core.RoomGroup, bot)
	time.Sleep(200 * time.Millisecond)

	e.say(r, "@Bot one")
	if evs := e.quiet(r); len(evs) != 2 || e.llm.count() != 1 {
		t.Fatalf("want one reply from one node: %d calls %s", e.llm.count(), dump(evs))
	}
	stopA()
	time.Sleep(200 * time.Millisecond) // node-b takes the released lease
	e.say(r, "@Bot two")
	evs := e.waitFor(r, "a reply from node-b", func(evs []core.Event) bool { return len(evs) == 4 })
	if evs[3].SenderID != bot.ID {
		t.Fatalf("reply = %+v", evs[3])
	}
}

func TestDisabledAgentStops(t *testing.T) {
	e := newEnv(t)
	e.runner("node-a")
	bot := e.agent("Bot", 0)
	r := e.room(core.RoomGroup, bot)
	time.Sleep(200 * time.Millisecond)

	a, _ := e.store.GetBuiltinAgent(context.Background(), bot.ID)
	a.Enabled = false
	a.UpdatedAt = time.Now()
	if err := e.store.UpdateBuiltinAgent(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	e.bus.Publish(context.Background(), bus.BuiltinAgentsSubject, nil)
	time.Sleep(100 * time.Millisecond)
	e.say(r, "@Bot hello?")
	if evs := e.quiet(r); len(evs) != 1 {
		t.Fatalf("a disabled agent replied: %s", dump(evs))
	}
}

// A new agent answers a question asked just before it joined, but not an
// old one.
func TestJoinAnswersRecentQuestionOnly(t *testing.T) {
	e := newEnv(t)
	e.runner("node-a")
	bot := e.agent("Bot", 0)
	r := e.room(core.RoomGroup)
	_, err := e.store.AppendEvent(context.Background(), core.Event{
		RoomID: r.ID, Type: core.EventMessage, SenderID: e.ada.ID, Stream: core.StreamFinal,
		Parts:     []core.Part{{Kind: core.PartText, Data: []byte(`{"text":"@Bot old question"}`)}},
		CreatedAt: time.Now().Add(-time.Hour),
	}, store.AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.AddMember(context.Background(), core.Membership{RoomID: r.ID, PrincipalID: bot.ID, Role: core.RoleMember}); err != nil {
		t.Fatal(err)
	}
	e.bus.Publish(context.Background(), bus.RoomsSubject(bot.ID), nil)
	if evs := e.quiet(r); len(evs) != 1 {
		t.Fatalf("answered an hour-old question: %s", dump(evs))
	}
	recent := e.say(r, "@Bot new question")
	evs := e.waitFor(r, "a reply", func(evs []core.Event) bool { return len(evs) == 3 })
	if evs[2].Causation.CausedBy != recent.ID {
		t.Fatalf("reply = %+v", evs[2])
	}
}
