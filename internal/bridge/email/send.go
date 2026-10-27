package email

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"net/textproto"
	"regexp"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/mail"

	"github.com/lum1n/say-server/internal/bridge"
)

// Send mails the chat's people as the owner, answering the last message
// of the chat. The Message-ID it sets is the one the copy in the sent
// folder will have, so it is the remote id.
func (c *conn) Send(ctx context.Context, m bridge.Outgoing) (string, error) {
	var to []*mail.Address
	for _, a := range strings.Split(m.Chat, ",") {
		if a = strings.TrimSpace(a); a != "" {
			to = append(to, &mail.Address{Address: a})
		}
	}
	if len(to) == 0 {
		return "", bridge.Rejected(errors.New("email: a chat without addresses"))
	}
	t := c.thread(m.Chat)
	id := m.EventID + "@" + c.domain()
	subject := "Message from say"
	if t.Subject != "" {
		subject = "Re: " + rePrefix.ReplaceAllString(t.Subject, "")
	}
	parent := t.MessageID
	if m.ReplyTo != "" {
		parent = m.ReplyTo
	}
	now := time.Now()
	raw, err := c.compose(to, id, subject, parent, m.Text, now)
	if err != nil {
		return "", err
	}
	addrs := make([]string, len(to))
	for i, a := range to {
		addrs[i] = a.Address
	}
	if err := c.sendSMTP(ctx, addrs, raw); err != nil {
		return "", err
	}
	// From here on the mail is out: what fails is only logged.
	c.noteThread(m.Chat, thread{MessageID: id, Subject: subject, At: now})
	if err := c.save(ctx); err != nil {
		slog.Warn("email: save state", "err", err)
	}
	if c.cfg.SentFolder != "" {
		// Filed after Send returns: the core holds the chat while a send
		// is under way, and a slow IMAP server must not hold it longer.
		go func() {
			actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), appendTimeout)
			defer cancel()
			if err := c.appendSent(actx, raw, now); err != nil {
				slog.Warn("email: file in sent folder", "folder", c.cfg.SentFolder, "err", err)
			}
		}()
	}
	return id, nil
}

// rePrefix matches the reply prefixes of a subject, to add just one.
var rePrefix = regexp.MustCompile(`^(?i)((re|aw|sv|fwd?)\s*:\s*)+`)

// compose builds an RFC 5322 message.
func (c *conn) compose(to []*mail.Address, id, subject, parent, text string, at time.Time) ([]byte, error) {
	var h mail.Header
	h.SetAddressList("From", []*mail.Address{{Name: c.cfg.Name, Address: c.cfg.Address}})
	h.SetAddressList("To", to)
	h.SetDate(at)
	h.SetSubject(subject)
	h.SetMessageID(id)
	if parent != "" {
		h.SetMsgIDList("In-Reply-To", []string{parent})
		h.SetMsgIDList("References", []string{parent})
	}
	h.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
	h.Set("Content-Transfer-Encoding", "quoted-printable")
	var buf bytes.Buffer
	w, err := mail.CreateSingleInlineWriter(&buf, h)
	if err != nil {
		return nil, err
	}
	body := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n")
	if _, err := w.Write([]byte(body + "\r\n")); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// sendSMTP hands a message to the SMTP server. A 5xx reply is a refusal;
// anything else leaves the outcome unknown.
func (c *conn) sendSMTP(ctx context.Context, to []string, raw []byte) error {
	host, _, _ := net.SplitHostPort(c.cfg.SMTP)
	tlsConfig := &tls.Config{ServerName: host}
	d := &net.Dialer{Timeout: dialTimeout}
	var (
		nc  net.Conn
		err error
	)
	implicit := c.cfg.Security != "none" && !c.startTLS(c.cfg.SMTP)
	if implicit {
		nc, err = (&tls.Dialer{NetDialer: d, Config: tlsConfig}).DialContext(ctx, "tcp", c.cfg.SMTP)
	} else {
		nc, err = d.DialContext(ctx, "tcp", c.cfg.SMTP)
	}
	if err != nil {
		return fmt.Errorf("connect to %s: %w", c.cfg.SMTP, err)
	}
	defer nc.Close()
	dl, ok := ctx.Deadline()
	if !ok {
		dl = time.Now().Add(smtpTimeout)
	}
	nc.SetDeadline(dl)
	stop := context.AfterFunc(ctx, func() { nc.Close() })
	defer stop()
	cl, err := smtp.NewClient(nc, host)
	if err != nil {
		return smtpErr(err)
	}
	defer cl.Close()
	if err := cl.Hello(c.domain()); err != nil {
		return smtpErr(err)
	}
	if c.cfg.Security != "none" && !implicit {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return bridge.Rejected(fmt.Errorf("%s does not offer STARTTLS", c.cfg.SMTP))
		}
		if err := cl.StartTLS(tlsConfig); err != nil {
			return smtpErr(err)
		}
	}
	if ok, _ := cl.Extension("AUTH"); ok {
		if err := cl.Auth(smtp.PlainAuth("", c.cfg.Username, c.cfg.Password, host)); err != nil {
			return smtpErr(err)
		}
	}
	if err := cl.Mail(c.cfg.Address); err != nil {
		return smtpErr(err)
	}
	for _, a := range to {
		if err := cl.Rcpt(a); err != nil {
			return smtpErr(err)
		}
	}
	w, err := cl.Data()
	if err != nil {
		return smtpErr(err)
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return smtpErr(err)
	}
	// The mail is accepted; a failed goodbye changes nothing.
	cl.Quit()
	return nil
}

// smtpErr marks a permanent SMTP failure as a refusal.
func smtpErr(err error) error {
	var te *textproto.Error
	if errors.As(err, &te) && te.Code >= 500 && te.Code < 600 {
		return bridge.Rejected(err)
	}
	return err
}

// appendSent files a copy of a sent message in the sent folder, for the
// owner's other mail apps. The receive loop reads it back as the echo.
func (c *conn) appendSent(ctx context.Context, raw []byte, at time.Time) error {
	cl, err := c.dialIMAP(&imapclient.Options{})
	if err != nil {
		return err
	}
	defer cl.Close()
	// Closing the client ends any command waiting on a stalled server.
	stop := context.AfterFunc(ctx, func() { cl.Close() })
	defer stop()
	if err := cl.Login(c.cfg.Username, c.cfg.Password).Wait(); err != nil {
		return err
	}
	cmd := cl.Append(c.cfg.SentFolder, int64(len(raw)), &imap.AppendOptions{Flags: []imap.Flag{imap.FlagSeen}, Time: at})
	if _, err := cmd.Write(raw); err != nil {
		return err
	}
	if err := cmd.Close(); err != nil {
		return err
	}
	if _, err := cmd.Wait(); err != nil {
		return err
	}
	return cl.Logout().Wait()
}
