// Command say runs the chat server.
//
//	say                         same as `say serve`
//	say serve [flags]           run the server
//	say principal add [flags]   create a principal and print an access token
//	say version                 print the version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	addr := fs.String("addr", envOr("SAY_ADDR", ":8080"), "listen address")
	dsn := fs.String("db", envOr("SAY_DB", "data/say.db"), "SQLite file path, or a postgres:// URL for cluster mode")
	fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, *dsn)
	if err != nil {
		return err
	}
	defer st.Close()

	b := bus.NewInProc()
	defer b.Close()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.New(api.Config{Version: version, Store: st, Bus: b}).Handler(),
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

// principal creates a principal and prints its id and a new access token.
// It stands in for the setup wizard until that exists.
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
	secret, err := st.CreateToken(ctx, p.ID, "cli")
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
