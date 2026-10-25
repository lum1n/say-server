// Command say runs the chat server.
//
//	say                         same as `say serve`
//	say serve [flags]           run the server
//	say principal add [flags]   create a principal and print an access token
//	say tui [flags]             chat in the terminal, on a local node or a server
//	say version                 print the version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/vegard/say-v2/internal/core"
	"github.com/vegard/say-v2/internal/store"
	"github.com/vegard/say-v2/say"
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
	case "tui":
		err = runTUI(args)
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

	var peerList []string
	for _, p := range strings.Split(*peers, ",") {
		if p = strings.TrimSpace(p); p != "" {
			peerList = append(peerList, p)
		}
	}
	node, err := say.Open(ctx, say.Config{
		DB:            *dsn,
		Version:       version,
		SetupCode:     *setupCodeFlag,
		Peers:         peerList,
		ClusterAddr:   *clusterAddr,
		ClusterSecret: *clusterSecret,
	})
	if err != nil {
		return err
	}
	defer node.Close()
	ln, err := node.Listen(*addr)
	if err != nil {
		return err
	}
	slog.Info("say listening", "addr", ln.String(), "version", version)
	if code := node.SetupCode(); code != "" {
		slog.Info("open the setup wizard to create the admin", "url", setupURL(ln, code))
	}
	<-ctx.Done()
	return nil
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
