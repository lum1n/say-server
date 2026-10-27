// Command libsay builds the say node as a C shared library, for programs
// in C, Rust, Python, Swift and others (docs/adr/0013-embedding.md):
//
//	make libsay   # build/libsay.so (or .dylib) and build/libsay.h
//
// Nodes and sessions are integer handles; 0 means failure. Strings
// returned by the library are the caller's to release with say_free. A
// function that can fail takes char **err and sets it (to be freed too)
// on failure. Requests and envelopes are the JSON of protocol/.
//
// Sessions are polled with say_recv rather than calling back into the
// caller, so no foreign runtime is entered from a Go thread.
package main

/*
#include <stdint.h>
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
	"unsafe"

	"github.com/lum1n/say-server/say"
)

func main() {}

var (
	mu      sync.Mutex
	next    int64
	nodes   = map[int64]*say.Node{}
	conns   = map[int64]*session{}
	ownedBy = map[int64]int64{} // session → node
)

// session buffers a Conn's envelopes for say_recv.
type session struct {
	c    *say.Conn
	msgs chan string
	err  error // why the session ended, once msgs is closed
}

func handle() int64 {
	next++
	return next
}

func setErr(errp **C.char, err error) {
	if errp != nil {
		*errp = C.CString(err.Error())
	}
}

func node(h C.int64_t, errp **C.char) *say.Node {
	mu.Lock()
	defer mu.Unlock()
	n := nodes[int64(h)]
	if n == nil {
		setErr(errp, errors.New("say: no such node"))
	}
	return n
}

//export say_open
func say_open(dbPath *C.char, errp **C.char) C.int64_t {
	n, err := say.Open(context.Background(), say.Config{DB: C.GoString(dbPath), Version: "libsay"})
	if err != nil {
		setErr(errp, err)
		return 0
	}
	mu.Lock()
	defer mu.Unlock()
	h := handle()
	nodes[h] = n
	return C.int64_t(h)
}

//export say_listen
func say_listen(h C.int64_t, addr *C.char, errp **C.char) *C.char {
	n := node(h, errp)
	if n == nil {
		return nil
	}
	a, err := n.Listen(C.GoString(addr))
	if err != nil {
		setErr(errp, err)
		return nil
	}
	return C.CString(a.String())
}

//export say_setup_code
func say_setup_code(h C.int64_t) *C.char {
	n := node(h, nil)
	if n == nil {
		return nil
	}
	return C.CString(n.SetupCode())
}

// say_local_user returns {"id","display_name","admin","token"} as JSON.
//
//export say_local_user
func say_local_user(h C.int64_t, username, displayName, client *C.char, errp **C.char) *C.char {
	n := node(h, errp)
	if n == nil {
		return nil
	}
	u, err := n.LocalUser(context.Background(), C.GoString(username), C.GoString(displayName), C.GoString(client))
	if err != nil {
		setErr(errp, err)
		return nil
	}
	b, _ := json.Marshal(map[string]any{"id": u.ID, "display_name": u.DisplayName, "admin": u.Admin, "token": u.Token})
	return C.CString(string(b))
}

// say_request calls the REST API, like say_request(node, "GET",
// "/api/v1/rooms", token, NULL, &status, &err), and returns the body.
// body and token may be NULL.
//
//export say_request
func say_request(h C.int64_t, method, path, token, body *C.char, status *C.int, errp **C.char) *C.char {
	n := node(h, errp)
	if n == nil {
		return nil
	}
	res, err := n.Request(context.Background(), C.GoString(method), C.GoString(path), goStringOrEmpty(token), goStringOrEmpty(body))
	if err != nil {
		setErr(errp, err)
		return nil
	}
	if status != nil {
		*status = C.int(res.Status)
	}
	return C.CString(res.Body)
}

//export say_connect
func say_connect(h C.int64_t, token *C.char, errp **C.char) C.int64_t {
	n := node(h, errp)
	if n == nil {
		return 0
	}
	c, err := n.Connect(context.Background(), C.GoString(token))
	if err != nil {
		setErr(errp, err)
		return 0
	}
	s := &session{c: c, msgs: make(chan string, 1024)}
	go func() {
		for {
			msg, err := c.Recv(context.Background())
			if err != nil {
				s.err = err
				close(s.msgs)
				return
			}
			s.msgs <- msg
		}
	}()
	mu.Lock()
	defer mu.Unlock()
	ch := handle()
	conns[ch] = s
	ownedBy[ch] = int64(h)
	return C.int64_t(ch)
}

func conn(h C.int64_t, errp **C.char) *session {
	mu.Lock()
	defer mu.Unlock()
	s := conns[int64(h)]
	if s == nil {
		setErr(errp, errors.New("say: no such session"))
	}
	return s
}

// say_send writes one envelope; it returns 0, or -1 and sets *err.
//
//export say_send
func say_send(h C.int64_t, envelope *C.char, errp **C.char) C.int {
	s := conn(h, errp)
	if s == nil {
		return -1
	}
	if err := s.c.Send(context.Background(), C.GoString(envelope)); err != nil {
		setErr(errp, err)
		return -1
	}
	return 0
}

// say_recv waits up to timeout_ms (-1: forever) for the next envelope. It
// returns NULL with *err unset on a timeout, and NULL with *err set once
// the session has ended.
//
//export say_recv
func say_recv(h C.int64_t, timeoutMS C.int, errp **C.char) *C.char {
	s := conn(h, errp)
	if s == nil {
		return nil
	}
	var timeout <-chan time.Time
	if timeoutMS >= 0 {
		t := time.NewTimer(time.Duration(timeoutMS) * time.Millisecond)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case msg, ok := <-s.msgs:
		if !ok {
			setErr(errp, s.err)
			return nil
		}
		return C.CString(msg)
	case <-timeout:
		return nil
	}
}

//export say_disconnect
func say_disconnect(h C.int64_t) {
	mu.Lock()
	s := conns[int64(h)]
	delete(conns, int64(h))
	delete(ownedBy, int64(h))
	mu.Unlock()
	if s != nil {
		s.c.Close()
	}
}

// say_close ends the node's sessions and stops it.
//
//export say_close
func say_close(h C.int64_t) C.int {
	mu.Lock()
	n := nodes[int64(h)]
	delete(nodes, int64(h))
	var mine []*session
	for ch, nh := range ownedBy {
		if nh == int64(h) {
			mine = append(mine, conns[ch])
			delete(conns, ch)
			delete(ownedBy, ch)
		}
	}
	mu.Unlock()
	for _, s := range mine {
		s.c.Close()
	}
	if n == nil || n.Close() != nil {
		return -1
	}
	return 0
}

//export say_free
func say_free(p *C.char) {
	C.free(unsafe.Pointer(p))
}

func goStringOrEmpty(p *C.char) string {
	if p == nil {
		return ""
	}
	return C.GoString(p)
}
