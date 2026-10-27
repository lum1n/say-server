package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/lum1n/say-server/internal/tui"
	"github.com/lum1n/say-server/say"
)

// runTUI runs the terminal client. By default it embeds a local node
// whose user is the person at the terminal; with --server it is a client
// of a remote server.
func runTUI(args []string) error {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	db := fs.String("db", envOr("SAY_DB", defaultDB()), "the local node's SQLite file")
	server := fs.String("server", os.Getenv("SAY_SERVER"), "URL of a say server to use instead of a local node")
	token := fs.String("token", os.Getenv("SAY_TOKEN"), "access token for --server")
	name := fs.String("user", "", "local username (default: your login name)")
	listen := fs.String("listen", "", "also serve the local node here, like 127.0.0.1:7290, for the web app and bots")
	fs.Parse(args)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c *say.Client
	where := "local"
	if *server != "" {
		if *token == "" {
			return errors.New("--server needs --token (or SAY_TOKEN)")
		}
		u, err := url.Parse(*server)
		if err != nil || u.Host == "" {
			return errors.New("--server must be a URL like https://chat.example.com")
		}
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
		c, where = say.NewClient(*server, *token), u.Host
	} else {
		// The node logs to a file next to its database, not over the screen.
		if err := os.MkdirAll(filepath.Dir(*db), 0o700); err != nil {
			return err
		}
		logf, err := os.OpenFile(filepath.Join(filepath.Dir(*db), "tui.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer logf.Close()
		slog.SetDefault(slog.New(slog.NewTextHandler(logf, nil)))

		node, err := say.Open(ctx, say.Config{DB: *db, Version: version})
		if err != nil {
			return err
		}
		defer node.Close()
		if *listen != "" {
			addr, err := node.Listen(*listen)
			if err != nil {
				return err
			}
			where = "local · http://" + addr.String()
		}
		username, display := localName(*name)
		me, err := node.LocalUser(ctx, username, display, "tui")
		if err != nil {
			return err
		}
		c = node.Client(me.Token)
	}
	rt, err := c.Realtime(ctx)
	if err != nil {
		return err
	}
	defer rt.Close()
	_, err = tea.NewProgram(tui.New(c, rt, where), tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	return err
}

// defaultDB is where the TUI keeps its node: the XDG data directory on
// Linux, the user's application data directory elsewhere.
func defaultDB() string {
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		if runtime.GOOS == "linux" || runtime.GOOS == "freebsd" {
			dir = filepath.Join(home, ".local", "share")
		} else if dir, _ = os.UserConfigDir(); dir == "" {
			dir = home
		}
	}
	return filepath.Join(dir, "say", "say.db")
}

// localName is the username and display name of the person at the
// terminal.
func localName(username string) (string, string) {
	u, err := user.Current()
	if username != "" || err != nil {
		if username == "" {
			username = "me"
		}
		return username, ""
	}
	return u.Username, u.Name
}
