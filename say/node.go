// Package say runs a say node inside another program: a mobile app, a TUI
// or a server (docs/adr/0013-embedding.md). A node is the whole core
// (store, bus, API, built-in agents and bridges) in this process.
//
// Programs talk to it with the same REST and WebSocket protocol as any
// remote client (protocol/), over an in-memory connection: HTTPClient
// and Client need no port. Listen also serves the API and the web app on a
// real address, for a WebView, bots or other clients.
//
//	node, err := say.Open(ctx, say.Config{DB: "data/say.db"})
//	...
//	defer node.Close()
//	me, err := node.LocalUser(ctx, "ada", "Ada", "tui")
//	c := node.Client(me.Token)
//	rooms, err := c.Rooms(ctx)
package say

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lum1n/say-server/internal/agents"
	"github.com/lum1n/say-server/internal/api"
	"github.com/lum1n/say-server/internal/bridge"
	_ "github.com/lum1n/say-server/internal/bridge/networks" // registers every network
	"github.com/lum1n/say-server/internal/bus"
	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/password"
	"github.com/lum1n/say-server/internal/store"
)

// Config is how to run a node. The zero value runs a single node on
// data/say.db.
type Config struct {
	// DB is a SQLite file path, or a postgres:// URL for cluster mode.
	DB string
	// Version is reported by /api/v1/health and to agents' MCP servers.
	Version string
	// SetupCode guards the setup wizard while no admin exists; it is
	// random when empty. Nodes that call LocalUser never need the wizard.
	SetupCode string
	// SetupWindow, when set, closes the wizard that long after Open if
	// nobody has used it (a node deployed for someone who never claims
	// it).
	SetupWindow time.Duration
	// TrustProxy takes the host from X-Forwarded-Host, for a node behind
	// a reverse proxy (such as exe.dev's) that every request comes
	// through. Origin checks, such as the WebSocket's, then see the
	// address people use. Never set it when clients can reach the node
	// directly: they could claim any host.
	TrustProxy bool
	// UpdateDir, on a node that updates itself (the say-node image), is
	// where `say update` reports and say asks it to run, for
	// GET /api/v1/node.
	UpdateDir string

	// Peers are host:port cluster addresses of other nodes. Setting them
	// runs the NATS cluster bus, which needs a postgres:// DB.
	Peers         []string
	ClusterAddr   string // default ":6222"
	ClusterSecret string
	// Holder names this node in leases; by default the host name plus a
	// random part.
	Holder string
}

// Node is a running say node.
type Node struct {
	ctx    context.Context
	cancel context.CancelFunc

	store    store.Store
	bus      bus.Bus
	api      *api.Server
	handler  http.Handler
	mem      *memListener
	client   *http.Client
	agents   chan struct{} // closed when the agent runner has stopped
	bridges  chan struct{} // closed when the bridge runner has stopped
	setup    string
	mu       sync.Mutex
	servers  []*http.Server
	closeErr error
	closed   sync.Once
}

// BaseURL is the address of a node's in-memory endpoint, which only its
// HTTPClient can reach.
const BaseURL = "http://say.internal"

// Open opens the store, migrates it, and starts the node: the API on its
// in-memory endpoint and the built-in agents.
func Open(ctx context.Context, c Config) (*Node, error) {
	if c.DB == "" {
		c.DB = "data/say.db"
	}
	if c.Version == "" {
		c.Version = "dev"
	}
	if c.ClusterAddr == "" {
		c.ClusterAddr = ":6222"
	}
	if c.Holder == "" {
		host, _ := os.Hostname()
		c.Holder = host + "/" + rand.Text()[:8]
	}
	b, err := openBus(c)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(ctx, c.DB)
	if err != nil {
		b.Close()
		return nil, err
	}
	code, err := setupCode(ctx, st, c.SetupCode)
	if err != nil {
		st.Close()
		b.Close()
		return nil, err
	}
	nctx, cancel := context.WithCancel(context.Background())
	n := &Node{
		ctx:     nctx,
		cancel:  cancel,
		store:   st,
		bus:     b,
		mem:     newMemListener(),
		agents:  make(chan struct{}),
		bridges: make(chan struct{}),
		setup:   code,
	}
	var setupUntil time.Time
	if code != "" && c.SetupWindow > 0 {
		setupUntil = time.Now().Add(c.SetupWindow)
	}
	n.api = api.New(api.Config{Version: c.Version, Store: st, Bus: b, SetupCode: code, SetupUntil: setupUntil, UpdateDir: c.UpdateDir})
	n.handler = n.api.Handler()
	if c.TrustProxy {
		n.handler = forwardedHost(n.handler)
	}
	n.client = &http.Client{Transport: &http.Transport{
		DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return n.mem.dial(ctx) },
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     time.Minute,
	}}
	n.serve(n.mem)
	runner := agents.New(agents.Config{Store: st, Bus: b, Chat: n.api, Holder: c.Holder, Version: c.Version})
	go func() {
		defer close(n.agents)
		runner.Run(nctx)
	}()
	bridges := bridge.New(bridge.Config{Store: st, Bus: b, Chat: n.api, Holder: c.Holder})
	go func() {
		defer close(n.bridges)
		bridges.Run(nctx)
	}()
	return n, nil
}

func openBus(c Config) (bus.Bus, error) {
	var peers []string
	for _, p := range c.Peers {
		if p = strings.TrimSpace(p); p != "" {
			peers = append(peers, p)
		}
	}
	if len(peers) == 0 {
		return bus.NewInProc(), nil
	}
	if !store.IsPostgres(c.DB) {
		return nil, errors.New("say: cluster peers need a shared postgres:// database")
	}
	if c.ClusterSecret == "" {
		slog.Warn("cluster port is open to anyone who can reach it; set a cluster secret")
	}
	b, err := bus.NewNATS(bus.NATSConfig{ClusterAddr: c.ClusterAddr, Peers: peers, Secret: c.ClusterSecret})
	if err != nil {
		return nil, err
	}
	slog.Info("cluster bus started", "cluster_addr", b.ClusterAddr(), "peers", peers)
	return b, nil
}

// setupCode returns the code that guards the setup wizard, or "" when the
// instance is already set up.
func setupCode(ctx context.Context, st store.Store, fixed string) (string, error) {
	done, err := st.SetupDone(ctx)
	if err != nil || done {
		return "", err
	}
	if fixed != "" {
		return fixed, nil
	}
	return rand.Text(), nil
}

func (n *Node) serve(l net.Listener) {
	srv := &http.Server{
		Handler:           n.handler,
		ReadHeaderTimeout: 10 * time.Second,
		// Streams and long polls end when the node closes instead of
		// holding up its shutdown.
		BaseContext: func(net.Listener) context.Context { return n.ctx },
	}
	n.mu.Lock()
	n.servers = append(n.servers, srv)
	n.mu.Unlock()
	go srv.Serve(l)
}

// forwardedHost serves requests with the host the proxy was asked for.
func forwardedHost(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
			// A proxy chain may list several; the first is the client's.
			fh, _, _ = strings.Cut(fh, ",")
			r.Host = strings.TrimSpace(fh)
		}
		h.ServeHTTP(w, r)
	})
}

// Handler serves the API, the web app and /mcp, to mount in a program's
// own HTTP server.
func (n *Node) Handler() http.Handler { return n.handler }

// HTTPClient reaches the node in memory: use it with BaseURL, for REST
// calls and WebSockets alike. It does not reach other hosts.
func (n *Node) HTTPClient() *http.Client { return n.client }

// Listen also serves the node on a TCP address, such as "127.0.0.1:0"
// for a WebView in the same app or bots on the same machine. It returns
// the address it listens on.
func (n *Node) Listen(addr string) (net.Addr, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	n.serve(ln)
	return ln.Addr(), nil
}

// SetupCode is the code the setup wizard asks for, or "" once the
// instance has an admin.
func (n *Node) SetupCode() string {
	if done, err := n.store.SetupDone(n.ctx); err == nil && done {
		return ""
	}
	return n.setup
}

// User is a person's principal and an access token for them.
type User struct {
	ID          string
	DisplayName string
	Admin       bool
	Token       string
}

// LocalUser returns a token for the person with username, creating them
// if need be, for an app whose user is the person at the device. The first
// one becomes the instance admin, without the setup wizard. Each call
// replaces the token LocalUser last issued for the same person and client,
// so an app may call it on every start. The person gets a random password;
// they sign in elsewhere only if an admin sets one.
func (n *Node) LocalUser(ctx context.Context, username, displayName, client string) (User, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" || client == "" {
		return User{}, errors.New("say: LocalUser needs a username and a client name")
	}
	var p core.Principal
	l, err := n.store.LoginByUsername(ctx, username)
	switch {
	case err == nil:
		if p, err = n.store.GetPrincipal(ctx, l.PrincipalID); err != nil {
			return User{}, err
		}
	case errors.Is(err, store.ErrNotFound):
		if p, err = n.createLocal(ctx, username, displayName); err != nil {
			return User{}, err
		}
	default:
		return User{}, err
	}
	name := "local:" + client
	if err := n.store.RevokeTokensNamed(ctx, p.ID, name); err != nil {
		return User{}, err
	}
	token, err := n.store.CreateToken(ctx, p.ID, name, time.Time{})
	if err != nil {
		return User{}, err
	}
	return User{ID: p.ID, DisplayName: p.DisplayName, Admin: p.Admin, Token: token}, nil
}

func (n *Node) createLocal(ctx context.Context, username, displayName string) (core.Principal, error) {
	if displayName = strings.TrimSpace(displayName); displayName == "" {
		displayName = username
	}
	p := core.Principal{ID: core.NewID(), Kind: core.PrincipalHuman, DisplayName: displayName, CreatedAt: time.Now().Truncate(time.Millisecond)}
	l := store.Login{Username: username, PasswordHash: password.Hash(rand.Text() + rand.Text())}
	done, err := n.store.SetupDone(ctx)
	if err != nil {
		return core.Principal{}, err
	}
	if !done {
		err = n.store.Setup(ctx, p, l)
		if err == nil {
			p.Admin = true
			return p, nil
		}
		if !errors.Is(err, store.ErrSetupDone) {
			return core.Principal{}, err
		}
	}
	switch err := n.store.CreateUser(ctx, p, l); {
	case errors.Is(err, store.ErrUsernameTaken):
		// Created by a concurrent call.
		l, err := n.store.LoginByUsername(ctx, username)
		if err != nil {
			return core.Principal{}, err
		}
		return n.store.GetPrincipal(ctx, l.PrincipalID)
	case err != nil:
		return core.Principal{}, fmt.Errorf("say: creating %q: %w", username, err)
	}
	return p, nil
}

// Close stops the node: its listeners, open streams, agents and bridges,
// then the store and bus.
func (n *Node) Close() error {
	n.closed.Do(func() {
		n.cancel()
		n.mu.Lock()
		servers := n.servers
		n.mu.Unlock()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, srv := range servers {
			if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
				n.closeErr = errors.Join(n.closeErr, err)
			}
		}
		n.mem.Close()
		n.client.CloseIdleConnections()
		// The runners release their leases on the way out, so the store
		// must still be open.
		<-n.agents
		<-n.bridges
		n.closeErr = errors.Join(n.closeErr, n.store.Close(), n.bus.Close())
	})
	return n.closeErr
}

// memListener hands one end of an in-memory pipe to the node's HTTP server
// for each dial.
type memListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newMemListener() *memListener {
	return &memListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *memListener) dial(ctx context.Context) (net.Conn, error) {
	server, client := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.done:
	case <-ctx.Done():
		server.Close()
		client.Close()
		return nil, ctx.Err()
	}
	server.Close()
	client.Close()
	return nil, errors.New("say: the node is closed")
}

func (l *memListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *memListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *memListener) Addr() net.Addr { return memAddr{} }

type memAddr struct{}

func (memAddr) Network() string { return "memory" }
func (memAddr) String() string  { return "say.internal" }
