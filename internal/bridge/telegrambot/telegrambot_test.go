package telegrambot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lum1n/say-server/internal/bridge"
)

const token = "123456:SECRETtoken_abc"

// reply is a canned answer of the fake Bot API.
type reply struct {
	status int
	body   any
}

// fakeAPI is the Bot API.
type fakeAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	updates []map[string]any
	calls   []call
	offsets []int64
	// canned answers per method, used before the normal ones.
	canned map[string][]reply
	nextID int64
}

// call is a request the fake got.
type call struct {
	method string
	params map[string]any
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, canned: map[string][]reply{}, nextID: 100}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(code int, desc string) reply {
	return reply{code, map[string]any{"ok": false, "error_code": code, "description": desc}}
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/bot")
	tok, method, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 || tok != token {
		write(w, 401, map[string]any{"ok": false, "error_code": 401, "description": "Unauthorized"})
		return
	}
	var params map[string]any
	json.NewDecoder(r.Body).Decode(&params)
	f.mu.Lock()
	f.calls = append(f.calls, call{method, params})
	if c := f.canned[method]; len(c) > 0 {
		f.canned[method] = c[1:]
		f.mu.Unlock()
		write(w, c[0].status, c[0].body)
		return
	}
	switch method {
	case "getMe":
		f.mu.Unlock()
		write(w, 200, map[string]any{"ok": true, "result": map[string]any{"id": 42, "is_bot": true, "first_name": "Say", "username": "say_test_bot"}})
	case "getUpdates":
		offset := int64(params["offset"].(float64))
		f.offsets = append(f.offsets, offset)
		var out []map[string]any
		for _, u := range f.updates {
			if int64(u["update_id"].(int)) >= offset {
				out = append(out, u)
			}
		}
		f.mu.Unlock()
		if len(out) == 0 {
			// A short long poll.
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
		write(w, 200, map[string]any{"ok": true, "result": out})
	case "sendMessage":
		f.nextID++
		id := f.nextID
		f.mu.Unlock()
		write(w, 200, map[string]any{"ok": true, "result": map[string]any{"message_id": id}})
	case "editMessageText":
		f.mu.Unlock()
		write(w, 200, map[string]any{"ok": true, "result": map[string]any{"message_id": params["message_id"]}})
	default:
		f.mu.Unlock()
		write(w, 404, map[string]any{"ok": false, "error_code": 404, "description": "Not Found"})
	}
}

func (f *fakeAPI) push(u ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, u...)
}

func (f *fakeAPI) can(method string, r ...reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canned[method] = append(f.canned[method], r...)
}

func (f *fakeAPI) callsOf(method string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeAPI) firstOffset() (int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.offsets) == 0 {
		return 0, false
	}
	return f.offsets[0], true
}

// fakeHost is the core.
type fakeHost struct {
	chats []string

	mu        sync.Mutex
	got       []bridge.Incoming
	states    []string
	connected int
}

func (h *fakeHost) Receive(ctx context.Context, m bridge.Incoming) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, m)
	return nil
}

func (h *fakeHost) Ask(ctx context.Context, p bridge.Prompt) (string, error) {
	return "", errors.New("no prompts expected")
}

func (h *fakeHost) SaveState(ctx context.Context, state json.RawMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.states = append(h.states, string(state))
	return nil
}

func (h *fakeHost) Chats(ctx context.Context) ([]string, error) { return h.chats, nil }

func (h *fakeHost) Connected(ctx context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.connected++
}

func (h *fakeHost) received() []bridge.Incoming {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]bridge.Incoming(nil), h.got...)
}

func (h *fakeHost) lastState() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.states) == 0 {
		return ""
	}
	return h.states[len(h.states)-1]
}

func newConn(t *testing.T, f *fakeAPI, h bridge.Host, st string) *conn {
	t.Helper()
	bc, err := open(bridge.Info{ID: "b1", Mode: "bot", Config: json.RawMessage(`{"bot_token":"` + token + `"}`), State: json.RawMessage(st)}, h)
	if err != nil {
		t.Fatal(err)
	}
	c := bc.(*conn)
	c.api = f.srv.URL + "/"
	return c
}

// run runs c until the test ends and returns a stop that waits for it.
func run(t *testing.T, c *conn) (stop func() error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Error("Run did not return")
			}
		})
		return err
	}
	t.Cleanup(func() { stop() })
	return stop
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var group = map[string]any{"id": -1001, "type": "supergroup", "title": "Crew"}

func ada() map[string]any {
	return map[string]any{"id": 7, "is_bot": false, "first_name": "Ada", "last_name": "Lovelace", "username": "ada"}
}

func TestRun(t *testing.T) {
	f := newFakeAPI(t)
	f.push(
		map[string]any{"update_id": 1, "message": map[string]any{
			"message_id": 10, "from": ada(), "chat": group, "date": 1700000000,
			"caption": "look", "photo": []any{map[string]any{"file_id": "x"}},
			"reply_to_message": map[string]any{"message_id": 9, "chat": group, "date": 1},
		}},
		map[string]any{"update_id": 2, "message": map[string]any{
			"message_id": 11, "from": map[string]any{"id": 8, "first_name": "", "username": "bob"}, "chat": group, "date": 1700000001,
			"document": map[string]any{"file_id": "y", "file_name": "plan.pdf"},
		}},
		map[string]any{"update_id": 3, "message": map[string]any{
			"message_id": 5, "from": ada(), "chat": map[string]any{"id": -2002, "type": "group", "title": "Elsewhere"}, "date": 1700000002, "text": "not linked",
		}},
		map[string]any{"update_id": 4, "edited_message": map[string]any{
			"message_id": 10, "from": ada(), "chat": group, "date": 1700000000, "edit_date": 1700000100, "text": "look again",
		}},
		map[string]any{"update_id": 5, "channel_post": map[string]any{
			"message_id": 3, "chat": map[string]any{"id": -1003, "type": "channel", "title": "News"},
			"sender_chat": map[string]any{"id": -1003, "type": "channel", "title": "News"}, "date": 1700000003,
			"sticker": map[string]any{"file_id": "z", "emoji": "😀"},
		}},
		map[string]any{"update_id": 6, "message": map[string]any{
			"message_id": 12, "from": ada(), "chat": group, "date": 1700000004,
			"new_chat_members": []any{ada()},
		}},
	)
	h := &fakeHost{chats: []string{"-1001", "-1003"}}
	c := newConn(t, f, h, "")
	run(t, c)

	eventually(t, "the offset is saved", func() bool { return h.lastState() == `{"offset":7}` })
	if h.connected != 1 {
		t.Errorf("connected %d times", h.connected)
	}
	got := h.received()
	if len(got) != 4 {
		t.Fatalf("got %d messages: %+v", len(got), got)
	}
	want := bridge.Incoming{
		Chat: bridge.RemoteChat{ID: "-1001", Name: "Crew"}, ID: "10",
		Author: bridge.Author{ID: "7", Name: "Ada Lovelace"}, Text: "look", ReplyTo: "9", Time: time.Unix(1700000000, 0),
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("photo message = %+v", got[0])
	}
	if got[1].Text != "[document: plan.pdf]" || got[1].Author != (bridge.Author{ID: "8", Name: "bob"}) || got[1].ReplyTo != "" {
		t.Errorf("document = %+v", got[1])
	}
	if !got[2].Edit || got[2].ID != "10" || got[2].Text != "look again" {
		t.Errorf("edit = %+v", got[2])
	}
	if got[3].Chat.ID != "-1003" || got[3].Author != (bridge.Author{ID: "-1003", Name: "News"}) || got[3].Text != "[sticker 😀]" {
		t.Errorf("channel post = %+v", got[3])
	}
	gu := f.callsOf("getUpdates")[0].params
	if gu["timeout"] != float64(50) || len(gu["allowed_updates"].([]any)) != 4 {
		t.Errorf("getUpdates params = %v", gu)
	}
}

func TestOffsetAfterRestart(t *testing.T) {
	f := newFakeAPI(t)
	f.push(
		map[string]any{"update_id": 9, "message": map[string]any{"message_id": 1, "from": ada(), "chat": group, "date": 1, "text": "old"}},
		map[string]any{"update_id": 10, "message": map[string]any{"message_id": 2, "from": ada(), "chat": group, "date": 2, "text": "new"}},
	)
	h := &fakeHost{chats: []string{"-1001"}}
	c := newConn(t, f, h, `{"offset":10}`)
	run(t, c)
	eventually(t, "the offset is saved", func() bool { return h.lastState() == `{"offset":11}` })
	if o, _ := f.firstOffset(); o != 10 {
		t.Errorf("first offset asked = %d", o)
	}
	if got := h.received(); len(got) != 1 || got[0].Text != "new" {
		t.Errorf("got %+v", got)
	}
}

func TestChatID(t *testing.T) {
	f := newFakeAPI(t)
	f.push(
		map[string]any{"update_id": 1, "message": map[string]any{"message_id": 4, "from": ada(), "chat": group, "date": 1, "text": "/chatid@Say_Test_Bot"}},
		map[string]any{"update_id": 2, "message": map[string]any{"message_id": 5, "from": ada(), "chat": group, "date": 1, "text": "/chatid@other_bot"}},
	)
	h := &fakeHost{chats: []string{"-1001"}}
	c := newConn(t, f, h, "")
	run(t, c)
	eventually(t, "the offset is saved", func() bool { return h.lastState() == `{"offset":3}` })
	sent := f.callsOf("sendMessage")
	if len(sent) != 1 {
		t.Fatalf("sent %d messages", len(sent))
	}
	p := sent[0].params
	if p["chat_id"] != "-1001" || !strings.Contains(p["text"].(string), "<code>-1001</code>") ||
		p["reply_parameters"].(map[string]any)["message_id"] != float64(4) {
		t.Errorf("answer = %v", p)
	}
	// Only the one addressed to another bot comes in.
	if got := h.received(); len(got) != 1 || got[0].ID != "5" {
		t.Errorf("got %+v", got)
	}
}

func TestTopics(t *testing.T) {
	f := newFakeAPI(t)
	forum := map[string]any{"id": -1001, "type": "supergroup", "title": "Crew", "is_forum": true}
	inTopic := func(update, id, topic int, text string) map[string]any {
		return map[string]any{"update_id": update, "message": map[string]any{
			"message_id": id, "message_thread_id": topic, "is_topic_message": true, "from": ada(), "chat": forum, "date": 1, "text": text,
			"reply_to_message": map[string]any{"message_id": topic, "chat": forum, "date": 1},
		}}
	}
	f.push(
		inTopic(1, 50, 42, "in the linked topic"),
		inTopic(2, 51, 7, "in another topic"),
		map[string]any{"update_id": 3, "message": map[string]any{"message_id": 52, "from": ada(), "chat": forum, "date": 1, "text": "in General"}},
		inTopic(4, 53, 42, "/chatid"),
	)
	h := &fakeHost{chats: []string{"-1001", "-1001/42"}}
	c := newConn(t, f, h, "")
	run(t, c)
	eventually(t, "the offset is saved", func() bool { return h.lastState() == `{"offset":5}` })
	got := h.received()
	if len(got) != 3 {
		t.Fatalf("got %d messages: %+v", len(got), got)
	}
	for i, chat := range []string{"-1001/42", "-1001", "-1001"} {
		if got[i].Chat.ID != chat || got[i].ReplyTo != "" {
			t.Errorf("message %d = %+v, want chat %s", i, got[i], chat)
		}
	}
	answer := f.callsOf("sendMessage")[0].params
	if answer["chat_id"] != "-1001" || answer["message_thread_id"] != float64(42) || !strings.Contains(answer["text"].(string), "<code>-1001/42</code>") {
		t.Errorf("/chatid answer = %v", answer)
	}

	if _, err := c.Send(context.Background(), bridge.Outgoing{Chat: "-1001/42", Sender: "Ada", Text: "hi topic"}); err != nil {
		t.Fatal(err)
	}
	if p := f.callsOf("sendMessage")[1].params; p["chat_id"] != "-1001" || p["message_thread_id"] != float64(42) {
		t.Errorf("send = %v", p)
	}
	if _, err := c.Send(context.Background(), bridge.Outgoing{Chat: "-1001", Sender: "Ada", Text: "hi group"}); err != nil {
		t.Fatal(err)
	}
	if p := f.callsOf("sendMessage")[2].params; p["chat_id"] != "-1001" || p["message_thread_id"] != nil {
		t.Errorf("send to the group = %v", p)
	}
	if err := c.Edit(context.Background(), bridge.Outgoing{Chat: "-1001/42", Sender: "Ada", Text: "edited"}, "101"); err != nil {
		t.Fatal(err)
	}
	if p := f.callsOf("editMessageText")[0].params; p["chat_id"] != "-1001" {
		t.Errorf("edit = %v", p)
	}
}

func TestSend(t *testing.T) {
	f := newFakeAPI(t)
	c := newConn(t, f, &fakeHost{}, "")
	id, err := c.Send(context.Background(), bridge.Outgoing{Chat: "-1001", Sender: `Ada <&> "L"`, Text: "1 < 2 & <b>x</b>", ReplyTo: "9"})
	if err != nil || id != "101" {
		t.Fatalf("Send = %q, %v", id, err)
	}
	p := f.callsOf("sendMessage")[0].params
	if p["text"] != "<b>Ada &lt;&amp;&gt; &quot;L&quot;</b>: 1 &lt; 2 &amp; &lt;b&gt;x&lt;/b&gt;" || p["parse_mode"] != "HTML" || p["chat_id"] != "-1001" {
		t.Errorf("params = %v", p)
	}
	if p["link_preview_options"].(map[string]any)["is_disabled"] != true {
		t.Errorf("link previews = %v", p["link_preview_options"])
	}
	rp := p["reply_parameters"].(map[string]any)
	if rp["message_id"] != float64(9) || rp["allow_sending_without_reply"] != true {
		t.Errorf("reply = %v", rp)
	}
	// No reply when the id is not Telegram's.
	c.Send(context.Background(), bridge.Outgoing{Chat: "-1001", Sender: "Ada", Text: "hi"})
	if p := f.callsOf("sendMessage")[1].params; p["reply_parameters"] != nil {
		t.Errorf("reply = %v", p["reply_parameters"])
	}
}

func TestSendLong(t *testing.T) {
	f := newFakeAPI(t)
	c := newConn(t, f, &fakeHost{}, "")
	// Emoji take two UTF-16 units each.
	text := strings.Repeat("😀", 3000) + " " + strings.Repeat("ab ", 1000)
	id, err := c.Send(context.Background(), bridge.Outgoing{Chat: "-1001", Sender: "Ada", Text: text, ReplyTo: "3"})
	if err != nil || id != "101" {
		t.Fatalf("Send = %q, %v", id, err)
	}
	sent := f.callsOf("sendMessage")
	if len(sent) < 2 {
		t.Fatalf("sent %d messages", len(sent))
	}
	var joined []string
	for n, s := range sent {
		txt := s.params["text"].(string)
		body, ok := strings.CutPrefix(txt, "<b>Ada</b>: ")
		if !ok {
			t.Errorf("part %d = %.40q", n, txt)
		}
		if l := utf16Len("Ada: " + body); l > maxText {
			t.Errorf("part %d is %d units", n, l)
		}
		if (s.params["reply_parameters"] != nil) != (n == 0) {
			t.Errorf("part %d reply = %v", n, s.params["reply_parameters"])
		}
		joined = append(joined, body)
	}
	if strings.ReplaceAll(strings.Join(joined, ""), " ", "") != strings.ReplaceAll(text, " ", "") {
		t.Error("parts do not add up to the text")
	}
}

func TestSendRateLimited(t *testing.T) {
	f := newFakeAPI(t)
	f.can("sendMessage", reply{429, map[string]any{"ok": false, "error_code": 429, "description": "Too Many Requests: retry after 1", "parameters": map[string]any{"retry_after": 1}}})
	c := newConn(t, f, &fakeHost{}, "")
	start := time.Now()
	id, err := c.Send(context.Background(), bridge.Outgoing{Chat: "-1001", Sender: "Ada", Text: "hi"})
	if err != nil || id == "" {
		t.Fatalf("Send = %q, %v", id, err)
	}
	if time.Since(start) < 900*time.Millisecond || len(f.callsOf("sendMessage")) != 2 {
		t.Errorf("did not wait and retry")
	}
}

func TestSendErrors(t *testing.T) {
	f := newFakeAPI(t)
	f.can("sendMessage", fail(400, "Bad Request: chat not found"), fail(500, "Internal Server Error"))
	c := newConn(t, f, &fakeHost{}, "")
	_, err := c.Send(context.Background(), bridge.Outgoing{Chat: "-1", Text: "hi"})
	if !errors.Is(err, bridge.ErrRejected) || !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("400: %v", err)
	}
	_, err = c.Send(context.Background(), bridge.Outgoing{Chat: "-1", Text: "hi"})
	if err == nil || errors.Is(err, bridge.ErrRejected) {
		t.Errorf("500: %v", err)
	}
}

func TestEdit(t *testing.T) {
	f := newFakeAPI(t)
	f.can("editMessageText", fail(400, "Bad Request: message is not modified: specified new message content and reply markup are exactly the same"))
	c := newConn(t, f, &fakeHost{}, "")
	m := bridge.Outgoing{Chat: "-1001", Sender: "Ada", Text: "fixed & done"}
	if err := c.Edit(context.Background(), m, "55"); err != nil {
		t.Errorf("not modified: %v", err)
	}
	if err := c.Edit(context.Background(), m, "55"); err != nil {
		t.Errorf("edit: %v", err)
	}
	p := f.callsOf("editMessageText")[1].params
	if p["message_id"] != float64(55) || p["text"] != "<b>Ada</b>: fixed &amp; done" || p["parse_mode"] != "HTML" {
		t.Errorf("params = %v", p)
	}
}

func TestTokenNotInErrors(t *testing.T) {
	f := newFakeAPI(t)
	c := newConn(t, f, &fakeHost{}, "")
	f.srv.Close()
	_, err := c.Send(context.Background(), bridge.Outgoing{Chat: "-1", Text: "hi"})
	if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "bot<token>/sendMessage") {
		t.Errorf("error = %v", err)
	}
	if err := c.Run(context.Background()); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("Run error = %v", err)
	}
}

func TestBadToken(t *testing.T) {
	f := newFakeAPI(t)
	f.can("getMe", fail(401, "Unauthorized"))
	h := &fakeHost{}
	c := newConn(t, f, h, "")
	err := c.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "token") || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("Run error = %v", err)
	}
	if h.connected != 0 {
		t.Error("connected with a bad token")
	}
	if _, err := open(bridge.Info{Config: json.RawMessage(`{"bot_token":"nope"}`)}, h); err == nil {
		t.Error("open took a malformed token")
	}
}

func TestConflict(t *testing.T) {
	f := newFakeAPI(t)
	f.can("getUpdates", fail(409, "Conflict: can't use getUpdates method while webhook is active; use deleteWebhook to delete the webhook first"))
	c := newConn(t, f, &fakeHost{}, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.Run(ctx)
	if err == nil || ctx.Err() != nil || !strings.Contains(err.Error(), "webhook") {
		t.Errorf("Run error = %v", err)
	}
}

func TestSplit(t *testing.T) {
	got := split("aaaa bbbb\ncccc", 10)
	if len(got) != 2 || got[0] != "aaaa bbbb" || got[1] != "cccc" {
		t.Errorf("split = %q", got)
	}
	got = split(strings.Repeat("x", 25), 10)
	if len(got) != 3 || got[2] != "xxxxx" {
		t.Errorf("hard split = %q", got)
	}
	if s := single("Ada", strings.Repeat("y ", 3000)); utf16Len(s) > maxText+len("<b></b>") || !strings.HasSuffix(s, "…") {
		t.Errorf("single is %d units", utf16Len(s))
	}
}
