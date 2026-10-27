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

	"github.com/lum1n/say-server/internal/api"
	"github.com/lum1n/say-server/internal/bus"
	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/llm"
	"github.com/lum1n/say-server/internal/store"
)

func TestMain(m *testing.M) {
	scanEvery = 50 * time.Millisecond
	leaseTTL = 300 * time.Millisecond
	resync = 200 * time.Millisecond
	streamEvery = 20 * time.Millisecond
	os.Exit(m.Run())
}

// fakeLLM answers every request with a numbered line naming the agent,
// or with what script returns, and remembers what it was asked. It
// streams the answer a word at a time, pausing delay between words.
type fakeLLM struct {
	mu     sync.Mutex
	calls  []llm.Request
	script func(llm.Request) llm.Response
	delay  time.Duration
}

func (f *fakeLLM) provider(c llm.Config) (llm.Provider, error) {
	return fakeProvider{f: f, model: c.Model}, nil
}

type fakeProvider struct {
	f     *fakeLLM
	model string
}

func (p fakeProvider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	p.f.mu.Lock()
	p.f.calls = append(p.f.calls, req)
	resp := llm.Response{Text: fmt.Sprintf("%s says %d", p.model, len(p.f.calls))}
	if p.f.script != nil {
		resp = p.f.script(req)
	}
	resp.InputTokens, resp.OutputTokens = 10, 5
	delay := p.f.delay
	p.f.mu.Unlock()
	if req.OnText != nil {
		words := strings.SplitAfter(resp.Text, " ")
		for i := range words {
			if i > 0 {
				select {
				case <-ctx.Done():
					return llm.Response{InputTokens: 10}, ctx.Err()
				case <-time.After(delay):
				}
			}
			req.OnText(strings.Join(words[:i+1], ""))
		}
	}
	return resp, nil
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
func (e *env) agent(name string, budget int64, servers ...store.MCPServer) core.Principal {
	e.t.Helper()
	p := core.Principal{ID: core.NewID(), Kind: core.PrincipalAgent, DisplayName: name, CreatedAt: time.Now()}
	a := store.BuiltinAgent{Provider: llm.ProviderAnthropic, Model: name, MaxTokens: 100, DailyTokenBudget: budget, Enabled: true, SystemPrompt: "Be " + name + ".", MCPServers: servers}
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
	ev, _, err := e.srv.SendText(context.Background(), e.ada, r.ID, text, "", "", false)
	if err != nil {
		e.t.Fatal(err)
	}
	return ev
}

// events returns the room's final messages.
func (e *env) events(r core.Room) []core.Event {
	e.t.Helper()
	var msgs []core.Event
	for _, ev := range e.log(r) {
		if ev.Type == core.EventMessage && ev.Stream == core.StreamFinal {
			msgs = append(msgs, ev)
		}
	}
	return msgs
}

// log returns all of the room's events.
func (e *env) log(r core.Room) []core.Event {
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

// update changes a built-in agent's settings, and renames it unless
// rename is empty, the way the API does.
func (e *env) update(id, rename string, change func(*store.BuiltinAgent)) {
	e.t.Helper()
	ctx := context.Background()
	a, err := e.store.GetBuiltinAgent(ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	change(&a)
	a.UpdatedAt = a.UpdatedAt.Add(time.Millisecond)
	if err := e.store.UpdateBuiltinAgent(ctx, a, rename); err != nil {
		e.t.Fatal(err)
	}
	e.bus.Publish(ctx, bus.BuiltinAgentsSubject, nil)
	time.Sleep(100 * time.Millisecond)
}

func TestCostBudget(t *testing.T) {
	e := newEnv(t)
	e.runner("node-a")
	bot := e.agent("Bot", 0)
	r := e.room(core.RoomGroup, bot)
	// One call is 10 input and 5 output tokens: 30 + 75 micro-dollars.
	e.update(bot.ID, "", func(a *store.BuiltinAgent) {
		a.InputPrice, a.OutputPrice, a.DailyCostBudget = 3_000_000, 15_000_000, 105
	})

	e.say(r, "@Bot now?")
	e.waitFor(r, "a reply", func(evs []core.Event) bool { return len(evs) == 2 })
	e.say(r, "@Bot again?")
	if evs := e.quiet(r); len(evs) != 3 || e.llm.count() != 1 {
		t.Fatalf("the cost budget did not stop the agent: %d calls %s", e.llm.count(), dump(evs))
	}
	a, err := e.store.GetBuiltinAgent(context.Background(), bot.ID)
	if err != nil || !strings.Contains(a.LastError, "daily budget of $0.000105 is spent") {
		t.Fatalf("last error = %q, %v", a.LastError, err)
	}
	if u, _ := e.store.AgentUsage(context.Background(), bot.ID, time.Now()); u.Cost != 105 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestRenamedAgentAnswersToItsNewName(t *testing.T) {
	e := newEnv(t)
	e.runner("node-a")
	bot := e.agent("Bot", 0)
	r := e.room(core.RoomGroup, bot)
	time.Sleep(200 * time.Millisecond)
	e.update(bot.ID, "Sage", func(*store.BuiltinAgent) {})

	e.say(r, "@Bot are you there?")
	if evs := e.quiet(r); len(evs) != 1 {
		t.Fatalf("the old name got a reply: %s", dump(evs))
	}
	e.say(r, "@Sage are you there?")
	e.waitFor(r, "a reply to the new name", func(evs []core.Event) bool { return len(evs) == 3 && evs[2].SenderID == bot.ID })
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
	if err := e.store.UpdateBuiltinAgent(context.Background(), a, ""); err != nil {
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

func TestStreamsReplies(t *testing.T) {
	e := newEnv(t)
	e.llm.script = func(llm.Request) llm.Response { return llm.Response{Text: "one two three four five six"} }
	e.llm.delay = 40 * time.Millisecond
	e.runner("node-a")
	bot := e.agent("Bot", 0)
	dm := e.room(core.RoomDM, bot)
	var mu sync.Mutex
	var frames []string
	unsubscribe, _ := e.bus.Subscribe(bus.RoomStreamSubject(dm.ID), func(_ string, data []byte) {
		mu.Lock()
		frames = append(frames, string(data))
		mu.Unlock()
	})
	defer unsubscribe()
	time.Sleep(200 * time.Millisecond)

	ask := e.say(dm, "count")
	e.waitFor(dm, "the reply", func(evs []core.Event) bool { return len(evs) == 2 })
	e.quiet(dm)
	log := e.log(dm)
	if len(log) != 3 {
		t.Fatalf("want the question, the reply and its end: %s", dump(log))
	}
	reply, end := log[1], log[2]
	if reply.SenderID != bot.ID || reply.Causation.CausedBy != ask.ID || reply.Text() != "one two three four five six" || reply.Stream != core.StreamFinal {
		t.Fatalf("reply = %+v", reply)
	}
	if end.Type != core.EventEdit || end.Edits != reply.ID || end.Stream != core.StreamFinal || end.Text() != reply.Text() {
		t.Fatalf("end = %+v", end)
	}
	mu.Lock()
	defer mu.Unlock()
	// The first words start the message; later ones arrive as frames.
	if len(frames) < 3 || !strings.Contains(frames[0], `"stream":"streaming"`) || !strings.Contains(frames[len(frames)-1], "five") {
		t.Fatalf("frames = %q", frames)
	}
}

func TestAnswersAStreamWhenItEnds(t *testing.T) {
	e := newEnv(t)
	e.runner("node-a")
	bot := e.agent("Bot", 0)
	r := e.room(core.RoomGroup, bot)
	time.Sleep(200 * time.Millisecond)
	ctx := context.Background()

	msg, _, err := e.srv.SendText(ctx, e.ada, r.ID, "@bot what is", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.srv.StreamText(ctx, e.ada, r.ID, msg.ID, "@bot what is 2+"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if n := e.llm.count(); n != 0 {
		t.Fatalf("the bot answered a message still streaming (%d calls)", n)
	}
	if err := e.srv.EndText(ctx, e.ada, r.ID, msg.ID, "@bot what is 2+2?"); err != nil {
		t.Fatal(err)
	}
	evs := e.waitFor(r, "the reply", func(evs []core.Event) bool { return len(evs) == 2 })
	if evs[1].SenderID != bot.ID || evs[1].Causation.CausedBy != msg.ID {
		t.Fatalf("reply = %+v", evs[1])
	}
	if evs := e.quiet(r); len(evs) != 2 {
		t.Fatalf("want one reply: %s", dump(evs))
	}
	if got := e.llm.last().Messages[0].Text; got != "Ada: @bot what is 2+2?" {
		t.Errorf("transcript = %q", got)
	}
}

func TestEndsStreamsLeftBehind(t *testing.T) {
	e := newEnv(t)
	bot := e.agent("Bot", 0)
	r := e.room(core.RoomGroup, bot)
	// A node wrote part of a reply and died.
	parts := []core.Part{
		core.TextPart("half a"),
		core.ToolCall{ID: "c1", Name: "wx__forecast", Status: core.ToolDone}.Part(),
		core.ToolCall{ID: "c2", Name: "wx__forecast", Status: core.ToolRunning}.Part(),
	}
	msg, _, err := e.srv.SendParts(context.Background(), bot, r.ID, parts, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	e.runner("node-a")
	evs := e.waitFor(r, "the stream to end", func(evs []core.Event) bool { return len(evs) == 1 })
	// The call that was running never finished.
	if evs[0].ID != msg.ID || evs[0].Text() != "half a\n[used wx__forecast]\n[used wx__forecast, failed]" {
		t.Fatalf("message: %s", dump(evs))
	}
	if e.llm.count() != 0 {
		t.Fatal("ending a stream called the model")
	}
}
