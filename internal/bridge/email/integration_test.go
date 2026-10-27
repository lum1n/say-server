package email

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/smtp"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/lum1n/say-server/internal/bridge"
)

// TestGreenMail runs against a real mail server:
//
//	docker run -d --name say-test-email-greenmail -p 127.0.0.1:18964:3025 -p 127.0.0.1:18965:3143 \
//	  -e GREENMAIL_OPTS='-Dgreenmail.setup.test.smtp -Dgreenmail.setup.test.imap -Dgreenmail.hostname=0.0.0.0 -Dgreenmail.users=me:pw@example.com,bob:pw@example.com -Dgreenmail.users.login=email' \
//	  greenmail/standalone
//	SAY_TEST_EMAIL=127.0.0.1 go test -run GreenMail ./internal/bridge/email/
func TestGreenMail(t *testing.T) {
	host := os.Getenv("SAY_TEST_EMAIL")
	if host == "" {
		t.Skip("SAY_TEST_EMAIL is not set")
	}
	imapAddr, smtpAddr := net.JoinHostPort(host, "18965"), net.JoinHostPort(host, "18964")
	run := fmt.Sprint(time.Now().UnixNano())

	// The sent folder, which GreenMail does not make by itself.
	cl, err := imapclient.DialInsecure(imapAddr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Login("me@example.com", "pw").Wait(); err != nil {
		t.Fatal(err)
	}
	cl.Create("Sent", nil).Wait() // it may be there from an earlier run
	cl.Logout().Wait()
	cl.Close()

	// Bob writes first.
	bobID := "bob-" + run + "@example.com"
	mail := "From: Bob <bob@example.com>\r\nTo: me@example.com\r\nSubject: Ping " + run + "\r\n" +
		"Message-ID: <" + bobID + ">\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\nAre you there?\r\n"
	if err := smtp.SendMail(smtpAddr, nil, "bob@example.com", []string{"me@example.com"}, []byte(mail)); err != nil {
		t.Fatal(err)
	}

	cfg, _ := json.Marshal(config{
		Address: "me@example.com", Name: "Me", IMAP: imapAddr, SMTP: smtpAddr,
		Password: "pw", Security: "none", SentFolder: "Sent",
	})
	h := newHost()
	bc, err := open(bridge.Info{ID: "b1", Config: cfg}, h)
	if err != nil {
		t.Fatal(err)
	}
	c := bc.(*conn)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	select {
	case <-h.connected:
	case err := <-done:
		t.Fatalf("run: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("not connected")
	}
	in := waitID(t, h, bobID)
	if in.Chat.ID != "bob@example.com" || !in.Chat.Direct || in.Author.Self || in.Text != "Subject: Ping "+run+"\nAre you there?" {
		t.Errorf("incoming: %+v", in)
	}

	id, err := c.Send(ctx, bridge.Outgoing{Chat: "bob@example.com", EventID: "ev" + run, Text: "Yes, here."})
	if err != nil {
		t.Fatal(err)
	}
	if id != "ev"+run+"@example.com" {
		t.Errorf("id = %q", id)
	}
	// The copy in the sent folder is the echo.
	if e := waitID(t, h, id); !e.Author.Self || e.Chat.ID != "bob@example.com" || e.Text != "Yes, here." {
		t.Errorf("echo: %+v", e)
	}

	// Bob has it, threaded under his mail.
	raw := bobMail(t, imapAddr, id)
	for _, want := range []string{"In-Reply-To: <" + bobID + ">", "References: <" + bobID + ">", "Subject: Re: Ping " + run, "Yes, here."} {
		if !strings.Contains(raw, want) {
			t.Errorf("bob's copy lacks %q:\n%s", want, raw)
		}
	}
}

// waitID waits for a message to be received.
func waitID(t *testing.T, h *host, id string) bridge.Incoming {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range h.received() {
			if m.ID == id {
				return m
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s did not arrive; got %+v", id, h.received())
	return bridge.Incoming{}
}

// bobMail returns the message in Bob's inbox with a Message-ID.
func bobMail(t *testing.T, addr, id string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		cl, err := imapclient.DialInsecure(addr, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := cl.Login("bob@example.com", "pw").Wait(); err != nil {
			t.Fatal(err)
		}
		if _, err := cl.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
		res, err := cl.UIDSearch(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: id}}}, nil).Wait()
		if err != nil {
			t.Fatal(err)
		}
		if uids := res.AllUIDs(); len(uids) > 0 {
			bufs, err := cl.Fetch(imap.UIDSetNum(uids[0]), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect()
			cl.Close()
			if err != nil || len(bufs) == 0 || len(bufs[0].BodySection) == 0 {
				t.Fatalf("fetch: %v", err)
			}
			return string(bufs[0].BodySection[0].Bytes)
		}
		cl.Close()
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s is not in bob's inbox", id)
	return ""
}
