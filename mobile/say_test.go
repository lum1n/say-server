package say

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type listener struct {
	msgs   chan string
	closed chan string
}

func (l *listener) OnMessage(m string) { l.msgs <- m }
func (l *listener) OnClose(r string)   { l.closed <- r }

func TestNode(t *testing.T) {
	n, err := Open(filepath.Join(t.TempDir(), "say.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	me, err := n.LocalUser("me", "Me", "android")
	if err != nil || !me.Admin {
		t.Fatalf("LocalUser = %+v, %v", me, err)
	}
	res, err := n.Request("POST", "/api/v1/rooms", me.Token, `{"kind":"group","name":"Notes"}`)
	if err != nil || res.Status != 201 {
		t.Fatalf("create room = %+v, %v", res, err)
	}
	var room struct{ ID string }
	json.Unmarshal([]byte(res.Body), &room)

	l := &listener{msgs: make(chan string, 16), closed: make(chan string, 1)}
	c, err := n.Connect(me.Token, l)
	if err != nil {
		t.Fatal(err)
	}
	c.Send(fmt.Sprintf(`{"id":"1","type":"subscribe","payload":{"room_id":%q,"after":0}}`, room.ID))
	n.Request("POST", "/api/v1/rooms/"+room.ID+"/messages", me.Token, `{"parts":[{"kind":"text","data":{"text":"remember the milk"}}]}`)
	deadline := time.After(5 * time.Second)
	for got := false; !got; {
		select {
		case m := <-l.msgs:
			got = strings.Contains(m, `"type":"event"`) && strings.Contains(m, "remember the milk")
		case <-deadline:
			t.Fatal("no event")
		}
	}
	c.Close()
	select {
	case r := <-l.closed:
		if r != "" {
			t.Fatalf("close reason = %q", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no OnClose")
	}
	addr, err := n.Listen("127.0.0.1:0")
	if err != nil || !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("Listen = %q, %v", addr, err)
	}
}
