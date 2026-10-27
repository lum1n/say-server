package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/lum1n/say-v2/internal/update"
	"github.com/lum1n/say-v2/say/release"
)

// updateCmd is `say update`: say-update.service runs it as root on the
// say-node image (docs/adr/0017-own-node.md).
func updateCmd(args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "only look for a newer release")
	manifest := fs.String("manifest", release.LatestURL, "the release manifest's URL")
	keyFile := fs.String("key", "", "a minisign public key file to check the manifest with, instead of say's release key; for testing")
	fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	u := update.Node(version)
	u.Manifest = *manifest
	if *keyFile != "" {
		b, err := os.ReadFile(*keyFile)
		if err != nil {
			return err
		}
		if u.Key, err = release.ParsePublicKey(b); err != nil {
			return err
		}
	}
	return u.Run(ctx, !*check)
}
