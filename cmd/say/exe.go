package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/lum1n/say-server/say/deploy"
	"github.com/lum1n/say-server/say/exe"
)

const exeUsage = `usage:
  say exe login [--token TOKEN]   sign in to exe.dev (or paste a token)
  say exe whoami                  the exe.dev account signed in
  say exe ls                      the account's VMs
  say exe deploy [flags]          make your own say node and claim it
  say exe rm NAME                 remove a VM and everything on it
  say exe logout                  forget the key and token on this computer`

// exeCmd tries the app's exe.dev deploy from a terminal
// (docs/adr/0017-own-node.md). The key and token live in the user's
// config directory, as the app keeps them in the Keychain.
func exeCmd(args []string) error {
	if len(args) == 0 {
		return errors.New(exeUsage)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	dir, err := exeDir()
	if err != nil {
		return err
	}
	switch args[0] {
	case "login":
		return exeLogin(ctx, dir, args[1:])
	case "logout":
		for _, f := range []string{"key", "token"} {
			if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		fmt.Println("Forgot the exe.dev key and token. To take the key off your exe.dev account too, run `ssh exe.dev ssh-key list` and remove the one named say.")
		return nil
	}
	c, err := exeClient(dir)
	if err != nil {
		return err
	}
	switch args[0] {
	case "whoami":
		who, err := c.Whoami(ctx)
		if err != nil {
			return err
		}
		fmt.Println(who)
	case "ls":
		nodes, err := c.List(ctx)
		if err != nil {
			return err
		}
		for _, n := range nodes {
			fmt.Printf("%s\t%s\n", n.Name, n.URL)
		}
	case "deploy":
		return exeDeploy(ctx, c, args[1:])
	case "rm":
		if len(args) != 2 {
			return errors.New("usage: say exe rm NAME")
		}
		if err := c.Delete(ctx, args[1]); err != nil {
			return err
		}
		fmt.Println("Removed", args[1])
	default:
		return errors.New(exeUsage)
	}
	return nil
}

func exeDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "say", "exe")
	return dir, os.MkdirAll(dir, 0o700)
}

// exeClient loads the token, minting a new one from the key when it
// runs out within two weeks.
func exeClient(dir string) (*exe.Client, error) {
	token, err := os.ReadFile(filepath.Join(dir, "token"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("not signed in to exe.dev; run `say exe login`")
	}
	if err != nil {
		return nil, err
	}
	t := strings.TrimSpace(string(token))
	if exe.NeedsRemint(t, time.Now()) {
		if key, err := os.ReadFile(filepath.Join(dir, "key")); err == nil {
			if t, err = exe.Mint(string(key)); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(dir, "token"), []byte(t), 0o600); err != nil {
				return nil, err
			}
		}
	}
	return exe.New(t), nil
}

func exeLogin(ctx context.Context, dir string, args []string) error {
	fs := flag.NewFlagSet("exe login", flag.ExitOnError)
	pasted := fs.String("token", "", "an exe0 token to use instead of signing in; make one with:\n"+exe.APIKeyCommand)
	fs.Parse(args)
	if *pasted != "" {
		c := exe.New(*pasted)
		who, err := c.Whoami(ctx)
		if err != nil {
			return err
		}
		os.Remove(filepath.Join(dir, "key")) // a pasted token isn't reminted
		fmt.Println("Signed in to exe.dev as", who)
		return os.WriteFile(filepath.Join(dir, "token"), []byte(c.Token), 0o600)
	}

	keyFile := filepath.Join(dir, "key")
	var key string
	if b, err := os.ReadFile(keyFile); err == nil {
		key = string(b)
	} else {
		k, err := exe.NewKey()
		if err != nil {
			return err
		}
		if err := os.WriteFile(keyFile, []byte(k.PrivateKey), 0o600); err != nil {
			return err
		}
		key = k.PrivateKey
	}
	token, err := exe.Mint(key)
	if err != nil {
		return err
	}
	c := exe.New(token)
	if who, err := c.Whoami(ctx); err == nil {
		fmt.Println("Signed in to exe.dev as", who)
		return os.WriteFile(filepath.Join(dir, "token"), []byte(token), 0o600)
	}

	// The app draws exe.dev's screen with a text field under it; here the
	// screen is redrawn whole and each line typed is sent.
	l, err := exe.StartLogin(ctx, exe.LoginConfig{
		PrivateKey: key, Cols: 80, Rows: 24,
		OnScreen: func(s exe.Screen) {
			fmt.Print("\x1b[H\x1b[2J" + strings.ReplaceAll(s.Text(), "\n", "\r\n") + "\r\n\r\n> ")
		},
	})
	if err != nil {
		return err
	}
	defer l.Close()
	go func() {
		in := bufio.NewScanner(os.Stdin)
		for in.Scan() {
			l.Type(in.Text() + "\r")
		}
	}()
	who, err := exe.WaitSignedIn(ctx, c, 3*time.Second)
	if err != nil {
		return err
	}
	fmt.Println("\nSigned in to exe.dev as", who)
	return os.WriteFile(filepath.Join(dir, "token"), []byte(token), 0o600)
}

var usernameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

func exeDeploy(ctx context.Context, c *exe.Client, args []string) error {
	fs := flag.NewFlagSet("exe deploy", flag.ExitOnError)
	name := fs.String("name", "", "VM name; say-<random> when empty")
	image := fs.String("image", "", "image; the newest release's say-node when empty")
	username := fs.String("username", defaultUsername(), "the node's admin")
	keep := fs.Bool("keep", false, "keep a node that never opens for setup, to look at it")
	fs.Parse(args)

	d, err := deploy.Run(ctx, c, deploy.Options{
		Name: *name, Image: *image,
		Progress: func(s deploy.Step) { fmt.Println("…", s) },
	})
	if err != nil {
		if d.Name != "" && !*keep {
			fmt.Println("Removing", d.Name)
			c.Delete(context.Background(), d.Name)
		}
		return err
	}
	password := rand.Text()
	device := "say exe"
	if h, err := os.Hostname(); err == nil {
		device += " on " + h
	}
	token, err := deploy.Claim(ctx, nil, d.URL, d.SetupCode, *username, password, device)
	if err != nil {
		return fmt.Errorf("%w (setup code %s)", err, d.SetupCode)
	}
	fmt.Printf("\nYour say node is up.\n\nurl       %s\nvm        %s\nusername  %s\npassword  %s\ntoken     %s\n", d.URL, d.Name, *username, password, token)
	return nil
}

func defaultUsername() string {
	if u, err := user.Current(); err == nil && usernameRE.MatchString(strings.ToLower(u.Username)) {
		return strings.ToLower(u.Username)
	}
	return "admin"
}
