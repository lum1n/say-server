package say

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	embedded "github.com/lum1n/say-server/say"
)

// A device signs in to a person's node once and keeps a token that doesn't
// expire, until they revoke it from another device
// (docs/adr/0018-devices.md). In Swift, on the device that adds one:
//
//	let p = try SayCreatePairing(nodeURL, token)
//	show(qr: p.link)                         // and p.code to type in
//
// and on the new device:
//
//	let l = try SayParsePairingLink(scanned)
//	let d = try SayPairWithCode(l.nodeURL, l.code, "Ada's iPad")
//	// keep d.token in the Keychain

// PairedDevice is the token a device signs in with, and whose it is.
type PairedDevice struct {
	Token       string
	PrincipalID string
	DisplayName string
	Admin       bool
}

func paired(d embedded.Device) *PairedDevice {
	return &PairedDevice{Token: d.Token, PrincipalID: d.Principal.ID, DisplayName: d.Principal.DisplayName, Admin: d.Principal.Admin}
}

func deviceContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// SignInDevice signs this device in to the node at nodeURL with a
// username and password, as deviceName.
func SignInDevice(nodeURL, username, password, deviceName string) (*PairedDevice, error) {
	ctx, cancel := deviceContext()
	defer cancel()
	d, err := embedded.NewClient(nodeURL, "").Pair(ctx, username, password, deviceName)
	if err != nil {
		return nil, err
	}
	return paired(d), nil
}

// PairWithCode signs this device in with a pairing code from a device the
// person is signed in on, as deviceName.
func PairWithCode(nodeURL, code, deviceName string) (*PairedDevice, error) {
	ctx, cancel := deviceContext()
	defer cancel()
	d, err := embedded.NewClient(nodeURL, "").PairWithCode(ctx, code, deviceName)
	if err != nil {
		return nil, err
	}
	return paired(d), nil
}

// Pairing is a code that adds another device, good once until ExpiresAt
// (Unix seconds). Link carries the node and the code, for a QR code.
type Pairing struct {
	Code      string
	ExpiresAt int64
	Link      string
}

// CreatePairing makes a pairing code for the person signed in with token.
func CreatePairing(nodeURL, token string) (*Pairing, error) {
	ctx, cancel := deviceContext()
	defer cancel()
	p, err := embedded.NewClient(nodeURL, token).CreatePairing(ctx)
	if err != nil {
		return nil, err
	}
	return &Pairing{Code: p.Code, ExpiresAt: p.ExpiresAt.Unix(), Link: PairingLink(nodeURL, p.Code)}, nil
}

// PairingLink is what an "Add a device" QR code holds: the node's address
// with /pair and the code, so a phone's camera that doesn't know say
// still opens the node.
func PairingLink(nodeURL, code string) string {
	return strings.TrimRight(nodeURL, "/") + "/pair?code=" + url.QueryEscape(code)
}

// PairingTarget is a scanned pairing link taken apart.
type PairingTarget struct {
	NodeURL string
	Code    string
}

// ParsePairingLink takes apart a link from PairingLink.
func ParsePairingLink(link string) (*PairingTarget, error) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || !strings.HasSuffix(u.Path, "/pair") {
		return nil, errors.New("this isn't a say pairing code")
	}
	code := u.Query().Get("code")
	if code == "" {
		return nil, errors.New("this pairing link has no code")
	}
	u.Path, u.RawQuery, u.Fragment = strings.TrimSuffix(u.Path, "/pair"), "", ""
	return &PairingTarget{NodeURL: u.String(), Code: code}, nil
}
