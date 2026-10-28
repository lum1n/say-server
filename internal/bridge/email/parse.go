package email

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	gomessage "github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // mail in other charsets than UTF-8
	"github.com/emersion/go-message/mail"
	"golang.org/x/net/html"

	"github.com/lum1n/say-server/internal/bridge"
	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/media"
)

// message is a mail as say sees it.
type message struct {
	ID   string // Message-ID, without angle brackets
	From *mail.Address
	// To are the recipients, To and Cc.
	To        []*mail.Address
	Subject   string
	InReplyTo string
	Date      time.Time
	// Text is the readable body: quotes and signature cut, and the
	// attachments not in Files named.
	Text string
	// Files are the attachments read whole.
	Files []file
	// Sent says it was read from the sent folder, the only place mail
	// is trusted to be the owner's: anyone can put their address in a
	// From.
	Sent bool
	// Cut says raw was cut short at maxBody, so Files are left out; Size
	// and UID say where to read all of it.
	Cut  bool
	Size int64
	UID  imap.UID
	// Folder is where the message was read.
	Folder string
}

// file is an attachment of a mail.
type file struct {
	Name, Mime string
	Data       []byte
}

// maxPart is how much of one text part is read.
const maxPart = 256 << 10

// parse reads a raw message. Its attachments are kept as Files when
// files is set and raw is the whole message; otherwise they are named.
func parse(raw []byte, files bool) (*message, error) {
	r, err := mail.CreateReader(bytes.NewReader(raw))
	if r == nil {
		return nil, err
	}
	h := r.Header
	m := &message{}
	m.ID, _ = h.MessageID()
	if m.ID == "" {
		// Without a Message-ID the content is the only stable key.
		sum := sha256.Sum256(raw)
		m.ID = hex.EncodeToString(sum[:16]) + "@say.invalid"
	}
	if from, _ := h.AddressList("From"); len(from) > 0 {
		m.From = from[0]
	}
	if m.From == nil {
		return nil, errors.New("email: a message without a sender")
	}
	for _, k := range []string{"To", "Cc"} {
		l, _ := h.AddressList(k)
		m.To = append(m.To, l...)
	}
	m.Subject, _ = h.Subject()
	m.Subject = strings.TrimSpace(m.Subject)
	if ids, _ := h.MsgIDList("In-Reply-To"); len(ids) > 0 {
		m.InReplyTo = ids[0]
	}
	m.Date, _ = h.Date()

	var plain, htm string
	var named []string
	// keep reads an attachment, or names it when it is not wanted or
	// can't be read whole.
	keep := func(name, typ string, body io.Reader) {
		if typ == "" {
			typ = "application/octet-stream"
		}
		if files {
			b, err := io.ReadAll(io.LimitReader(body, media.DefaultMax+1))
			if err == nil && len(b) > 0 && len(b) <= media.DefaultMax {
				m.Files = append(m.Files, file{Name: name, Mime: typ, Data: b})
				return
			}
		}
		named = append(named, core.Attachment{Name: name, Mime: typ}.Placeholder())
	}
	for {
		p, err := r.NextPart()
		if err != nil && !(p != nil && gomessage.IsUnknownCharset(err)) {
			// The end, or a message cut short: keep what was read.
			break
		}
		switch ph := p.Header.(type) {
		case *mail.InlineHeader:
			t, params, _ := ph.ContentType()
			switch {
			case t == "text/plain" && plain == "":
				plain = readPart(p.Body)
			case t == "text/html" && htm == "":
				htm = readPart(p.Body)
			case !strings.HasPrefix(t, "text/"):
				keep(partName(params["name"], t), t, p.Body)
			}
		case *mail.AttachmentHeader:
			name, _ := ph.Filename()
			t, _, _ := ph.ContentType()
			keep(partName(name, t), t, p.Body)
		}
	}
	body := plain
	if strings.TrimSpace(body) == "" && htm != "" {
		body = htmlText(htm)
	}
	var lines []string
	if m.InReplyTo == "" && m.Subject != "" {
		lines = append(lines, "Subject: "+m.Subject)
	}
	if body = stripQuotes(body); body != "" {
		lines = append(lines, body)
	}
	lines = append(lines, named...)
	m.Text = strings.Join(lines, "\n")
	if m.Text == "" && len(m.Files) == 0 {
		m.Text = "[empty message]"
	}
	return m, nil
}

func readPart(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, maxPart))
	return string(b)
}

func partName(name, contentType string) string {
	if name != "" {
		return name
	}
	if contentType != "" {
		return contentType
	}
	return "file"
}

// incoming makes a message say's: the chat is everyone on it but the
// owner. Mail with nobody else on it has no chat and is skipped.
func (c *conn) incoming(m *message) (bridge.Incoming, bool) {
	from := strings.ToLower(strings.TrimSpace(m.From.Address))
	names := map[string]string{}
	add := func(a *mail.Address) {
		addr := strings.ToLower(strings.TrimSpace(a.Address))
		if addr == "" || addr == c.self {
			return
		}
		// The first real name given for an address wins.
		if n, ok := names[addr]; !ok || n == addr {
			names[addr] = displayName(a)
		}
	}
	add(m.From)
	for _, a := range m.To {
		add(a)
	}
	if len(names) == 0 {
		return bridge.Incoming{}, false
	}
	addrs := make([]string, 0, len(names))
	for a := range names {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	shown := make([]string, len(addrs))
	for i, a := range addrs {
		shown[i] = names[a]
	}
	return bridge.Incoming{
		Chat:    bridge.RemoteChat{ID: strings.Join(addrs, ","), Name: strings.Join(shown, ", "), Direct: len(addrs) == 1},
		ID:      m.ID,
		Author:  c.author(m, from),
		Text:    m.Text,
		Media:   mediaOf(m.Files),
		Time:    m.Date,
		ReplyTo: m.InReplyTo,
	}, true
}

// mediaOf hands a mail's files to say, which reads them from memory.
func mediaOf(files []file) []bridge.Media {
	var out []bridge.Media
	for _, f := range files {
		out = append(out, bridge.Media{
			Mime: f.Mime, Name: f.Name, Size: int64(len(f.Data)),
			Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(f.Data)), nil },
		})
	}
	return out
}

// author is who wrote a message. Mail from the owner's address outside
// the sent folder may be forged, so it comes in as someone else.
func (c *conn) author(m *message, from string) bridge.Author {
	a := bridge.Author{ID: from, Name: displayName(m.From)}
	switch {
	case from != c.self:
	case m.Sent:
		a.Self = true
	default:
		a.Name += " (unverified)"
	}
	return a
}

func displayName(a *mail.Address) string {
	if n := strings.TrimSpace(a.Name); n != "" {
		return n
	}
	return strings.ToLower(strings.TrimSpace(a.Address))
}

var (
	// wrote is the line a mail client puts above a quoted reply.
	wrote = regexp.MustCompile(`^On\b.*\bwrote:$`)
	// wroteEnd is the second half of one wrapped onto two lines.
	wroteEnd = regexp.MustCompile(`\bwrote:$`)
)

// stripQuotes cuts the quoted mail under a reply and the signature, and
// drops quoted lines in between, so what is left is what the person
// wrote.
func stripQuotes(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	var out []string
	// rest reports whether lines after i hold anything but quotes, as
	// when someone answers between the quoted lines.
	rest := func(i int) bool {
		for _, l := range lines[i:] {
			if l == "-- " {
				return false
			}
			if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, ">") {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(lines); i++ {
		if lines[i] == "-- " {
			break
		}
		l := strings.TrimRight(lines[i], " \t")
		t := strings.TrimSpace(l)
		attribution := 0
		switch {
		case wrote.MatchString(t):
			attribution = 1
		case strings.HasPrefix(t, "On ") && i+1 < len(lines) && wroteEnd.MatchString(strings.TrimSpace(lines[i+1])):
			attribution = 2
		}
		if attribution > 0 {
			if !rest(i + attribution) {
				break
			}
			i += attribution - 1
			continue
		}
		if strings.HasPrefix(t, "-----Original Message-----") {
			break
		}
		if strings.HasPrefix(t, ">") {
			continue
		}
		out = append(out, l)
	}
	res := strings.TrimSpace(strings.Join(out, "\n"))
	res = blankRuns.ReplaceAllString(res, "\n\n")
	if res == "" {
		// All of it was quoted: better the quote than nothing.
		return strings.TrimSpace(s)
	}
	return res
}

var (
	blankRuns = regexp.MustCompile(`\n{3,}`)
	spaceRuns = regexp.MustCompile(`[ \t\r\n\f]+`)
)

// htmlText turns an HTML body into plain text. Quoted mail, which HTML
// clients put in a blockquote, is left out.
func htmlText(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	skip := 0
	var href string
	linkStart := 0
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			lines := strings.Split(b.String(), "\n")
			for i, l := range lines {
				lines[i] = strings.TrimSpace(l)
			}
			return strings.TrimSpace(blankRuns.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
		case html.TextToken:
			if skip == 0 {
				b.WriteString(spaceRuns.ReplaceAllString(string(z.Text()), " "))
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			switch tag := string(name); tag {
			case "script", "style", "head", "title", "blockquote":
				if tt == html.StartTagToken {
					skip++
				}
			case "br":
				b.WriteString("\n")
			case "li":
				b.WriteString("\n- ")
			case "p", "div", "tr", "table", "ul", "ol", "h1", "h2", "h3", "h4", "h5", "h6", "hr":
				b.WriteString("\n")
			case "a":
				href = ""
				for hasAttr {
					var k, v []byte
					k, v, hasAttr = z.TagAttr()
					if string(k) == "href" {
						href = string(v)
					}
				}
				linkStart = b.Len()
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "script", "style", "head", "title", "blockquote":
				if skip > 0 {
					skip--
				}
			case "p", "div", "tr", "table", "ul", "ol", "h1", "h2", "h3", "h4", "h5", "h6":
				b.WriteString("\n")
			case "a":
				text := strings.TrimSpace(b.String()[min(linkStart, b.Len()):])
				if skip == 0 && href != "" && text != href && !strings.HasPrefix(href, "mailto:") && !strings.HasPrefix(href, "#") {
					b.WriteString(" (" + href + ")")
				}
				href = ""
			}
		}
	}
}
