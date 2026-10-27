package exe_test

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/lum1n/say-server/say"
	"github.com/lum1n/say-server/say/deploy"
	"github.com/lum1n/say-server/say/exe"
	"github.com/lum1n/say-server/say/exe/exetest"
)

func TestMintToken(t *testing.T) {
	key, err := exe.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key.PublicKey, "ssh-ed25519 ") || !strings.HasSuffix(key.PublicKey, " say") {
		t.Fatalf("public key = %q", key.PublicKey)
	}
	expires := time.Unix(2_000_000_000, 0)
	token, err := exe.MintToken(key.PrivateKey, exe.Commands, expires)
	if err != nil {
		t.Fatal(err)
	}
	perms, payload, sig, err := exe.ParseToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if perms.Exp != expires.Unix() || strings.Join(perms.Cmds, ",") != strings.Join(exe.Commands, ",") {
		t.Fatalf("permissions = %+v", perms)
	}
	if string(payload) != `{"cmds":["whoami","ls","new","rm","restart","share show","share port","share set-public"],"exp":2000000000}` {
		t.Fatalf("payload = %s", payload)
	}
	pub, err := exe.VerifySSHSIG(sig, exe.SignatureNamespace, payload)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))); !strings.HasPrefix(key.PublicKey, got) {
		t.Fatalf("signed by %s, not %s", got, key.PublicKey)
	}
	if _, err := exe.VerifySSHSIG(sig, "other@example", payload); err == nil {
		t.Fatal("verified in another namespace")
	}

	// OpenSSH must take the signature too, as exe.dev does.
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen to check the signature with")
	}
	dir := t.TempDir()
	sigFile := filepath.Join(dir, "payload.sig")
	os.WriteFile(sigFile, pem.EncodeToMemory(&pem.Block{Type: "SSH SIGNATURE", Bytes: sig}), 0o600)
	cmd := exec.Command(keygen, "-Y", "check-novalidate", "-n", exe.SignatureNamespace, "-s", sigFile)
	cmd.Stdin = strings.NewReader(string(payload))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
}

func TestNeedsRemint(t *testing.T) {
	key, _ := exe.NewKey()
	now := time.Now()
	fresh, _ := exe.Mint(key.PrivateKey)
	old, _ := exe.MintToken(key.PrivateKey, exe.Commands, now.Add(10*24*time.Hour))
	forever, _ := exe.MintToken(key.PrivateKey, exe.Commands, time.Time{})
	if exe.NeedsRemint(fresh, now) || !exe.NeedsRemint(old, now) || exe.NeedsRemint(forever, now) || !exe.NeedsRemint("junk", now) {
		t.Fatal("NeedsRemint is wrong")
	}
	perms, _, _, _ := exe.ParseToken(forever)
	if perms.Exp != 0 {
		t.Fatalf("a token without expiry has exp %d", perms.Exp)
	}
}

func TestAPIKeyCommand(t *testing.T) {
	want := "ssh exe.dev ssh-key generate-api-key --exp=90d --cmds=whoami,ls,new,rm,restart,share show,share port,share set-public"
	if exe.APIKeyCommand != want {
		t.Fatalf("APIKeyCommand = %q", exe.APIKeyCommand)
	}
}

// TestSignIn signs in as the app does: the sign-in screen asks for an
// email, the token starts working once the key is on the account.
func TestSignIn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stand := exetest.New(nil)
	defer stand.Close()
	key, _ := exe.NewKey()
	token, _ := exe.Mint(key.PrivateKey)
	c := &exe.Client{Base: stand.URL, Token: token}

	if _, err := c.Whoami(ctx); !exe.TokenRefused(err) {
		t.Fatalf("whoami before signing in: %v", err)
	}

	screens := make(chan exe.Screen, 100)
	l, err := exe.StartLogin(ctx, exe.LoginConfig{
		PrivateKey: key.PrivateKey, Cols: 60, Rows: 12,
		Addr: stand.SSHAddr, HostKey: stand.HostKey,
		OnScreen: func(s exe.Screen) { screens <- s },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	waitScreen(t, screens, "Email:")

	who := make(chan string, 1)
	go func() {
		w, err := exe.WaitSignedIn(ctx, c, 50*time.Millisecond)
		if err != nil {
			t.Error(err)
		}
		who <- w
	}()
	l.Type("ada@example.com\r")
	s := waitScreen(t, screens, "We sent a link")
	if !strings.Contains(s.Text(), "Email: ada@example.com") {
		t.Fatalf("typed email not echoed:\n%s", s.Text())
	}
	if w := <-who; w != "ada@example.com" {
		t.Fatalf("whoami = %q", w)
	}
	select {
	case <-l.Done():
		if err := l.Err(); err != nil {
			t.Fatalf("session ended with %v", err)
		}
	case <-ctx.Done():
		t.Fatal("session didn't end")
	}
}

func TestSignInChecksHostKey(t *testing.T) {
	stand := exetest.New(nil)
	defer stand.Close()
	key, _ := exe.NewKey()
	_, err := exe.StartLogin(context.Background(), exe.LoginConfig{
		PrivateKey: key.PrivateKey, Addr: stand.SSHAddr, HostKey: exe.HostKey,
	})
	if err == nil || !strings.Contains(err.Error(), "host key") {
		t.Fatalf("StartLogin with a wrong host key: %v", err)
	}
}

func waitScreen(t *testing.T, screens <-chan exe.Screen, want string) exe.Screen {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case s := <-screens:
			if strings.Contains(s.Text(), want) {
				return s
			}
		case <-timeout:
			t.Fatalf("no screen with %q", want)
		}
	}
}

// signedIn is a client whose key is on the stand-in's account.
func signedIn(t *testing.T, stand *exetest.Server, cmds ...string) *exe.Client {
	t.Helper()
	key, _ := exe.NewKey()
	if err := stand.AddKey(key.PublicKey); err != nil {
		t.Fatal(err)
	}
	if cmds == nil {
		cmds = exe.Commands
	}
	token, err := exe.MintToken(key.PrivateKey, cmds, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return &exe.Client{Base: stand.URL, Token: token}
}

// startSay runs a say node in process, as the say-node image would, with
// the settings exe.dev was given.
func startSay(t *testing.T) exetest.StartFunc {
	return func(name, image string, env map[string]string) (string, func(), error) {
		window := time.Duration(0)
		if env["SAY_SETUP_MINUTES"] != "" {
			window = 30 * time.Minute
		}
		n, err := say.Open(context.Background(), say.Config{
			DB:          filepath.Join(t.TempDir(), name+".db"),
			Version:     "test",
			SetupCode:   env["SAY_SETUP_CODE"],
			SetupWindow: window,
		})
		if err != nil {
			return "", nil, err
		}
		addr, err := n.Listen("127.0.0.1:0")
		if err != nil {
			n.Close()
			return "", nil, err
		}
		return "http://" + addr.String(), func() { n.Close() }, nil
	}
}

func TestDeploy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	stand := exetest.New(startSay(t))
	defer stand.Close()
	c := signedIn(t, stand)

	var mu sync.Mutex
	var steps []string
	d, err := deploy.Run(ctx, c, deploy.Options{
		Image:    "ghcr.io/lum1n/say-node:0.1.0",
		Progress: func(s deploy.Step) { mu.Lock(); steps = append(steps, string(s)); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(steps, ",") != "creating,starting,ready" {
		t.Fatalf("steps = %v", steps)
	}
	vm := stand.VM(d.Name)
	if vm == nil || vm.Image != "ghcr.io/lum1n/say-node:0.1.0" || vm.Port != exe.Port || !vm.Public ||
		vm.Env["SAY_SETUP_CODE"] != d.SetupCode || vm.Env["SAY_SETUP_MINUTES"] != "30" {
		t.Fatalf("vm = %+v, deployment %+v", vm, d)
	}
	if !strings.HasPrefix(d.Name, "say-") || d.URL != vm.URL {
		t.Fatalf("deployment = %+v", d)
	}
	cmds := stand.Commands()
	if !strings.HasPrefix(cmds[0], "new --json --no-email --name "+d.Name+" --image ghcr.io/lum1n/say-node:0.1.0 --memory 2GB --disk 20GB --env SAY_SETUP_CODE=") {
		t.Fatalf("new = %q", cmds[0])
	}

	if _, err := deploy.Claim(ctx, nil, d.URL, "WRONG-CODE", "ada", "a long password"); err == nil {
		t.Fatal("claimed with a wrong code")
	}
	token, err := deploy.Claim(ctx, nil, d.URL, d.SetupCode, "ada", "a long password")
	if err != nil {
		t.Fatal(err)
	}
	me, err := say.NewClient(d.URL, token).Me(ctx)
	if err != nil || !me.Admin {
		t.Fatalf("me = %+v, %v", me, err)
	}
	if _, err := deploy.Claim(ctx, nil, d.URL, d.SetupCode, "bob", "a long password"); !errors.Is(err, deploy.ErrSetupDone) {
		t.Fatalf("second claim: %v", err)
	}
	if err := deploy.WaitOpen(ctx, http.DefaultClient, d.URL, time.Second); !errors.Is(err, deploy.ErrSetupDone) {
		t.Fatalf("WaitOpen after the claim: %v", err)
	}

	nodes, err := c.List(ctx)
	if err != nil || len(nodes) != 1 || nodes[0] != d.Node {
		t.Fatalf("ls = %+v, %v", nodes, err)
	}
	if err := c.Delete(ctx, d.Name); err != nil {
		t.Fatal(err)
	}
	if stand.VM(d.Name) != nil {
		t.Fatal("rm left the VM")
	}
}

func TestCreateReadsOutputLeniently(t *testing.T) {
	ctx := context.Background()
	shapes := map[string]func(name, url string) any{
		"name and url": func(name, url string) any {
			return map[string]string{"name": name, "url": "https://" + name + ".exe.xyz/"}
		},
		"nested":   func(name, url string) any { return map[string]any{"vm": map[string]string{"name": name}} },
		"text":     func(name, url string) any { return "Created " + name },
		"bad name": func(name, url string) any { return map[string]string{"name": "x; rm -rf"} },
	}
	for label, shape := range shapes {
		stand := exetest.New(nil)
		stand.Shape = shape
		n, err := signedIn(t, stand).Create(ctx, deploy.Spec{Name: "say-abc", Image: "img:1"})
		stand.Close()
		if err != nil || n.Name != "say-abc" || n.URL != "https://say-abc.exe.xyz" {
			t.Errorf("%s: node = %+v, %v", label, n, err)
		}
	}
}

func TestCreateRefusesUnsafeValues(t *testing.T) {
	stand := exetest.New(nil)
	defer stand.Close()
	c := signedIn(t, stand)
	ctx := context.Background()
	for _, spec := range []deploy.Spec{
		{Name: "say-abc --image evil", Image: "img:1"},
		{Name: "Say", Image: "img:1"},
		{Name: "say-abc", Image: "img:1 --env X=1"},
		{Name: "say-abc", Image: "img:1", Env: map[string]string{"A": "b c"}},
		{Name: "say-abc", Image: "img:1", Env: map[string]string{"a b": "c"}},
		{Name: "say-abc", Image: "img:1", Env: map[string]string{"A": ""}},
	} {
		if _, err := c.Create(ctx, spec); err == nil {
			t.Errorf("%+v accepted", spec)
		}
	}
	if len(stand.Commands()) != 0 {
		t.Fatalf("commands ran: %v", stand.Commands())
	}
	if err := c.Delete(ctx, "x;y"); err == nil {
		t.Fatal("rm x;y accepted")
	}
}

func TestTokenScope(t *testing.T) {
	stand := exetest.New(nil)
	defer stand.Close()
	c := signedIn(t, stand, "whoami")
	ctx := context.Background()
	if _, err := c.Whoami(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.List(ctx); !exe.TokenRefused(err) {
		t.Fatalf("ls with a whoami-only token: %v", err)
	}
	// A token whose signature was tampered with is refused.
	parts := strings.Split(c.Token, ".")
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"cmds":["whoami","rm"]}`))
	bad := &exe.Client{Base: stand.URL, Token: parts[0] + "." + payload + "." + parts[2]}
	if _, err := bad.Whoami(ctx); !exe.TokenRefused(err) {
		t.Fatalf("tampered token: %v", err)
	}
}

func TestNameOf(t *testing.T) {
	for url, want := range map[string]string{
		"https://say-k3x9q2.exe.xyz":       "say-k3x9q2",
		"https://say-k3x9q2.exe.xyz/rooms": "say-k3x9q2",
		"https://example.com":              "",
		"https://a.b.exe.xyz":              "",
	} {
		if got := exe.NameOf(url); got != want {
			t.Errorf("NameOf(%q) = %q, want %q", url, got, want)
		}
	}
}
