package signal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vegard/say-v2/internal/bridge"
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeEnv) != "" {
		fakeSignalCLI()
		return
	}
	// signal-cli processes started by the tests are this binary.
	os.Setenv(fakeEnv, "1")
	os.Exit(m.Run())
}

// host is a fake core.
type host struct {
	in        chan bridge.Incoming
	prompts   chan bridge.Prompt
	cleared   chan struct{} // an Ask ended without an answer
	connected chan struct{}

	mu    sync.Mutex
	state json.RawMessage
	once  sync.Once
}

func newHost(buffer int) *host {
	return &host{
		in:        make(chan bridge.Incoming, buffer),
		prompts:   make(chan bridge.Prompt, 4),
		cleared:   make(chan struct{}, 4),
		connected: make(chan struct{}),
	}
}

func (h *host) Receive(ctx context.Context, m bridge.Incoming) error {
	h.in <- m
	return nil
}

func (h *host) Ask(ctx context.Context, p bridge.Prompt) (string, error) {
	h.prompts <- p
	<-ctx.Done()
	h.cleared <- struct{}{}
	return "", ctx.Err()
}

func (h *host) SaveState(ctx context.Context, s json.RawMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state = s
	return nil
}

func (h *host) saved() state {
	h.mu.Lock()
	defer h.mu.Unlock()
	var s state
	json.Unmarshal(h.state, &s)
	return s
}

func (h *host) Chats(ctx context.Context) ([]string, error) { return nil, nil }

func (h *host) Connected(ctx context.Context) { h.once.Do(func() { close(h.connected) }) }

// bridgeRun is a running connection to a fake signal-cli.
type bridgeRun struct {
	t    *testing.T
	dir  string
	h    *host
	c    *conn
	stop context.CancelFunc
	done chan error
}

// start runs a connection; linked says whether signal-cli already has the
// account.
func start(t *testing.T, linked bool, st *state) *bridgeRun {
	return startWith(t, newHost(32), linked, st)
}

func startWith(t *testing.T, h *host, linked bool, st *state) *bridgeRun {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if linked {
		os.WriteFile(filepath.Join(dir, "account"), []byte(fakeNumber), 0o600)
	}
	cfg, _ := json.Marshal(config{Path: exe, DataDir: dir})
	info := bridge.Info{ID: "b1", Config: cfg}
	if st != nil {
		info.State, _ = json.Marshal(st)
	}
	c, err := open(info, h)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &bridgeRun{t: t, dir: dir, h: h, c: c.(*conn), stop: cancel, done: make(chan error, 1)}
	go func() { r.done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(15 * time.Second):
			t.Error("Run did not return")
		}
	})
	return r
}

func (r *bridgeRun) waitConnected() {
	r.t.Helper()
	select {
	case <-r.h.connected:
	case err := <-r.done:
		r.t.Fatalf("Run returned: %v", err)
	case <-time.After(10 * time.Second):
		r.t.Fatal("not connected")
	}
}

var pushes int

// push makes the fake signal-cli deliver an envelope.
func (r *bridgeRun) push(env string) {
	r.t.Helper()
	pushes++
	params := fmt.Sprintf(`{"account":%q,"envelope":%s}`, fakeNumber, env)
	tmp := filepath.Join(r.dir, "tmp")
	if err := os.WriteFile(tmp, []byte(params), 0o600); err != nil {
		r.t.Fatal(err)
	}
	os.Rename(tmp, filepath.Join(r.dir, fmt.Sprintf("push-%04d.json", pushes)))
}

func (r *bridgeRun) next() bridge.Incoming {
	r.t.Helper()
	select {
	case m := <-r.h.in:
		return m
	case <-time.After(5 * time.Second):
		r.t.Fatal("no message")
		return bridge.Incoming{}
	}
}

// requests returns the params of the requests of a method the fake got.
func (r *bridgeRun) requests(method string) []map[string]any {
	raw, _ := os.ReadFile(filepath.Join(r.dir, "requests.jsonl"))
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var w wire
		if json.Unmarshal([]byte(line), &w) != nil || w.Method != method {
			continue
		}
		var p map[string]any
		json.Unmarshal(w.Params, &p)
		out = append(out, p)
	}
	return out
}

func TestLinkShowsQRUntilLinked(t *testing.T) {
	r := start(t, false, nil)
	var p bridge.Prompt
	select {
	case p = <-r.h.prompts:
	case err := <-r.done:
		t.Fatalf("Run returned: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no QR prompt")
	}
	if p.Kind != "qr" || !strings.HasPrefix(p.Data, "sgnl://linkdevice") {
		t.Fatalf("prompt %+v", p)
	}
	select {
	case <-r.h.connected:
		t.Fatal("connected before the phone scanned")
	case <-time.After(50 * time.Millisecond):
	}
	os.WriteFile(filepath.Join(r.dir, "scanned"), nil, 0o600)
	select {
	case <-r.h.cleared:
	case <-time.After(5 * time.Second):
		t.Fatal("the QR prompt was not cleared")
	}
	r.waitConnected()
	if s := r.h.saved(); s.Account != fakeNumber || s.UUID != fakeUUID {
		t.Fatalf("state %+v", s)
	}
	links := r.requests("finishLink")
	if len(links) != 1 || links[0]["deviceName"] != "say" || links[0]["deviceLinkUri"] != p.Data {
		t.Fatalf("finishLink %v", links)
	}
}

func TestOutdatedSignalCLISaysSo(t *testing.T) {
	r := start(t, false, nil)
	select {
	case <-r.h.prompts:
	case err := <-r.done:
		t.Fatalf("Run returned: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no QR prompt")
	}
	os.WriteFile(filepath.Join(r.dir, "outdated"), nil, 0o600)
	select {
	case err := <-r.done:
		r.done <- err // for the cleanup
		if err == nil || !strings.Contains(err.Error(), "signal-cli is too old") {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestLinkedAccountSkipsLinking(t *testing.T) {
	r := start(t, true, &state{Account: fakeNumber})
	r.waitConnected()
	if len(r.requests("startLink")) != 0 {
		t.Fatal("linked again")
	}
	select {
	case p := <-r.h.prompts:
		t.Fatalf("asked %+v", p)
	default:
	}
}

func TestReceive(t *testing.T) {
	r := start(t, true, &state{Account: fakeNumber})
	r.waitConnected()

	// A direct message with a mention and an attachment.
	r.push(`{"source":"+4711111111","sourceNumber":"+4711111111","sourceUuid":"u-bob","sourceName":"Bobby","timestamp":1000,
		"dataMessage":{"timestamp":1000,"message":"hi ￼!","mentions":[{"name":"+4700000001","number":"+4700000001","uuid":"u-me","start":3,"length":1}],
		"attachments":[{"contentType":"image/jpeg","filename":"cat.jpg"}]}}`)
	m := r.next()
	want := bridge.Incoming{
		Chat:   bridge.RemoteChat{ID: "user:u-bob", Name: "Bob", Direct: true},
		ID:     "u-bob:1000",
		Author: bridge.Author{ID: "u-bob", Name: "Bob"},
		Text:   "hi @" + fakeNumber + "!\n[attachment: cat.jpg]",
		Time:   time.UnixMilli(1000),
	}
	if !equal(m, want) {
		t.Fatalf("got %+v\nwant %+v", m, want)
	}

	// Typing, receipts and reactions are skipped.
	r.push(`{"sourceUuid":"u-bob","timestamp":1001,"typingMessage":{"action":"STARTED"}}`)
	r.push(`{"sourceUuid":"u-bob","timestamp":1002,"dataMessage":{"timestamp":1002,"reaction":{"emoji":"👍","targetSentTimestamp":1000}}}`)

	// A group message, quoting one of the account's own.
	r.push(`{"sourceUuid":"u-bob","sourceNumber":"+4711111111","timestamp":2000,
		"dataMessage":{"timestamp":2000,"message":"yes","groupInfo":{"groupId":"g1","type":"DELIVER"},
		"quote":{"id":1500,"authorUuid":"u-me","authorNumber":"+4700000001","text":"lunch?"}}}`)
	m = r.next()
	want = bridge.Incoming{
		Chat:    bridge.RemoteChat{ID: "group:g1", Name: "Friends"},
		ID:      "u-bob:2000",
		Author:  bridge.Author{ID: "u-bob", Name: "Bob"},
		Text:    "> lunch?\nyes",
		Time:    time.UnixMilli(2000),
		ReplyTo: fakeNumber + ":1500",
	}
	if !equal(m, want) {
		t.Fatalf("got %+v\nwant %+v", m, want)
	}

	// Sent by the person from their phone.
	r.push(`{"sourceUuid":"u-me","sourceNumber":"+4700000001","timestamp":3000,
		"syncMessage":{"sentMessage":{"destination":"+4711111111","destinationNumber":"+4711111111","destinationUuid":"u-bob","timestamp":3000,"message":"from my phone"}}}`)
	m = r.next()
	want = bridge.Incoming{
		Chat:   bridge.RemoteChat{ID: "user:u-bob", Name: "Bob", Direct: true},
		ID:     fakeNumber + ":3000",
		Author: bridge.Author{ID: fakeNumber, Self: true},
		Text:   "from my phone",
		Time:   time.UnixMilli(3000),
	}
	if !equal(m, want) {
		t.Fatalf("got %+v\nwant %+v", m, want)
	}

	// An edit by Bob, and one by the person from their phone.
	r.push(`{"sourceUuid":"u-bob","timestamp":4000,"editMessage":{"targetSentTimestamp":1000,"dataMessage":{"timestamp":4000,"message":"hello"}}}`)
	m = r.next()
	if !m.Edit || m.ID != "u-bob:1000" || m.Text != "hello" || m.Chat.ID != "user:u-bob" {
		t.Fatalf("edit %+v", m)
	}
	r.push(`{"sourceUuid":"u-me","timestamp":5000,"syncMessage":{"sentMessage":{"destinationUuid":"u-bob",
		"editMessage":{"targetSentTimestamp":3000,"dataMessage":{"timestamp":5000,"message":"from my phone!"}}}}}`)
	m = r.next()
	if !m.Edit || m.ID != fakeNumber+":3000" || !m.Author.Self || m.Text != "from my phone!" {
		t.Fatalf("self edit %+v", m)
	}

	// Another account in the same data directory is not this bridge's.
	r.push(`{"sourceUuid":"u-bob","timestamp":6000,"dataMessage":{"timestamp":6000,"message":"x"}},"account":"+4799999999"`)
	r.push(`{"sourceUuid":"u-carol","sourceName":"Carol","timestamp":7000,"dataMessage":{"timestamp":7000,"message":"last"}}`)
	m = r.next()
	if m.ID != "u-carol:7000" || m.Author.Name != "Carol" || m.Chat.Name != "Carol" {
		t.Fatalf("got %+v", m)
	}
}

func equal(a, b bridge.Incoming) bool {
	return a.Chat == b.Chat && a.ID == b.ID && a.Author == b.Author && a.Text == b.Text &&
		a.Time.Equal(b.Time) && a.Edit == b.Edit && a.ReplyTo == b.ReplyTo
}

func TestSend(t *testing.T) {
	r := start(t, true, &state{Account: fakeNumber})
	r.waitConnected()
	ctx := context.Background()

	id, err := r.c.Send(ctx, bridge.Outgoing{Chat: "user:u-bob", Text: "hi"})
	if err != nil || id != fakeNumber+":1700000000001" {
		t.Fatalf("send: %q %v", id, err)
	}
	id, err = r.c.Send(ctx, bridge.Outgoing{Chat: "group:g1", Text: "all", ReplyTo: "u-bob:2000"})
	if err != nil || id != fakeNumber+":1700000000002" {
		t.Fatalf("send to group: %q %v", id, err)
	}
	if err := r.c.Edit(ctx, bridge.Outgoing{Chat: "user:u-bob", Text: "hi!"}, fakeNumber+":1700000000001"); err != nil {
		t.Fatal(err)
	}
	sends := r.requests("send")
	if len(sends) != 3 {
		t.Fatalf("sends %v", sends)
	}
	if s := sends[0]; s["account"] != fakeNumber || s["message"] != "hi" || fmt.Sprint(s["recipient"]) != "[u-bob]" {
		t.Fatalf("send %v", s)
	}
	if s := sends[1]; s["groupId"] != "g1" || s["quoteAuthor"] != "u-bob" || s["quoteTimestamp"] != float64(2000) {
		t.Fatalf("group send %v", s)
	}
	if s := sends[2]; s["editTimestamp"] != float64(1700000000001) || s["message"] != "hi!" {
		t.Fatalf("edit %v", s)
	}

	if _, err := r.c.Send(ctx, bridge.Outgoing{Chat: "user:u-gone", Text: "hi"}); !errors.Is(err, bridge.ErrRejected) {
		t.Fatalf("unregistered: %v", err)
	}
	if _, err := r.c.Send(ctx, bridge.Outgoing{Chat: "user:u-flaky", Text: "hi"}); err == nil || errors.Is(err, bridge.ErrRejected) {
		t.Fatalf("network failure: %v", err)
	}
	if _, err := r.c.Send(ctx, bridge.Outgoing{Chat: "nope", Text: "hi"}); !errors.Is(err, bridge.ErrRejected) {
		t.Fatalf("bad chat: %v", err)
	}
}

// A send completes while the core is still taking in a message, as the
// core's per-chat lock needs.
func TestSendWhileReceiving(t *testing.T) {
	r := startWith(t, newHost(0), true, &state{Account: fakeNumber})
	r.waitConnected()
	r.push(`{"sourceUuid":"u-bob","timestamp":1000,"dataMessage":{"timestamp":1000,"message":"one"}}`)
	r.push(`{"sourceUuid":"u-bob","timestamp":1001,"dataMessage":{"timestamp":1001,"message":"two"}}`)
	// Nobody takes the messages yet: Receive blocks on the first.
	time.Sleep(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.c.Send(ctx, bridge.Outgoing{Chat: "user:u-bob", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if r.next().Text != "one" || r.next().Text != "two" {
		t.Fatal("messages out of order")
	}
}

func TestExitIsAnError(t *testing.T) {
	r := start(t, true, &state{Account: fakeNumber})
	r.waitConnected()
	os.WriteFile(filepath.Join(r.dir, "die"), nil, 0o600)
	select {
	case err := <-r.done:
		r.done <- err // for the cleanup
		if err == nil || !strings.Contains(err.Error(), "boom: database is locked") {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	if _, err := r.c.Send(context.Background(), bridge.Outgoing{Chat: "user:u-bob", Text: "hi"}); err == nil {
		t.Fatal("sent while disconnected")
	}
}

func TestStopEndsProcess(t *testing.T) {
	r := start(t, true, &state{Account: fakeNumber})
	r.waitConnected()
	r.stop()
	select {
	case err := <-r.done:
		r.done <- err
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestDefaultDataDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/data")
	c, err := open(bridge.Info{ID: "b9"}, newHost(0))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := c.(*conn).dataDir()
	if err != nil || dir != "/data/say/signal/b9" {
		t.Fatalf("%q %v", dir, err)
	}
	if cc := c.(*conn); cc.cfg.Path != "signal-cli" || cc.cfg.DeviceName != "say" {
		t.Fatalf("defaults %+v", cc.cfg)
	}
}

// TestRealSignalCLI starts linking with the installed signal-cli and stops
// at the QR code: linking itself needs a phone. Set SAY_TEST_SIGNAL_CLI to
// signal-cli's path to run it; it reaches Signal's servers.
func TestRealSignalCLI(t *testing.T) {
	path := os.Getenv("SAY_TEST_SIGNAL_CLI")
	if path == "" {
		t.Skip("SAY_TEST_SIGNAL_CLI is not set")
	}
	cfg, _ := json.Marshal(config{Path: path, DataDir: t.TempDir()})
	h := newHost(0)
	c, err := open(bridge.Info{ID: "real", Config: cfg}, h)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	select {
	case p := <-h.prompts:
		if p.Kind != "qr" || !strings.HasPrefix(p.Data, "sgnl://linkdevice?") {
			t.Fatalf("prompt %+v", p)
		}
	case err := <-done:
		t.Fatalf("Run: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("no QR code")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("signal-cli did not stop")
	}
}

func TestPartlyDeliveredGroupSendIsSent(t *testing.T) {
	r := start(t, true, &state{Account: fakeNumber})
	r.waitConnected()
	id, err := r.c.Send(context.Background(), bridge.Outgoing{Chat: "group:g-partial", Text: "hi all"})
	if err != nil || id != fakeNumber+":1700000000999" {
		t.Fatalf("send: %q %v", id, err)
	}
}

func TestSendError(t *testing.T) {
	results := func(code int, ts int64, types ...string) error {
		var rs []string
		for _, ty := range types {
			rs = append(rs, fmt.Sprintf(`{"type":%q}`, ty))
		}
		data := fmt.Sprintf(`{"response":{"timestamp":%d,"results":[%s]}}`, ts, strings.Join(rs, ","))
		return &rpcError{Code: code, Message: "Failed", Data: json.RawMessage(data)}
	}
	for _, tc := range []struct {
		name     string
		err      error
		ts       int64
		rejected bool
	}{
		{"partly delivered", results(-1, 7, "SUCCESS", "UNREGISTERED_FAILURE"), 7, false},
		{"partly untrusted", results(-4, 7, "IDENTITY_FAILURE", "SUCCESS"), 7, false},
		{"partly rate limited", results(-5, 7, "RATE_LIMIT_FAILURE", "SUCCESS"), 7, false},
		{"unregistered", results(-1, 7, "UNREGISTERED_FAILURE"), 0, true},
		{"untrusted", results(-4, 7, "IDENTITY_FAILURE"), 0, true},
		{"rate limited", results(-5, 7, "RATE_LIMIT_FAILURE"), 0, true},
		{"network failure", results(-1, 7, "NETWORK_FAILURE", "UNREGISTERED_FAILURE"), 0, false},
		{"refused before sending", &rpcError{Code: -1, Message: "Invalid group id"}, 0, true},
		{"bad params", &rpcError{Code: -32602, Message: "Unknown group"}, 0, true},
		{"io error", &rpcError{Code: -3, Message: "timeout"}, 0, false},
		{"not rpc", errors.New("closed"), 0, false},
	} {
		ts, err := sendError(tc.err)
		if ts != tc.ts || (ts == 0) != (err != nil) || errors.Is(err, bridge.ErrRejected) != tc.rejected {
			t.Errorf("%s: %d %v", tc.name, ts, err)
		}
	}
}

// One person is one chat and one author, whether Signal names them by
// number, uuid or both.
func TestPersonIDs(t *testing.T) {
	r := start(t, true, &state{Account: fakeNumber})
	r.waitConnected()

	// Bob's number is in the contacts with his uuid.
	r.push(`{"source":"+4711111111","sourceNumber":"+4711111111","timestamp":1000,"dataMessage":{"timestamp":1000,"message":"by number"}}`)
	m := r.next()
	if m.Chat.ID != "user:u-bob" || m.Author.ID != "u-bob" || m.ID != "u-bob:1000" {
		t.Fatalf("number only: %+v", m)
	}
	r.push(`{"sourceNumber":"+4711111111","timestamp":1100,"editMessage":{"targetSentTimestamp":1000,"dataMessage":{"timestamp":1100,"message":"edited"}}}`)
	if m = r.next(); !m.Edit || m.ID != "u-bob:1000" || m.Chat.ID != "user:u-bob" {
		t.Fatalf("edit by number: %+v", m)
	}
	r.push(`{"sourceUuid":"u-me","timestamp":1200,"syncMessage":{"sentMessage":{"destination":"+4711111111","destinationNumber":"+4711111111","timestamp":1200,"message":"to his number"}}}`)
	if m = r.next(); m.Chat.ID != "user:u-bob" {
		t.Fatalf("sync to number: %+v", m)
	}

	// Dan is no contact; an envelope with both teaches his number.
	r.push(`{"source":"+4733333333","sourceNumber":"+4733333333","sourceUuid":"u-dan","timestamp":2000,"dataMessage":{"timestamp":2000,"message":"both"}}`)
	r.push(`{"source":"+4733333333","sourceNumber":"+4733333333","timestamp":2100,"dataMessage":{"timestamp":2100,"message":"number"}}`)
	for _, ts := range []string{"2000", "2100"} {
		if m = r.next(); m.Chat.ID != "user:u-dan" || m.ID != "u-dan:"+ts {
			t.Fatalf("dan: %+v", m)
		}
	}

	// Only a number is ever known: it is the id.
	r.push(`{"source":"+4744444444","sourceNumber":"+4744444444","timestamp":3000,"dataMessage":{"timestamp":3000,"message":"eve"}}`)
	if m = r.next(); m.Chat.ID != "user:+4744444444" || m.Author.ID != "+4744444444" {
		t.Fatalf("number only: %+v", m)
	}
	// A uuid in "source" is a uuid.
	r.push(`{"source":"u-fay","timestamp":4000,"dataMessage":{"timestamp":4000,"message":"fay"}}`)
	if m = r.next(); m.Chat.ID != "user:u-fay" {
		t.Fatalf("uuid source: %+v", m)
	}
}
