package say

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	embedded "github.com/lum1n/say-server/say"
	"github.com/lum1n/say-server/say/deploy"
	"github.com/lum1n/say-server/say/exe"
	"github.com/lum1n/say-server/say/exe/exetest"
)

type screen struct {
	text     chan string
	signedIn chan string
	closed   chan string
}

func (s *screen) OnScreen(text string, _, _ int, _ bool) { s.text <- text }
func (s *screen) OnSignedIn(who string)                  { s.signedIn <- who }
func (s *screen) OnClose(reason string)                  { s.closed <- reason }

// TestExe goes through the app's deploy with the bindings: sign in,
// create, wait, claim, list and delete, on a stand-in exe.dev that runs a
// say node in process.
func TestExe(t *testing.T) {
	stand := exetest.New(func(name, _ string, env map[string]string) (string, func(), error) {
		n, err := embedded.Open(context.Background(), embedded.Config{
			DB: filepath.Join(t.TempDir(), "say.db"), SetupCode: env["SAY_SETUP_CODE"], SetupWindow: time.Hour,
		})
		if err != nil {
			return "", nil, err
		}
		addr, err := n.Listen("127.0.0.1:0")
		if err != nil {
			n.Close()
			return "", nil, err
		}
		return "http://" + addr.String(), func() { n.Close() }, nil
	})
	defer stand.Close()

	key, err := NewExeKey()
	if err != nil {
		t.Fatal(err)
	}
	token, err := MintExeToken(key.PrivateKey)
	if err != nil || ExeTokenNeedsRemint(token) {
		t.Fatalf("token %q, %v", token, err)
	}
	client := &exe.Client{Base: stand.URL, Token: token}
	s := &screen{text: make(chan string, 100), signedIn: make(chan string, 1), closed: make(chan string, 1)}
	l, err := startExeLogin(exe.LoginConfig{
		PrivateKey: key.PrivateKey, Cols: 60, Rows: 12, Addr: stand.SSHAddr, HostKey: stand.HostKey,
	}, client, 50*time.Millisecond, s)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for text := ""; !strings.Contains(text, "Email:"); {
		select {
		case text = <-s.text:
		case <-time.After(10 * time.Second):
			t.Fatalf("no email prompt; last screen:\n%s", text)
		}
	}
	l.Type("ada@example.com\r")
	select {
	case who := <-s.signedIn:
		if who != "ada@example.com" {
			t.Fatalf("signed in as %q", who)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("not signed in")
	}
	if reason := <-s.closed; reason != "" {
		t.Fatalf("session closed with %q", reason)
	}

	e := newExe(client)
	d, err := e.create(deploy.Options{Image: "ghcr.io/lum1n/say-node:0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.WaitOpen(d.URL); err != nil {
		t.Fatal(err)
	}
	session, err := ClaimNode(d.URL, d.SetupCode, "ada", "a long password", "test phone")
	if err != nil || session == "" {
		t.Fatalf("claim: %q, %v", session, err)
	}
	if err := e.WaitOpen(d.URL); err == nil {
		t.Fatal("open after the claim")
	}
	nodes, err := e.Nodes()
	var list []struct{ Name, URL string }
	json.Unmarshal([]byte(nodes), &list)
	if err != nil || len(list) != 1 || list[0].Name != d.Name {
		t.Fatalf("nodes = %s, %v", nodes, err)
	}
	if err := e.Delete(d.Name); err != nil {
		t.Fatal(err)
	}
	if nodes, _ := e.Nodes(); nodes != "[]" {
		t.Fatalf("nodes after delete = %s", nodes)
	}
}
