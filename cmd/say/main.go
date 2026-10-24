// Command say runs the chat server.
//
//	say                         same as `say serve`
//	say serve [flags]           run the server
//	say principal add [flags]   create a principal and print an access token
//	say version                 print the version
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/vegard/say-v2/internal/agents"
	"github.com/vegard/say-v2/internal/api"
	"github.com/vegard/say-v2/internal/bus"
	"github.com/vegard/say-v2/internal/core"
	"github.com/vegard/say-v2/internal/store"
)

var version = "dev"

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "principal":
		err = principal(args)
	case "version":
		fmt.Println(version)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "say:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", envOr("SAY_ADDR", ":7290"), "listen address")
	dsn := fs.String("db", envOr("SAY_DB", "data/say.db"), "SQLite file path, or a postgres:// URL for cluster mode")
	peers := fs.String("peers", os.Getenv("SAY_PEERS"), "comma-separated host:port cluster addresses of other nodes; enables cluster mode")
	clusterAddr := fs.String("cluster-addr", envOr("SAY_CLUSTER_ADDR", ":6222"), "listen address for other nodes in cluster mode")
	clusterSecret := fs.String("cluster-secret", os.Getenv("SAY_CLUSTER_SECRET"), "shared secret every node must present to join the cluster")
	setupCodeFlag := fs.String("setup-code", os.Getenv("SAY_SETUP_CODE"), "code the setup wizard asks for; random when empty. Give every node the same one in cluster mode")
	fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	b, err := openBus(*peers, *clusterAddr, *clusterSecret, *dsn)
	if err != nil {
		return err
	}
	defer b.Close()

	st, err := store.Open(ctx, *dsn)
	if err != nil {
		return err
	}
	defer st.Close()

	code, err := setupCode(ctx, st, *setupCodeFlag)
	if err != nil {
		return err
	}
	app := api.New(api.Config{Version: version, Store: st, Bus: b, SetupCode: code})
	srv := &http.Server{
		Addr:              *addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Requests share ctx, so streams and long polls end on shutdown
		// instead of holding it up.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	slog.Info("say listening", "addr", ln.Addr().String(), "version", version)
	runner := agents.New(agents.Config{Store: st, Bus: b, Chat: app, Holder: holderName()})
	agentsCtx, stopAgents := context.WithCancel(ctx)
	agentsDone := make(chan struct{})
	go func() {
		defer close(agentsDone)
		runner.Run(agentsCtx)
	}()
	// The runner releases its agents' leases on the way out, so another
	// node takes them over at once; the store must still be open.
	defer func() {
		stopAgents()
		<-agentsDone
	}()
	if code != "" {
		slog.Info("open the setup wizard to create the admin", "url", setupURL(ln.Addr(), code))
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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

// setupURL is the wizard link to print. A wildcard listen address becomes
// localhost, which is right when the operator runs say on their machine.
func setupURL(addr net.Addr, code string) string {
	host, port, _ := net.SplitHostPort(addr.String())
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		host = "localhost"
	}
	return (&url.URL{
		Scheme:   "http",
		Host:     net.JoinHostPort(host, port),
		Path:     "/setup",
		RawQuery: url.Values{"code": {code}}.Encode(),
	}).String()
}

// holderName names this process in leases: the host name, which helps
// operators, plus a random part, because two nodes may share a host.
func holderName() string {
	host, _ := os.Hostname()
	return host + "/" + rand.Text()[:8]
}

// openBus returns the in-process bus, or the NATS cluster bus when peers
// are given.
func openBus(peers, clusterAddr, secret, dsn string) (bus.Bus, error) {
	var list []string
	for _, p := range strings.Split(peers, ",") {
		if p = strings.TrimSpace(p); p != "" {
			list = append(list, p)
		}
	}
	if len(list) == 0 {
		return bus.NewInProc(), nil
	}
	if !store.IsPostgres(dsn) {
		return nil, errors.New("--peers needs a shared postgres:// database (--db)")
	}
	if secret == "" {
		slog.Warn("cluster port is open to anyone who can reach it; set --cluster-secret")
	}
	b, err := bus.NewNATS(bus.NATSConfig{ClusterAddr: clusterAddr, Peers: list, Secret: secret})
	if err != nil {
		return nil, err
	}
	slog.Info("cluster bus started", "cluster_addr", b.ClusterAddr(), "peers", list)
	return b, nil
}

// principal creates a principal and prints its id and a new access token
// that never expires. It is for agents and scripts; people sign in through
// the web app.
func principal(args []string) error {
	if len(args) == 0 || args[0] != "add" {
		return errors.New("usage: say principal add --name NAME [--kind human|agent] [--db DSN]")
	}
	fs := flag.NewFlagSet("principal add", flag.ExitOnError)
	name := fs.String("name", "", "display name")
	kind := fs.String("kind", string(core.PrincipalHuman), "human or agent")
	dsn := fs.String("db", envOr("SAY_DB", "data/say.db"), "SQLite file path, or a postgres:// URL")
	fs.Parse(args[1:])
	if *name == "" {
		return errors.New("--name is required")
	}
	k := core.PrincipalKind(*kind)
	if k != core.PrincipalHuman && k != core.PrincipalAgent {
		return fmt.Errorf("--kind must be human or agent, not %q", *kind)
	}

	ctx := context.Background()
	st, err := store.Open(ctx, *dsn)
	if err != nil {
		return err
	}
	defer st.Close()
	p := core.Principal{ID: core.NewID(), Kind: k, DisplayName: *name}
	if err := st.CreatePrincipal(ctx, p); err != nil {
		return err
	}
	secret, err := st.CreateToken(ctx, p.ID, "cli", time.Time{})
	if err != nil {
		return err
	}
	fmt.Printf("principal %s\ntoken     %s\n", p.ID, secret)
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
