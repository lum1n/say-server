package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/telegram/updates/hook"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/lum1n/say-server/internal/bridge"
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

func TestDefaultApp(t *testing.T) {
	h := &fakeHost{}
	phone := `{"phone": "+4712345678"}`
	if _, err := open(bridge.Info{Config: json.RawMessage(phone)}, h); err == nil {
		t.Error("no app and no default: no error")
	}
	if fields(false)[0].Advanced || !fields(true)[0].Advanced || fields(true)[1].Required {
		t.Error("the app's fields are not advanced and optional with a default")
	}

	appID, appHash = "5", "built-in"
	t.Cleanup(func() { appID, appHash = "", "" })
	c, err := open(bridge.Info{Config: json.RawMessage(phone)}, h)
	if err != nil {
		t.Fatal(err)
	}
	if cfg := c.(*conn).cfg; cfg.APIID != 5 || cfg.APIHash != "built-in" {
		t.Errorf("built in: app = %d %q", cfg.APIID, cfg.APIHash)
	}

	t.Setenv("SAY_TELEGRAM_API_ID", "6")
	t.Setenv("SAY_TELEGRAM_API_HASH", "operator")
	c, _ = open(bridge.Info{Config: json.RawMessage(phone)}, h)
	if cfg := c.(*conn).cfg; cfg.APIID != 6 || cfg.APIHash != "operator" {
		t.Errorf("from the environment: app = %d %q", cfg.APIID, cfg.APIHash)
	}

	c, _ = open(bridge.Info{Config: json.RawMessage(`{"api_id": 7, "api_hash": "mine", "phone": "+4712345678"}`)}, h)
	if cfg := c.(*conn).cfg; cfg.APIID != 7 || cfg.APIHash != "mine" {
		t.Errorf("own app: app = %d %q", cfg.APIID, cfg.APIHash)
	}
	// Half an app of one's own is not mixed with the default.
	if _, err := open(bridge.Info{Config: json.RawMessage(`{"api_id": 7, "phone": "+4712345678"}`)}, h); err == nil {
		t.Error("an id without a hash: no error")
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
	if err := (&receiver{st: c.st, q: q, fetch: c.download}).Handle(context.Background(), u); err != nil {
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

func TestReceiveFiles(t *testing.T) {
	jpeg := []byte("\xff\xd8\xff the photo")
	api := &fakeAPI{answer: func(req bin.Encoder) (bin.Encoder, error) {
		r, ok := req.(*tg.UploadGetFileRequest)
		if !ok {
			return nil, fmt.Errorf("unexpected %T", req)
		}
		loc, ok := r.Location.(*tg.InputPhotoFileLocation)
		if !ok || loc.ID != 31 || loc.ThumbSize != "y" || r.Offset != 0 {
			return nil, fmt.Errorf("location %+v at %d", r.Location, r.Offset)
		}
		return &tg.UploadFile{Type: &tg.StorageFileJpeg{}, Bytes: jpeg}, nil
	}}
	c := newConn(t, &fakeHost{}, api)
	c.st.remember([]tg.UserClass{alice()}, nil)
	photo := &tg.Photo{ID: 31, AccessHash: 3, FileReference: []byte{1}, Sizes: []tg.PhotoSizeClass{
		&tg.PhotoStrippedSize{Type: "i"},
		&tg.PhotoSize{Type: "m", W: 320, H: 240, Size: 9000},
		&tg.PhotoSizeProgressive{Type: "y", W: 1280, H: 960, Sizes: []int{100, 5000, int(len(jpeg))}},
	}}
	doc := func(id int64, size int64, attrs ...tg.DocumentAttributeClass) *tg.MessageMediaDocument {
		return &tg.MessageMediaDocument{Document: &tg.Document{ID: id, MimeType: "application/pdf", Size: size, Attributes: attrs}}
	}
	at := &tg.PeerUser{UserID: 7}
	got := handle(t, c, &tg.Updates{Updates: []tg.UpdateClass{
		&tg.UpdateNewMessage{Message: &tg.Message{ID: 1, PeerID: at, Media: &tg.MessageMediaPhoto{Photo: photo}}},
		&tg.UpdateNewMessage{Message: &tg.Message{ID: 2, PeerID: at, Message: "the plan",
			Media: doc(40, 2000, &tg.DocumentAttributeFilename{FileName: "plan.pdf"})}},
		&tg.UpdateNewMessage{Message: &tg.Message{ID: 3, PeerID: at, Media: doc(41, 10, &tg.DocumentAttributeSticker{Alt: "😀"})}},
		&tg.UpdateNewMessage{Message: &tg.Message{ID: 4, PeerID: at, Message: "huge",
			Media: doc(42, 1<<30, &tg.DocumentAttributeFilename{FileName: "film.mkv"})}},
		&tg.UpdateEditMessage{Message: &tg.Message{ID: 1, PeerID: at, Message: "captioned", Media: &tg.MessageMediaPhoto{Photo: photo}}},
	}})
	if len(got) != 5 {
		t.Fatalf("got %d messages: %+v", len(got), got)
	}
	if got[0].Text != "" || len(got[0].Media) != 1 {
		t.Fatalf("photo = %+v", got[0])
	}
	m := got[0].Media[0]
	if m.Mime != "image/jpeg" || m.Name != "photo.jpg" || m.Width != 1280 || m.Height != 960 || m.Size != int64(len(jpeg)) {
		t.Errorf("photo's media = %+v", m)
	}
	r, err := m.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(data) != string(jpeg) {
		t.Errorf("photo reads %q, %v", data, err)
	}
	if got[1].Text != "the plan" || len(got[1].Media) != 1 || got[1].Media[0].Name != "plan.pdf" || got[1].Media[0].Mime != "application/pdf" {
		t.Errorf("document = %+v", got[1])
	}
	if got[2].Text != "[sticker 😀]" || len(got[2].Media) != 0 {
		t.Errorf("sticker = %+v", got[2])
	}
	if got[3].Text != "[file: film.mkv]\nhuge" || len(got[3].Media) != 0 {
		t.Errorf("too big = %+v", got[3])
	}
	if !got[4].Edit || got[4].Text != "captioned" || len(got[4].Media) != 0 {
		t.Errorf("caption edit = %+v", got[4])
	}
}

func TestSendFiles(t *testing.T) {
	h := &fakeHost{}
	q := newQueue()
	c := newConn(t, h, nil)
	r := &receiver{st: c.st, q: q}
	var mu sync.Mutex
	parts := map[int64][]byte{}
	next := 40
	api := &fakeAPI{answer: func(req bin.Encoder) (bin.Encoder, error) {
		mu.Lock()
		defer mu.Unlock()
		var msg string
		var random int64
		switch rq := req.(type) {
		case *tg.UploadSaveFilePartRequest:
			parts[rq.FileID] = append(parts[rq.FileID], rq.Bytes...)
			return &tg.BoolTrue{}, nil
		case *tg.MessagesSendMediaRequest:
			msg, random = rq.Message, rq.RandomID
		case *tg.MessagesSendMessageRequest:
			msg, random = rq.Message, rq.RandomID
		default:
			return nil, fmt.Errorf("unexpected %T", req)
		}
		next++
		return &tg.Updates{Updates: []tg.UpdateClass{
			&tg.UpdateMessageID{ID: next, RandomID: random},
			&tg.UpdateNewMessage{Message: &tg.Message{ID: next, Out: true, PeerID: &tg.PeerUser{UserID: 7}, Message: msg}},
		}}, nil
	}}
	// As in Run: the result of each call goes to the update handler.
	c.setAPI(tg.NewClient(hook.UpdateHook(r.Handle).Handle(api)))
	c.st.remember([]tg.UserClass{alice()}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.receiveLoop(ctx, q)

	file := func(mime, name, data string) bridge.Media {
		return bridge.Media{Mime: mime, Name: name, Open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(data)), nil
		}}
	}
	id, err := c.Send(ctx, bridge.Outgoing{Chat: "user:7", Text: "look", ReplyTo: "3", Media: []bridge.Media{
		file("image/png", "cat.png", "png bytes"),
		file("application/pdf", "plan.pdf", "pdf bytes"),
	}})
	if err != nil || id != "41" {
		t.Fatalf("Send = %q, %v", id, err)
	}
	var sends []*tg.MessagesSendMediaRequest
	for _, c := range api.called() {
		if r, ok := c.(*tg.MessagesSendMediaRequest); ok {
			sends = append(sends, r)
		}
	}
	if len(sends) != 2 {
		t.Fatalf("%d sends", len(sends))
	}
	photo, ok := sends[0].Media.(*tg.InputMediaUploadedPhoto)
	if !ok || sends[0].Message != "look" {
		t.Fatalf("first send = %+v", sends[0])
	}
	if r, ok := sends[0].ReplyTo.(*tg.InputReplyToMessage); !ok || r.ReplyToMsgID != 3 {
		t.Errorf("reply to = %+v", sends[0].ReplyTo)
	}
	f := photo.File.(*tg.InputFile)
	if f.Name != "cat.png" || string(parts[f.ID]) != "png bytes" {
		t.Errorf("photo = %+v: %q", f, parts[f.ID])
	}
	d, ok := sends[1].Media.(*tg.InputMediaUploadedDocument)
	if !ok || d.MimeType != "application/pdf" || sends[1].Message != "" || sends[1].ReplyTo != nil {
		t.Fatalf("second send = %+v", sends[1])
	}
	if n := d.Attributes[0].(*tg.DocumentAttributeFilename).FileName; n != "plan.pdf" || string(parts[d.File.(*tg.InputFile).ID]) != "pdf bytes" {
		t.Errorf("document %q = %q", n, parts[d.File.(*tg.InputFile).ID])
	}
	// The echo of the first goes to the core, which knows its id; the
	// second's is dropped.
	waitFor(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.got) == 1 && h.got[0].ID == "41"
	})
	q.push(bridge.Incoming{Chat: bridge.RemoteChat{ID: "user:7"}, ID: "99", Author: bridge.Author{Self: true}, Text: "later"})
	waitFor(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.got) == 2 && h.got[1].ID == "99"
	})
	// It stays dropped after a restart, as when backfill fetches it again.
	if !newSaved(mustJSON(t, c.st), "+4712345678", nil).wasSentToo("user:7", "42") {
		t.Error("the second message is not in the state")
	}

	// A text too long for a caption goes first, and its id is the one.
	long := strings.Repeat("word ", 300)
	if id, err := c.Send(ctx, bridge.Outgoing{Chat: "user:7", Text: long, Media: []bridge.Media{file("image/jpeg", "a.jpg", "jpg")}}); err != nil || id != "43" {
		t.Fatalf("Send = %q, %v", id, err)
	}
	last := api.called()
	if r, ok := last[len(last)-1].(*tg.MessagesSendMediaRequest); !ok || r.Message != "" {
		t.Errorf("file after the text = %+v", last[len(last)-1])
	}
}

func mustJSON(t *testing.T, st *saved) json.RawMessage {
	st.mu.Lock()
	defer st.mu.Unlock()
	data, err := json.Marshal(st.s)
	if err != nil {
		t.Fatal(err)
	}
	return data
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

func TestEndSession(t *testing.T) {
	for _, authorized := range []bool{false, true} {
		calls := 0
		err := endSession(context.Background(), &fakeAuth{authorized: authorized}, func(context.Context) error {
			calls++
			return nil
		})
		if err != nil || calls != map[bool]int{false: 0, true: 1}[authorized] {
			t.Errorf("authorized %v: logged out %d times, %v", authorized, calls, err)
		}
	}
}

func TestLogOutWithoutSession(t *testing.T) {
	cfg := json.RawMessage(`{"api_id": 1, "api_hash": "abc", "phone": "+4712345678"}`)
	// Neither connects: there is no session to end.
	for _, st := range []json.RawMessage{nil, json.RawMessage(`{"phone": "+4712345678"}`), json.RawMessage(`{"phone": "+4799999999", "session": "c2Vzc2lvbg=="}`)} {
		if err := logOut(context.Background(), bridge.Info{Config: cfg, State: st}); err != nil {
			t.Errorf("state %s: %v", st, err)
		}
	}
	if err := logOut(context.Background(), bridge.Info{Config: json.RawMessage(`{}`)}); err != nil {
		t.Errorf("bad config: %v", err)
	}
}

func TestReactions(t *testing.T) {
	api := &fakeAPI{answer: func(req bin.Encoder) (bin.Encoder, error) { return &tg.Updates{}, nil }}
	c := newConn(t, &fakeHost{}, api)
	q := newQueue()
	r := &receiver{st: c.st, q: q}
	thumbs := &tg.ReactionEmoji{Emoticon: "👍"}
	list := func(count int, mine bool, recent ...tg.MessagePeerReaction) tg.MessageReactions {
		rc := tg.ReactionCount{Reaction: thumbs, Count: count}
		if mine {
			rc.SetChosenOrder(0)
		}
		l := tg.MessageReactions{RecentReactions: recent}
		if count > 0 {
			l.Results = []tg.ReactionCount{rc}
		}
		return l
	}
	bob := tg.MessagePeerReaction{PeerID: &tg.PeerUser{UserID: 8}, Reaction: thumbs}
	me := tg.MessagePeerReaction{PeerID: &tg.PeerUser{UserID: selfID}, Reaction: thumbs, My: true}
	steps := []tg.MessageReactions{
		list(1, false, bob),    // Bob reacts
		list(2, true, bob, me), // so does the owner, from their phone
		list(1, true, me),      // Bob takes his back
		list(0, false),         // and the owner theirs
		list(0, false),         // nothing new
	}
	want := []string{"user:8 👍 +", "self 👍 +", "user:8 👍 -", "self 👍 -"}
	var got []string
	for _, l := range steps {
		u := &tg.Updates{
			Users:   []tg.UserClass{&tg.User{ID: 8, AccessHash: 88, FirstName: "Bob"}},
			Chats:   []tg.ChatClass{&tg.Chat{ID: 50, Title: "Family"}},
			Updates: []tg.UpdateClass{&tg.UpdateMessageReactions{Peer: &tg.PeerChat{ChatID: 50}, MsgID: 3, Reactions: l}},
		}
		if err := r.Handle(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	for _, in := range q.items {
		x := in.Reaction
		if x == nil || in.Chat.ID != "chat:50" || x.Target != "3" {
			t.Fatalf("not a reaction to message 3: %+v", in)
		}
		who := in.Author.ID
		if in.Author.Self {
			who = "self"
		}
		sign := "+"
		if x.Removed {
			sign = "-"
		}
		got = append(got, who+" "+x.Key+" "+sign)
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}

	// Sending sets the account's reaction, or clears it.
	ctx := context.Background()
	c.setAPI(tg.NewClient(api))
	if _, err := c.React(ctx, bridge.Reaction{Chat: "chat:50", Target: "3", Key: "🔥"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.React(ctx, bridge.Reaction{Chat: "chat:50", Target: "3", Key: "🔥", Removed: true}); err != nil {
		t.Fatal(err)
	}
	calls := api.called()
	add, ok1 := calls[0].(*tg.MessagesSendReactionRequest)
	del, ok2 := calls[1].(*tg.MessagesSendReactionRequest)
	if !ok1 || !ok2 || add.MsgID != 3 || len(add.Reaction) != 1 || add.Reaction[0].(*tg.ReactionEmoji).Emoticon != "🔥" || len(del.Reaction) != 0 {
		t.Errorf("calls = %+v", calls)
	}
}
