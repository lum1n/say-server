package email

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/lum1n/say-v2/internal/bridge"
)

func TestMain(m *testing.M) {
	pollEvery = 200 * time.Millisecond
	idleFor = time.Second
	os.Exit(m.Run())
}

// host is a fake bridge.Host.
type host struct {
	mu        sync.Mutex
	got       []bridge.Incoming
	state     json.RawMessage
	connected chan struct{}
	once      sync.Once
}

func newHost() *host { return &host{connected: make(chan struct{})} }

func (h *host) Receive(ctx context.Context, m bridge.Incoming) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, m)
	return nil
}

func (h *host) Ask(ctx context.Context, p bridge.Prompt) (string, error) {
	return "", errors.New("no prompts here")
}

func (h *host) SaveState(ctx context.Context, s json.RawMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state = append(json.RawMessage(nil), s...)
	return nil
}

func (h *host) Chats(ctx context.Context) ([]string, error) { return nil, nil }

func (h *host) Connected(ctx context.Context) { h.once.Do(func() { close(h.connected) }) }

func (h *host) received() []bridge.Incoming {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]bridge.Incoming(nil), h.got...)
}

func (h *host) saved() json.RawMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state
}

// waitFor waits until the host has received n messages.
func (h *host) waitFor(t *testing.T, n int) []bridge.Incoming {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got := h.received(); len(got) >= n {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("got %d messages, want %d: %+v", len(h.received()), n, h.received())
	return nil
}

// mailServer is an in-memory IMAP server and a fake SMTP server.
type mailServer struct {
	user *imapmemserver.User
	imap string
	smtp *smtpServer
}

func newMailServer(t *testing.T) *mailServer {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("me@example.com", "pw")
	for _, mb := range []string{"INBOX", "Sent"} {
		if err := user.Create(mb, nil); err != nil {
			t.Fatal(err)
		}
	}
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
		InsecureAuth: true,
		Logger:       quiet{},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return &mailServer{user: user, imap: ln.Addr().String(), smtp: newSMTPServer(t)}
}

type quiet struct{}

func (quiet) Printf(string, ...any) {}

// deliver puts a raw message in a folder, received at.
func (s *mailServer) deliver(t *testing.T, folder, raw string, at time.Time) {
	t.Helper()
	raw = strings.ReplaceAll(raw, "\n", "\r\n")
	if _, err := s.user.Append(folder, bytes.NewReader([]byte(raw)), &imap.AppendOptions{Time: at}); err != nil {
		t.Fatal(err)
	}
}

func (s *mailServer) config(sent string) json.RawMessage {
	b, _ := json.Marshal(config{
		Address: "Me@example.com", Username: "me@example.com", Name: "Me Myself", IMAP: s.imap, SMTP: s.smtp.addr,
		Password: "pw", Security: "none", SentFolder: sent,
	})
	return b
}

// run runs a connection until the test ends.
func run(t *testing.T, cfg, st json.RawMessage) (*conn, *host, func()) {
	t.Helper()
	h := newHost()
	c, err := open(bridge.Info{ID: "b1", Config: cfg, State: st}, h)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	stop := func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("run: %v", err)
		}
	}
	select {
	case <-h.connected:
	case err := <-done:
		t.Fatalf("run: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("not connected")
	}
	return c.(*conn), h, stop
}

func TestReceive(t *testing.T) {
	s := newMailServer(t)
	now := time.Now()
	day := 24 * time.Hour
	s.deliver(t, "INBOX", mailFrom("old@example.com", "<old@x>", "Ancient", "Too old."), now.Add(-40*day))
	s.deliver(t, "INBOX", `From: Bob Builder <Bob@Example.com>
To: Me <me@example.com>
Subject: Hello
Message-ID: <b1@example.com>
Date: `+now.Add(-3*day).Format(time.RFC1123Z)+`
Content-Type: text/plain

Hi there!

`+"-- \n"+`Bob, builder
`, now.Add(-3*day))
	s.deliver(t, "INBOX", `From: Carol <carol@example.com>
To: me@example.com, bob@example.com
Subject: Re: Plans
In-Reply-To: <plans@example.com>
Message-ID: <c1@example.com>
Date: `+now.Add(-2*day).Format(time.RFC1123Z)+`
Content-Type: multipart/mixed; boundary=XX

--XX
Content-Type: multipart/alternative; boundary=YY

--YY
Content-Type: text/html

<p>Sounds <b>good</b>, see <a href="https://example.com/x">this</a>.</p><div class="gmail_quote">On Mon, Bob wrote:<blockquote>old stuff</blockquote></div>
--YY--
--XX
Content-Type: image/png
Content-Disposition: attachment; filename="map.png"

xxxx
--XX--
`, now.Add(-2*day))
	// Sent from the phone: the owner's own message, in the sent folder.
	s.deliver(t, "Sent", `From: Me <me@example.com>
To: Bob <bob@example.com>
Subject: Re: Hello
In-Reply-To: <b1@example.com>
Message-ID: <m1@example.com>
Date: `+now.Add(-1*day).Format(time.RFC1123Z)+`
Content-Type: text/plain

Hello Bob.

On Tue, Bob Builder <bob@example.com>
wrote:
> Hi there!
`, now.Add(-1*day))

	_, h, stop := run(t, s.config("Sent"), nil)
	defer stop()
	got := h.waitFor(t, 3)
	want := []bridge.Incoming{
		{Chat: bridge.RemoteChat{ID: "bob@example.com", Name: "Bob Builder", Direct: true}, ID: "b1@example.com",
			Author: bridge.Author{ID: "bob@example.com", Name: "Bob Builder"}, Text: "Subject: Hello\nHi there!"},
		{Chat: bridge.RemoteChat{ID: "bob@example.com,carol@example.com", Name: "bob@example.com, Carol"}, ID: "c1@example.com",
			Author: bridge.Author{ID: "carol@example.com", Name: "Carol"}, ReplyTo: "plans@example.com",
			Text: "Sounds good, see this (https://example.com/x).\n[attachment: map.png]"},
		{Chat: bridge.RemoteChat{ID: "bob@example.com", Name: "Bob", Direct: true}, ID: "m1@example.com",
			Author: bridge.Author{ID: "me@example.com", Name: "Me", Self: true}, ReplyTo: "b1@example.com", Text: "Hello Bob."},
	}
	for i := range want {
		g := got[i]
		g.Time = time.Time{}
		if fmt.Sprint(g) != fmt.Sprint(want[i]) {
			t.Errorf("message %d:\n got %+v\nwant %+v", i, g, want[i])
		}
	}

	// New mail arrives while idling.
	s.deliver(t, "INBOX", mailFrom("dave@example.com", "<d1@example.com>", "News", "Fresh."), time.Now())
	got = h.waitFor(t, 4)
	if g := got[3]; g.ID != "d1@example.com" || g.Text != "Subject: News\nFresh." || !g.Chat.Direct {
		t.Errorf("new mail: %+v", g)
	}
	if len(got) != 4 {
		t.Errorf("extra messages: %+v", got[4:])
	}
}

func TestStripQuotes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Yes.\n\nOn Mon, 1 Jan 2026, Ann <a@x> wrote:\n> Coming?\n> \n", "Yes."},
		{"On Mon, Ann wrote:\n> q1\nanswer 1\n> q2\nanswer 2\n", "answer 1\nanswer 2"},
		{"Sure\r\n-- \r\nAnn\r\n", "Sure"},
		{"> only a quote\n", "> only a quote"},
		{"Top\n\n-----Original Message-----\nFrom: x\n", "Top"},
	}
	for _, c := range cases {
		if got := stripQuotes(c.in); got != c.want {
			t.Errorf("stripQuotes(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSendAndEcho(t *testing.T) {
	s := newMailServer(t)
	s.deliver(t, "INBOX", mailFrom("bob@example.com", "<b1@example.com>", "Lunch?", "Today at noon?"), time.Now())
	c, h, stop := run(t, s.config("Sent"), nil)
	defer stop()
	h.waitFor(t, 1)

	id, err := c.Send(context.Background(), bridge.Outgoing{Chat: "bob@example.com", EventID: "ev1", Text: "Yes!\nSee you.", Sender: "Me"})
	if err != nil {
		t.Fatal(err)
	}
	if id != "ev1@example.com" {
		t.Errorf("id = %q", id)
	}
	sent := s.smtp.messages()
	if len(sent) != 1 {
		t.Fatalf("smtp got %d messages", len(sent))
	}
	m := sent[0]
	if m.from != "Me@example.com" || strings.Join(m.to, ",") != "bob@example.com" {
		t.Errorf("envelope %q -> %q", m.from, m.to)
	}
	for _, want := range []string{
		"Message-Id: <ev1@example.com>", "In-Reply-To: <b1@example.com>", "References: <b1@example.com>",
		"Subject: Re: Lunch?", `From: "Me Myself" <Me@example.com>`, "To: <bob@example.com>", "Yes!\r\nSee you.",
	} {
		if !strings.Contains(m.data, want) {
			t.Errorf("sent mail lacks %q:\n%s", want, m.data)
		}
	}
	// The copy filed in the sent folder comes back as the echo.
	got := h.waitFor(t, 2)
	if e := got[1]; e.ID != id || !e.Author.Self || e.Chat.ID != "bob@example.com" || e.Text != "Yes!\nSee you." {
		t.Errorf("echo: %+v", e)
	}

	// A second message answers the first, without a second "Re:".
	if _, err := c.Send(context.Background(), bridge.Outgoing{Chat: "bob@example.com", EventID: "ev2", Text: "Oh, and…"}); err != nil {
		t.Fatal(err)
	}
	m = s.smtp.messages()[1]
	if !strings.Contains(m.data, "In-Reply-To: <ev1@example.com>") || !strings.Contains(m.data, "Subject: Re: Lunch?\r\n") {
		t.Errorf("second mail:\n%s", m.data)
	}

	// A fresh chat gets a default subject.
	if _, err := c.Send(context.Background(), bridge.Outgoing{Chat: "ann@example.com,zed@example.com", EventID: "ev3", Text: "Hi both"}); err != nil {
		t.Fatal(err)
	}
	m = s.smtp.messages()[2]
	if !strings.Contains(m.data, "Subject: Message from say") || strings.Contains(m.data, "In-Reply-To") || len(m.to) != 2 {
		t.Errorf("new chat mail %v:\n%s", m.to, m.data)
	}
}

func TestSendRejected(t *testing.T) {
	s := newMailServer(t)
	s.smtp.reject = "nobody@example.com"
	c, _, stop := run(t, s.config(""), nil)
	defer stop()
	_, err := c.Send(context.Background(), bridge.Outgoing{Chat: "nobody@example.com", EventID: "ev1", Text: "Hi"})
	if !errors.Is(err, bridge.ErrRejected) {
		t.Errorf("err = %v, want rejected", err)
	}
	if _, ok := any(c).(bridge.Editor); ok {
		t.Error("mail cannot be edited")
	}
}

func TestResume(t *testing.T) {
	s := newMailServer(t)
	s.deliver(t, "INBOX", mailFrom("bob@example.com", "<b1@example.com>", "One", "1"), time.Now())
	_, h, stop := run(t, s.config(""), nil)
	h.waitFor(t, 1)
	stop()
	st := h.saved()
	var parsed state
	if err := json.Unmarshal(st, &parsed); err != nil || parsed.Folders["INBOX"].UID == 0 {
		t.Fatalf("state %s: %v", st, err)
	}
	if parsed.Threads["bob@example.com"].MessageID != "b1@example.com" {
		t.Errorf("threads: %s", st)
	}

	// Mail that came while say was down, and nothing read twice.
	s.deliver(t, "INBOX", mailFrom("bob@example.com", "<b2@example.com>", "Two", "2"), time.Now())
	_, h, stop = run(t, s.config(""), st)
	got := h.waitFor(t, 1)
	time.Sleep(300 * time.Millisecond)
	if got = h.received(); len(got) != 1 || got[0].ID != "b2@example.com" {
		t.Errorf("after resume: %+v", got)
	}
	stop()

	// A renumbered folder is read afresh; the core drops the repeats.
	parsed.Folders["INBOX"] = position{Validity: 999, UID: 1}
	st, _ = json.Marshal(parsed)
	_, h, stop = run(t, s.config(""), st)
	defer stop()
	if got := h.waitFor(t, 2); got[0].ID != "b1@example.com" || got[1].ID != "b2@example.com" {
		t.Errorf("after renumbering: %+v", got)
	}
}

func TestOpen(t *testing.T) {
	n, ok := bridge.Lookup("email")
	if !ok {
		t.Fatal("not registered")
	}
	for _, cfg := range []string{
		`{"address":"nope","imap":"a:1","smtp":"b:2","password":"x"}`,
		`{"address":"a@b","imap":"a","smtp":"b:2","password":"x"}`,
		`{"address":"a@b","imap":"a:1","smtp":"b:2","password":"x","security":"maybe"}`,
	} {
		if _, err := n.Open(bridge.Info{Config: json.RawMessage(cfg)}, newHost()); err == nil {
			t.Errorf("open %s: no error", cfg)
		}
	}
}

func mailFrom(from, id, subject, body string) string {
	return fmt.Sprintf("From: %s\nTo: me@example.com\nSubject: %s\nMessage-ID: %s\nDate: %s\nContent-Type: text/plain\n\n%s\n",
		from, subject, id, time.Now().Format(time.RFC1123Z), body)
}

// smtpServer is a fake SMTP server that keeps what it is given.
type smtpServer struct {
	addr   string
	reject string // a recipient refused with 550

	mu   sync.Mutex
	msgs []smtpMessage
}

type smtpMessage struct {
	from string
	to   []string
	data string
}

func newSMTPServer(t *testing.T) *smtpServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &smtpServer{addr: ln.Addr().String()}
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(nc)
		}
	}()
	return s
}

func (s *smtpServer) messages() []smtpMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]smtpMessage(nil), s.msgs...)
}

func (s *smtpServer) serve(nc net.Conn) {
	defer nc.Close()
	r := bufio.NewReader(nc)
	say := func(l string) { fmt.Fprintf(nc, "%s\r\n", l) }
	say("220 fake ESMTP")
	var m smtpMessage
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			say("250-fake")
			say("250 8BITMIME")
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			m = smtpMessage{from: strings.Trim(line[len("MAIL FROM:"):], "<> ")}
			if i := strings.Index(m.from, ">"); i >= 0 {
				m.from = m.from[:i]
			}
			say("250 ok")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			to := strings.Trim(line[len("RCPT TO:"):], "<> ")
			if to == s.reject {
				say("550 no such user")
				continue
			}
			m.to = append(m.to, to)
			say("250 ok")
		case cmd == "DATA":
			say("354 go on")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(strings.TrimPrefix(l, "."))
			}
			m.data = b.String()
			s.mu.Lock()
			s.msgs = append(s.msgs, m)
			s.mu.Unlock()
			say("250 queued")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func TestBigMessage(t *testing.T) {
	s := newMailServer(t)
	big := strings.Repeat(strings.Repeat("A", 76)+"\n", 10000)
	s.deliver(t, "INBOX", `From: bob@example.com
To: me@example.com
Subject: Photos
Message-ID: <big@example.com>
Content-Type: multipart/mixed; boundary=XX

--XX
Content-Type: text/plain

Look!
--XX
Content-Type: image/jpeg; name="one.jpg"
Content-Disposition: attachment; filename="one.jpg"
Content-Transfer-Encoding: base64

`+big+`--XX--
`, time.Now())
	_, h, stop := run(t, s.config(""), nil)
	defer stop()
	if got := h.waitFor(t, 1); got[0].Text != "Subject: Photos\nLook!\n[attachment: one.jpg]" {
		t.Errorf("text = %q", got[0].Text)
	}
}

func TestForgedSelf(t *testing.T) {
	s := newMailServer(t)
	// Anyone can write the owner's address in a From.
	s.deliver(t, "INBOX", `From: Me <me@example.com>
To: me@example.com
Cc: Xavier <x@example.com>
Subject: Transfer the money
Message-ID: <forged@example.com>
Content-Type: text/plain

Do it now.
`, time.Now())
	_, h, stop := run(t, s.config("Sent"), nil)
	defer stop()
	got := h.waitFor(t, 1)[0]
	want := bridge.Author{ID: "me@example.com", Name: "Me (unverified)"}
	if got.Author != want || got.Chat.ID != "x@example.com" {
		t.Errorf("forged mail: %+v", got)
	}
}

// stalled is a server that accepts connections and never answers.
func stalled(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, nc)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, nc := range conns {
			nc.Close()
		}
	})
	return ln.Addr().String()
}

func TestStalledServers(t *testing.T) {
	s := newMailServer(t)
	newConn := func(imapAddr, smtpAddr string) *conn {
		cfg, _ := json.Marshal(config{Address: "me@example.com", IMAP: imapAddr, SMTP: smtpAddr, Password: "pw", Security: "none", SentFolder: "Sent"})
		c, err := open(bridge.Info{Config: cfg}, newHost())
		if err != nil {
			t.Fatal(err)
		}
		return c.(*conn)
	}
	quick := func(what string, f func()) {
		t.Helper()
		start := time.Now()
		f()
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s took %v", what, d)
		}
	}

	// A stalled IMAP server holds up neither Send nor the append.
	c := newConn(stalled(t), s.smtp.addr)
	quick("send", func() {
		if _, err := c.Send(context.Background(), bridge.Outgoing{Chat: "bob@example.com", EventID: "ev1", Text: "Hi"}); err != nil {
			t.Error(err)
		}
	})
	quick("append", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		if err := c.appendSent(ctx, []byte("x"), time.Now()); err == nil {
			t.Error("append to a stalled server worked")
		}
	})

	// A stalled SMTP server fails the send when its context ends, with
	// the outcome unknown.
	c = newConn(s.imap, stalled(t))
	quick("stalled smtp", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, err := c.Send(ctx, bridge.Outgoing{Chat: "bob@example.com", EventID: "ev2", Text: "Hi"})
		if err == nil || errors.Is(err, bridge.ErrRejected) {
			t.Errorf("err = %v, want unknown", err)
		}
	})
}
