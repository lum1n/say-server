package say

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPairing(t *testing.T) {
	n, err := Open(filepath.Join(t.TempDir(), "say.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	addr, err := n.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + addr
	phone, err := ClaimNode(url, n.SetupCode(), "ada", "a long password", "phone")
	if err != nil {
		t.Fatal(err)
	}

	p, err := CreatePairing(url, phone)
	if err != nil || len(p.Code) != 11 || p.ExpiresAt < time.Now().Unix() || !strings.HasPrefix(p.Link, url+"/pair?code=") {
		t.Fatalf("CreatePairing = %+v, %v", p, err)
	}
	target, err := ParsePairingLink(p.Link)
	if err != nil || target.NodeURL != url || target.Code != p.Code {
		t.Fatalf("ParsePairingLink(%q) = %+v, %v", p.Link, target, err)
	}
	ipad, err := PairWithCode(target.NodeURL, target.Code, "iPad")
	if err != nil || ipad.Token == "" || ipad.DisplayName != "ada" || !ipad.Admin {
		t.Fatalf("PairWithCode = %+v, %v", ipad, err)
	}
	if _, err := PairWithCode(url, p.Code, "another"); err == nil {
		t.Fatal("a used code paired again")
	}
	if d, err := SignInDevice(url, "ada", "a long password", "laptop"); err != nil || d.Token == "" {
		t.Fatalf("SignInDevice = %+v, %v", d, err)
	}
	if _, err := SignInDevice(url, "ada", "wrong password", "laptop"); err == nil {
		t.Fatal("signed in with a wrong password")
	}

	for _, bad := range []string{"", "hello", "say://pair?code=X", "https://node.example/rooms?code=X", "https://node.example/pair"} {
		if _, err := ParsePairingLink(bad); err == nil {
			t.Errorf("ParsePairingLink(%q) worked", bad)
		}
	}
	if got, _ := ParsePairingLink("https://say-k3x9.exe.xyz/pair?code=ABCDE-FGHJK"); got.NodeURL != "https://say-k3x9.exe.xyz" {
		t.Errorf("node url = %q", got.NodeURL)
	}
}
