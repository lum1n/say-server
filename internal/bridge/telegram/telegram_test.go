package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/telegram/updates/hook"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/vegard/say-v2/internal/bridge"
)

const selfID = 1000

// fakeHost is the core as the tests play it.
type fakeHost struct {
	mu        sync.Mutex
	got       []bridge.Incoming
	prompts   []bridge.Prompt
	answers   []string
	state     json.RawMessage
	saves     int
	connected bool
	// block, if set, makes Receive wait for it to close.
	block chan struct{}
}

func (h *fakeHost) Receive(ctx context.Context, m bridge.Incoming) error {
	if h.block != nil {
		select {
		case <-h.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, m)
	return nil
}

func (h *fakeHost) Ask(ctx context.Context, p bridge.Prompt) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prompts = append(h.prompts, p)
	if len(h.answers) == 0 {
		return "", errors.New("no answer")
	}
	a := h.answers[0]
	h.answers = h.answers[1:]
	return a, nil
}

func (h *fakeHost) SaveState(ctx context.Context, s json.RawMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state = append(json.RawMessage(nil), s...)
	h.saves++
	return nil
}

func (h *fakeHost) Chats(context.Context) ([]string, error) { return nil, nil }

func (h *fakeHost) Connected(context.Context) {
	h.mu.Lock()
	h.connected = true
	h.mu.Unlock()
}

// fakeAPI answers API calls the way the Telegram server would, by
// encoding the answer as it would arrive on the wire.
type fakeAPI struct {
	mu     sync.Mutex
	calls  []bin.Encoder
	answer func(req bin.Encoder) (bin.Encoder, error)
}

func (f *fakeAPI) Invoke(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
	f.mu.Lock()
	f.calls = append(f.calls, in)
	f.mu.Unlock()
	res, err := f.answer(in)
	if err != nil {
		return err
	}
	var b bin.Buffer
	if err := res.Encode(&b); err != nil {
		return err
	}
	return out.Decode(&b)
}

func (f *fakeAPI) called() []bin.Encoder {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bin.Encoder(nil), f.calls...)
}

// newConn makes a connection logged in as selfID over api.
func newConn(t *testing.T, h *fakeHost, api tg.Invoker) *conn {
	t.Helper()
	cfg := `{"api_id": 12345, "api_hash": "abc", "phone": "+47 123 45 678"}`
	b, err := open(bridge.Info{ID: "b1", Config: json.RawMessage(cfg), State: h.state}, h)
	if err != nil {
		t.Fatal(err)
	}
	c := b.(*conn)
	c.pause = 0
	c.st.loggedIn(&tg.User{ID: selfID, AccessHash: 1, FirstName: "Me"})
	if api != nil {
		c.setAPI(tg.NewClient(api))
	}
	return c
}

func alice() *tg.User {
	return &tg.User{ID: 7, AccessHash: 77, FirstName: "Alice", LastName: "Liddell"}
}

func TestOpenChecksConfig(t *testing.T) {
	h := &fakeHost{}
	for _, cfg := range []string{
		`{"api_hash": "abc", "phone": "+4712345678"}`,
		`{"api_id": 1, "phone": "+4712345678"}`,
		`{"api_id": 1, "api_hash": "abc", "phone": "12345678"}`,
	} {
		if _, err := open(bridge.Info{Config: json.RawMessage(cfg)}, h); err == nil {
			t.Errorf("%s: no error", cfg)
		}
	}
	c, err := open(bridge.Info{Config: json.RawMessage(`{"api_id": 1, "api_hash": "abc", "phone": "+47 (123) 45-678"}`)}, h)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.(*conn).cfg.Phone; got != "+4712345678" {
		t.Errorf("phone = %q", got)
	}
}

// fakeAuth plays Telegram's login.
type fakeAuth struct {
	authorized bool
	code       string
	password   string // "" is no two-step verification
	signedIn   bool
}

func (a *fakeAuth) Status(context.Context) (*auth.Status, error) {
	return &auth.Status{Authorized: a.authorized}, nil
}

func (a *fakeAuth) SendCode(_ context.Context, phone string, _ auth.SendCodeOptions) (tg.AuthSentCodeClass, error) {
	return &tg.AuthSentCode{Type: &tg.AuthSentCodeTypeApp{}, PhoneCodeHash: "hash"}, nil
}

func (a *fakeAuth) SignIn(_ context.Context, phone, code, hash string) (*tg.AuthAuthorization, error) {
	if hash != "hash" || code != a.code {
		return nil, tgerr.New(400, "PHONE_CODE_INVALID")
	}
	if a.password != "" {
		return nil, auth.ErrPasswordAuthNeeded
	}
	a.signedIn = true
	return &tg.AuthAuthorization{}, nil
}

func (a *fakeAuth) Password(_ context.Context, pw string) (*tg.AuthAuthorization, error) {
	if pw != a.password {
		return nil, auth.ErrPasswordInvalid
	}
	a.signedIn = true
	return &tg.AuthAuthorization{}, nil
}

func TestLoginAsksCodeAndPassword(t *testing.T) {
	h := &fakeHost{answers: []string{"11111", "12 345", "wrong", "secret"}}
	a := &fakeAuth{code: "12345", password: "secret"}
	if err := login(context.Background(), a, "+4712345678", h); err != nil {
		t.Fatal(err)
	}
	if !a.signedIn {
		t.Fatal("not signed in")
	}
	var kinds []string
	for _, p := range h.prompts {
		kinds = append(kinds, p.Kind)
	}
	if fmt.Sprint(kinds) != "[code code password password]" {
		t.Errorf("prompts = %v", kinds)
	}
	if want := "Telegram sent a code to +4712345678 in your Telegram app. Type it here."; h.prompts[0].Text != want {
		t.Errorf("first prompt = %q", h.prompts[0].Text)
	}
}

func TestLoginSkippedWhenAuthorized(t *testing.T) {
	h := &fakeHost{}
	if err := login(context.Background(), &fakeAuth{authorized: true}, "+4712345678", h); err != nil {
		t.Fatal(err)
	}
	if len(h.prompts) != 0 {
		t.Errorf("asked %v", h.prompts)
	}
}

func TestLoginGivesUpAfterWrongCodes(t *testing.T) {
	h := &fakeHost{answers: []string{"1", "2", "3", "4"}}
	err := login(context.Background(), &fakeAuth{code: "12345"}, "+4712345678", h)
	if err == nil || len(h.prompts) != tries {
		t.Fatalf("err = %v after %d prompts", err, len(h.prompts))
	}
}

// handle passes updates to a receiver and returns what it queued.
func handle(t *testing.T, c *conn, u tg.UpdatesClass) []bridge.Incoming {
	t.Helper()
	q := newQueue()
	if err := (&receiver{st: c.st, q: q}).Handle(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return q.items
}

func TestReceiveMessages(t *testing.T) {
	c := newConn(t, &fakeHost{}, nil)
	date := int(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).Unix())
	u := &tg.Updates{
		Users: []tg.UserClass{alice(), &tg.User{ID: 8, AccessHash: 88, Username: "bob"}},
		Chats: []tg.ChatClass{
			&tg.Chat{ID: 50, Title: "Family"},
			&tg.Channel{ID: 60, AccessHash: 66, Title: "News", Broadcast: true},
		},
		Updates: []tg.UpdateClass{
			// A private message from Alice.
			&tg.UpdateNewMessage{Message: &tg.Message{ID: 1, PeerID: &tg.PeerUser{UserID: 7}, Date: date, Message: "hi",
				ReplyTo: &tg.MessageReplyHeader{ReplyToMsgID: 0}}},
			// The owner writing to Alice from their phone.
			&tg.UpdateNewMessage{Message: &tg.Message{ID: 2, Out: true, PeerID: &tg.PeerUser{UserID: 7}, Date: date, Message: "hello",
				ReplyTo: &tg.MessageReplyHeader{ReplyToMsgID: 1}}},
			// Bob in a group, with a sticker.
			&tg.UpdateNewMessage{Message: &tg.Message{ID: 3, FromID: &tg.PeerUser{UserID: 8}, PeerID: &tg.PeerChat{ChatID: 50}, Date: date,
				Media: &tg.MessageMediaDocument{Document: &tg.Document{Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeSticker{Alt: "😀"}}}}}},
			// Someone joined: skipped.
			&tg.UpdateNewMessage{Message: &tg.MessageService{ID: 4, PeerID: &tg.PeerChat{ChatID: 50}, Action: &tg.MessageActionChatAddUser{}}},
			// A channel post with a photo and a hidden link.
			&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 9, PeerID: &tg.PeerChannel{ChannelID: 60}, Date: date, Post: true,
				Media: &tg.MessageMediaPhoto{}, Message: "read this 🎉 now",
				Entities: []tg.MessageEntityClass{&tg.MessageEntityTextURL{Offset: 5, Length: 4, URL: "https://example.com"}}}},
			// Alice edits her message.
			&tg.UpdateEditMessage{Message: &tg.Message{ID: 1, PeerID: &tg.PeerUser{UserID: 7}, Date: date, Message: "hi there"}},
			// Saved Messages.
			&tg.UpdateNewMessage{Message: &tg.Message{ID: 5, Out: true, PeerID: &tg.PeerUser{UserID: selfID}, Date: date, Message: "note"}},
		},
	}
	got := handle(t, c, u)
	at := time.Unix(int64(date), 0)
	aliceChat := bridge.RemoteChat{ID: "user:7", Name: "Alice Liddell", Direct: true}
	want := []bridge.Incoming{
		{Chat: aliceChat, ID: "1", Author: bridge.Author{ID: "user:7", Name: "Alice Liddell"}, Text: "hi", Time: at},
		{Chat: aliceChat, ID: "2", Author: bridge.Author{Self: true}, Text: "hello", Time: at, ReplyTo: "1"},
		{Chat: bridge.RemoteChat{ID: "chat:50", Name: "Family"}, ID: "3", Author: bridge.Author{ID: "user:8", Name: "@bob"}, Text: "[sticker 😀]", Time: at},
		{Chat: bridge.RemoteChat{ID: "channel:60", Name: "News"}, ID: "9", Author: bridge.Author{ID: "channel:60", Name: "News"},
			Text: "[photo]\nread this (https://example.com) 🎉 now", Time: at},
		{Chat: aliceChat, ID: "1", Author: bridge.Author{ID: "user:7", Name: "Alice Liddell"}, Text: "hi there", Time: at, Edit: true},
		{Chat: bridge.RemoteChat{ID: "user:1000", Name: "Saved Messages", Direct: true}, ID: "5", Author: bridge.Author{Self: true}, Text: "note", Time: at},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if fmt.Sprintf("%+v", got[i]) != fmt.Sprintf("%+v", want[i]) {
			t.Errorf("message %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

func TestMediaText(t *testing.T) {
	doc := func(attrs ...tg.DocumentAttributeClass) tg.MessageMediaClass {
		return &tg.MessageMediaDocument{Document: &tg.Document{Attributes: attrs}}
	}
	for _, c := range []struct {
		media tg.MessageMediaClass
		want  string
	}{
		{doc(&tg.DocumentAttributeAudio{Voice: true}), "[voice message]"},
		{doc(&tg.DocumentAttributeAudio{Performer: "Queen", Title: "Bohemian Rhapsody"}), "[audio: Queen - Bohemian Rhapsody]"},
		{doc(&tg.DocumentAttributeVideo{RoundMessage: true}), "[video message]"},
		{doc(&tg.DocumentAttributeVideo{}, &tg.DocumentAttributeAnimated{}), "[GIF]"},
		{doc(&tg.DocumentAttributeVideo{}, &tg.DocumentAttributeFilename{FileName: "a.mp4"}), "[video]"},
		{doc(&tg.DocumentAttributeFilename{FileName: "report.pdf"}), "[file: report.pdf]"},
		{doc(&tg.DocumentAttributeSticker{}), "[sticker]"},
		{&tg.MessageMediaGeo{Geo: &tg.GeoPoint{Lat: 59.9, Long: 10.75}}, "[location https://www.openstreetmap.org/?mlat=59.900000&mlon=10.750000]"},
		{&tg.MessageMediaContact{FirstName: "Ada", PhoneNumber: "+441"}, "[contact: Ada +441]"},
		{&tg.MessageMediaPoll{Poll: tg.Poll{Question: tg.TextWithEntities{Text: "Lunch?"}}}, "[poll: Lunch?]"},
		{&tg.MessageMediaDice{Emoticon: "🎲", Value: 4}, "[🎲 4]"},
		{&tg.MessageMediaWebPage{}, ""},
	} {
		if got := mediaText(c.media); got != c.want {
			t.Errorf("%T: got %q, want %q", c.media, got, c.want)
		}
	}
}

func TestSendReturnsEchoID(t *testing.T) {
	h := &fakeHost{}
	var echo *tg.Updates
	api := &fakeAPI{answer: func(req bin.Encoder) (bin.Encoder, error) {
		r, ok := req.(*tg.MessagesSendMessageRequest)
		if !ok {
			return nil, fmt.Errorf("unexpected %T", req)
		}
		echo = &tg.Updates{Users: []tg.UserClass{alice()}, Updates: []tg.UpdateClass{
			&tg.UpdateMessageID{ID: 42, RandomID: r.RandomID},
			&tg.UpdateNewMessage{Message: &tg.Message{ID: 42, Out: true, PeerID: &tg.PeerUser{UserID: 7}, Message: r.Message}},
		}}
		return echo, nil
	}}
	c := newConn(t, h, api)
	c.st.remember([]tg.UserClass{alice()}, nil)
	id, err := c.Send(context.Background(), bridge.Outgoing{Chat: "user:7", Text: "hello", ReplyTo: "3"})
	if err != nil {
		t.Fatal(err)
	}
	if id != "42" {
		t.Errorf("id = %q", id)
	}
	req := api.called()[0].(*tg.MessagesSendMessageRequest)
	peer, ok := req.Peer.(*tg.InputPeerUser)
	if !ok || peer.UserID != 7 || peer.AccessHash != 77 || req.Message != "hello" || req.RandomID == 0 {
		t.Errorf("request = %+v", req)
	}
	if r, ok := req.ReplyTo.(*tg.InputReplyToMessage); !ok || r.ReplyToMsgID != 3 {
		t.Errorf("reply to = %+v", req.ReplyTo)
	}
	// The echo carries the same id, as the owner's own message.
	got := handle(t, c, echo)
	if len(got) != 1 || got[0].ID != id || !got[0].Author.Self || got[0].Chat.ID != "user:7" {
		t.Errorf("echo = %+v", got)
	}
}

func TestSentIDShort(t *testing.T) {
	if id, ok := sentID(&tg.UpdateShortSentMessage{ID: 9, Out: true}, 1); !ok || id != 9 {
		t.Errorf("short: %d %v", id, ok)
	}
	u := &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 11, Out: true}}}}
	if id, ok := sentID(u, 1); !ok || id != 11 {
		t.Errorf("channel: %d %v", id, ok)
	}
	if _, ok := sentID(&tg.Updates{}, 1); ok {
		t.Error("empty updates gave an id")
	}
}

func TestSendErrors(t *testing.T) {
	api := &fakeAPI{answer: func(req bin.Encoder) (bin.Encoder, error) {
		switch req.(type) {
		case *tg.MessagesGetDialogsRequest:
			return &tg.MessagesDialogs{}, nil
		case *tg.MessagesSendMessageRequest:
			return nil, tgerr.New(403, "CHAT_WRITE_FORBIDDEN")
		}
		return nil, fmt.Errorf("unexpected %T", req)
	}}
	c := newConn(t, &fakeHost{}, api)
	_, err := c.Send(context.Background(), bridge.Outgoing{Chat: "chat:50", Text: "hi"})
	if !errors.Is(err, bridge.ErrRejected) {
		t.Errorf("forbidden: %v", err)
	}
	// An unknown user is looked for in the dialogs, then refused.
	_, err = c.Send(context.Background(), bridge.Outgoing{Chat: "user:99", Text: "hi"})
	if !errors.Is(err, bridge.ErrRejected) {
		t.Errorf("unknown user: %v", err)
	}
	if _, ok := api.called()[1].(*tg.MessagesGetDialogsRequest); !ok {
		t.Errorf("dialogs not fetched: %T", api.called()[1])
	}
	// A resent request that was posted the first time is not a refusal.
	api.answer = func(bin.Encoder) (bin.Encoder, error) { return nil, tgerr.New(400, "RANDOM_ID_DUPLICATE") }
	_, err = c.Send(context.Background(), bridge.Outgoing{Chat: "chat:50", Text: "hi"})
	if err == nil || errors.Is(err, bridge.ErrRejected) {
		t.Errorf("duplicate random id: %v", err)
	}
	// A server failure leaves the outcome unknown.
	api.answer = func(bin.Encoder) (bin.Encoder, error) { return nil, tgerr.New(500, "INTERNAL") }
	_, err = c.Send(context.Background(), bridge.Outgoing{Chat: "chat:50", Text: "hi"})
	if err == nil || errors.Is(err, bridge.ErrRejected) {
		t.Errorf("internal: %v", err)
	}
}

func TestEdit(t *testing.T) {
	api := &fakeAPI{answer: func(req bin.Encoder) (bin.Encoder, error) {
		r := req.(*tg.MessagesEditMessageRequest)
		if r.Message == "same" {
			return nil, tgerr.New(400, "MESSAGE_NOT_MODIFIED")
		}
		return &tg.Updates{}, nil
	}}
	c := newConn(t, &fakeHost{}, api)
	c.st.remember(nil, []tg.ChatClass{&tg.Channel{ID: 60, AccessHash: 66, Title: "Group", Megagroup: true}})
	if err := c.Edit(context.Background(), bridge.Outgoing{Chat: "channel:60", Text: "fixed"}, "12"); err != nil {
		t.Fatal(err)
	}
	r := api.called()[0].(*tg.MessagesEditMessageRequest)
	p, ok := r.Peer.(*tg.InputPeerChannel)
	if !ok || p.ChannelID != 60 || p.AccessHash != 66 || r.ID != 12 || r.Message != "fixed" {
		t.Errorf("request = %+v", r)
	}
	if err := c.Edit(context.Background(), bridge.Outgoing{Chat: "channel:60", Text: "same"}, "12"); err != nil {
		t.Errorf("not modified: %v", err)
	}
}

func TestBackfill(t *testing.T) {
	now := time.Now()
	recent := int(now.Add(-time.Hour).Unix())
	old := int(now.Add(-60 * 24 * time.Hour).Unix())
	api := &fakeAPI{answer: func(req bin.Encoder) (bin.Encoder, error) {
		switch r := req.(type) {
		case *tg.MessagesGetDialogsRequest:
			if r.Limit != 50 {
				return nil, fmt.Errorf("limit %d", r.Limit)
			}
			return &tg.MessagesDialogsSlice{
				Dialogs: []tg.DialogClass{
					&tg.Dialog{Peer: &tg.PeerUser{UserID: 7}, TopMessage: 3},
					&tg.Dialog{Peer: &tg.PeerChat{ChatID: 50}, TopMessage: 20},
				},
				Messages: []tg.MessageClass{
					&tg.Message{ID: 3, PeerID: &tg.PeerUser{UserID: 7}, Date: recent},
					&tg.Message{ID: 20, PeerID: &tg.PeerChat{ChatID: 50}, Date: old},
				},
				Users: []tg.UserClass{alice()},
				Chats: []tg.ChatClass{&tg.Chat{ID: 50, Title: "Old", Photo: &tg.ChatPhotoEmpty{}}},
			}, nil
		case *tg.MessagesGetHistoryRequest:
			p, ok := r.Peer.(*tg.InputPeerUser)
			if !ok || p.UserID != 7 || p.AccessHash != 77 || r.Limit != 20 {
				return nil, fmt.Errorf("history of %+v", r)
			}
			// Newest first, as the server sends it.
			return &tg.MessagesMessages{Messages: []tg.MessageClass{
				&tg.Message{ID: 3, PeerID: &tg.PeerUser{UserID: 7}, Date: recent, Message: "three", Out: true},
				&tg.MessageService{ID: 2, PeerID: &tg.PeerUser{UserID: 7}, Date: recent, Action: &tg.MessageActionPinMessage{}},
				&tg.Message{ID: 1, PeerID: &tg.PeerUser{UserID: 7}, Date: recent - 60, Message: "one"},
			}, Users: []tg.UserClass{alice()}}, nil
		}
		return nil, fmt.Errorf("unexpected %T", req)
	}}
	c := newConn(t, &fakeHost{}, api)
	q := newQueue()
	if err := c.fetchBackfill(context.Background(), c.client(), q); err != nil {
		t.Fatal(err)
	}
	if len(q.items) != 2 || q.items[0].ID != "1" || q.items[1].ID != "3" || !q.items[1].Author.Self ||
		q.items[0].Author.Name != "Alice Liddell" || !q.items[0].Time.Equal(time.Unix(int64(recent-60), 0)) {
		t.Errorf("backfill = %+v", q.items)
	}
	if n := len(api.called()); n != 2 {
		t.Errorf("%d calls; the old chat should not be fetched", n)
	}
}

// TestReceiveDoesNotBlockSend checks that a send completes while the host
// is still busy with an earlier message, as the core makes it wait for
// sends in progress on the same chat.
func TestReceiveDoesNotBlockSend(t *testing.T) {
	h := &fakeHost{block: make(chan struct{})}
	q := newQueue()
	c := newConn(t, h, nil)
	r := &receiver{st: c.st, q: q}
	api := &fakeAPI{answer: func(req bin.Encoder) (bin.Encoder, error) {
		rq := req.(*tg.MessagesSendMessageRequest)
		return &tg.Updates{Updates: []tg.UpdateClass{
			&tg.UpdateMessageID{ID: 5, RandomID: rq.RandomID},
			&tg.UpdateNewMessage{Message: &tg.Message{ID: 5, Out: true, PeerID: &tg.PeerChat{ChatID: 50}, Message: rq.Message}},
		}}, nil
	}}
	// As in Run: the result of each call goes to the update handler.
	c.setAPI(tg.NewClient(hook.UpdateHook(r.Handle).Handle(api)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.receiveLoop(ctx, q)
	q.push(bridge.Incoming{Chat: bridge.RemoteChat{ID: "chat:50"}, ID: "4", Text: "earlier"})

	done := make(chan error, 1)
	go func() {
		_, err := c.Send(ctx, bridge.Outgoing{Chat: "chat:50", Text: "mine"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("send blocked on receive")
	}
	close(h.block)
	waitFor(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.got) == 2 && h.got[1].ID == "5" && h.got[1].Author.Self
	})
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for range 500 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestStateSavedAndRestored(t *testing.T) {
	ctx := context.Background()
	h := &fakeHost{}
	c := newConn(t, h, nil)
	st := c.st
	st.StoreSession(ctx, []byte("session-bytes"))
	st.SetState(ctx, selfID, updates.State{Pts: 10, Qts: 1, Date: 100, Seq: 3})
	st.SetPts(ctx, selfID, 11)
	st.SetDateSeq(ctx, selfID, 101, 4)
	st.SetChannelPts(ctx, selfID, 60, 500)
	st.SetChannelAccessHash(ctx, selfID, 60, 66)
	st.remember([]tg.UserClass{alice()}, nil)
	if err := st.flush(ctx); err != nil {
		t.Fatal(err)
	}
	saves := h.saves
	if err := st.flush(ctx); err != nil || h.saves != saves {
		t.Errorf("an unchanged state was saved again")
	}

	// The next run, maybe on another node, starts from the saved state.
	c2 := newConn(t, h, nil)
	st2 := c2.st
	sess, err := st2.LoadSession(ctx)
	if err != nil || string(sess) != "session-bytes" {
		t.Errorf("session = %q, %v", sess, err)
	}
	s, found, _ := st2.GetState(ctx, selfID)
	if !found || s != (updates.State{Pts: 11, Qts: 1, Date: 101, Seq: 4}) {
		t.Errorf("updates state = %+v, %v", s, found)
	}
	if pts, ok, _ := st2.GetChannelPts(ctx, selfID, 60); !ok || pts != 500 {
		t.Errorf("channel pts = %d", pts)
	}
	if h, ok, _ := st2.GetChannelAccessHash(ctx, selfID, 60); !ok || h != 66 {
		t.Errorf("channel hash = %d", h)
	}
	if h, ok, _ := st2.GetUserAccessHash(ctx, selfID, 7); !ok || h != 77 {
		t.Errorf("user hash = %d", h)
	}

	// Another account on the same session forgets the old positions.
	st2.loggedIn(&tg.User{ID: 2000})
	if _, found, _ := st2.GetState(ctx, 2000); found {
		t.Error("state kept for another account")
	}
	// Another phone number logs in afresh.
	st3 := newSaved(h.state, "+4799999999", h.SaveState)
	if _, err := st3.LoadSession(ctx); err == nil {
		t.Error("session kept for another phone")
	}
}

func TestStateBatchesUpdatePositions(t *testing.T) {
	st := newSaved(nil, "+4712345678", nil)
	st.SetState(context.Background(), 0, updates.State{Pts: 1})
	st.SetPts(context.Background(), 0, 2)
	select {
	case <-st.kick:
		t.Error("an update position asked for a save at once")
	default:
	}
	st.StoreSession(context.Background(), []byte("s"))
	select {
	case <-st.kick:
	default:
		t.Error("a new session did not ask for a save")
	}
}

func TestPeerCacheIsBounded(t *testing.T) {
	st := newSaved(nil, "+4712345678", nil)
	clock := int64(0)
	st.now = func() time.Time { clock++; return time.Unix(clock, 0) }
	for i := range maxPeers + 10 {
		st.setHash(chatID("user", int64(i)), int64(i+1))
	}
	if n := len(st.s.Peers); n != maxPeers {
		t.Errorf("%d peers", n)
	}
	if _, ok := st.hash("user:0"); ok {
		t.Error("the oldest peer was kept")
	}
	if _, ok := st.hash(chatID("user", maxPeers+9)); !ok {
		t.Error("the newest peer was dropped")
	}
}

// TestResumeFetchesMissed runs gotd's update manager on a saved state, as
// a reconnect does: it asks for the difference since the saved position
// and passes on what was missed.
func TestResumeFetchesMissed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &fakeHost{}
	c := newConn(t, h, nil)
	c.st.SetState(ctx, selfID, updates.State{Pts: 100, Qts: 0, Date: 1000, Seq: 5})
	q := newQueue()
	api := &diffAPI{}
	mgr := updates.New(updates.Config{Handler: &receiver{st: c.st, q: q}, Storage: c.st, AccessHasher: c.st, UserAccessHasher: c.st})
	done := make(chan error, 1)
	go func() { done <- mgr.Run(ctx, api, selfID, updates.AuthOptions{}) }()
	in, err := q.pop(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if in.ID != "77" || in.Text != "while you were away" || in.Author.Name != "Alice Liddell" {
		t.Errorf("missed message = %+v", in)
	}
	if got := api.from(); got != 100 {
		t.Errorf("difference asked from pts %d", got)
	}
	waitFor(t, func() bool {
		s, _, _ := c.st.GetState(ctx, selfID)
		return s.Pts == 101
	})
	cancel()
	<-done
}

// diffAPI is a server with one message missed since pts 100.
type diffAPI struct {
	mu  sync.Mutex
	pts int
}

func (a *diffAPI) from() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pts
}

func (a *diffAPI) UpdatesGetState(context.Context) (*tg.UpdatesState, error) {
	return &tg.UpdatesState{Pts: 101, Date: 1001, Seq: 5}, nil
}

func (a *diffAPI) UpdatesGetDifference(_ context.Context, r *tg.UpdatesGetDifferenceRequest) (tg.UpdatesDifferenceClass, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pts != 0 {
		return &tg.UpdatesDifferenceEmpty{Date: 1001, Seq: 5}, nil
	}
	a.pts = r.Pts
	return &tg.UpdatesDifference{
		NewMessages: []tg.MessageClass{&tg.Message{ID: 77, PeerID: &tg.PeerUser{UserID: 7}, Date: 1001, Message: "while you were away"}},
		Users:       []tg.UserClass{alice()},
		State:       tg.UpdatesState{Pts: 101, Date: 1001, Seq: 5},
	}, nil
}

func (a *diffAPI) UpdatesGetChannelDifference(context.Context, *tg.UpdatesGetChannelDifferenceRequest) (tg.UpdatesChannelDifferenceClass, error) {
	return &tg.UpdatesChannelDifferenceEmpty{Final: true, Pts: 1}, nil
}

func TestFloodWaitRetries(t *testing.T) {
	calls := 0
	next := &fakeAPI{answer: func(bin.Encoder) (bin.Encoder, error) {
		calls++
		if calls == 1 {
			return nil, tgerr.New(420, "FLOOD_WAIT_0")
		}
		return &tg.Updates{}, nil
	}}
	inv := floodWait{max: time.Minute}.Handle(next)
	if err := inv(context.Background(), &tg.UpdatesGetStateRequest{}, &tg.UpdatesBox{}); err != nil || calls != 2 {
		t.Errorf("err = %v after %d calls", err, calls)
	}
	// A long wait is not waited out.
	calls = 0
	next.answer = func(bin.Encoder) (bin.Encoder, error) {
		calls++
		return nil, tgerr.New(420, "FLOOD_WAIT_"+strconv.Itoa(3600))
	}
	err := inv(context.Background(), &tg.UpdatesGetStateRequest{}, &tg.UpdatesBox{})
	if _, ok := tgerr.AsFloodWait(err); !ok || calls != 1 {
		t.Errorf("err = %v after %d calls", err, calls)
	}
	if !errors.Is(callErr(err), bridge.ErrRejected) {
		t.Error("a flood wait is not a refusal")
	}
}
