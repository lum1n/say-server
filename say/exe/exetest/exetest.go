// Package exetest is a stand-in for exe.dev, for tests: its HTTPS API
// (POST /exec, checking exe0 tokens as exe.dev does) and its SSH sign-in,
// which asks for an email and then adds the key to the account, as
// opening exe.dev's mail would. New VMs are started by a function the
// test gives, such as a say node in process.
package exetest

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/lum1n/say-v2/say/exe"
)

// VM is a VM the stand-in made.
type VM struct {
	Name   string
	Image  string
	Env    map[string]string
	URL    string
	Port   int
	Public bool
	stop   func()
}

// StartFunc runs a new VM's image with env and returns its address and
// a function that stops it.
type StartFunc func(name, image string, env map[string]string) (url string, stop func(), err error)

// Server is the stand-in.
type Server struct {
	// URL is the HTTPS API's base, for exe.Client.Base.
	URL string
	// SSHAddr and HostKey are the sign-in's, for exe.LoginConfig.
	SSHAddr string
	HostKey string
	// Email is who whoami says the account is.
	Email string
	// Shape, when set, is what new answers instead of the usual JSON, to
	// try other output shapes.
	Shape func(name, url string) any

	http  *httptest.Server
	ssh   net.Listener
	start StartFunc

	mu       sync.Mutex
	keys     map[string]bool // authorized public keys, by their wire form
	vms      map[string]*VM
	commands []string
}

// New starts a stand-in. start may be nil when no test runs `new`.
func New(start StartFunc) *Server {
	s := &Server{
		Email: "ada@example.com",
		start: start,
		keys:  map[string]bool{},
		vms:   map[string]*VM{},
	}
	s.http = httptest.NewServer(http.HandlerFunc(s.exec))
	s.URL = s.http.URL
	s.startSSH()
	return s
}

// Close stops the stand-in and its VMs.
func (s *Server) Close() {
	s.http.Close()
	s.ssh.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, vm := range s.vms {
		vm.stop()
	}
}

// AddKey puts an authorized_keys line on the account, as following the
// link in exe.dev's mail does.
func (s *Server) AddKey(authorizedKey string) error {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorizedKey))
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.keys[string(pub.Marshal())] = true
	s.mu.Unlock()
	return nil
}

// Commands are the command lines /exec was given, in order.
func (s *Server) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.commands)
}

// VM is the VM named name, or nil.
func (s *Server) VM(name string) *VM {
	s.mu.Lock()
	defer s.mu.Unlock()
	if vm := s.vms[name]; vm != nil {
		c := *vm
		return &c
	}
	return nil
}

func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/exec" {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	line := strings.TrimSpace(string(body))
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if status, msg := s.check(token, line); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	s.mu.Lock()
	s.commands = append(s.commands, line)
	s.mu.Unlock()
	status, out := s.run(strings.Fields(line))
	writeJSON(w, status, out)
}

// check verifies token as exe.dev does: a valid SSHSIG over the payload
// by a key on the account, not expired, allowing the command.
func (s *Server) check(token, line string) (int, string) {
	perms, payload, sig, err := exe.ParseToken(token)
	if err != nil {
		return http.StatusUnauthorized, err.Error()
	}
	pub, err := exe.VerifySSHSIG(sig, exe.SignatureNamespace, payload)
	if err != nil {
		return http.StatusUnauthorized, "bad signature: " + err.Error()
	}
	s.mu.Lock()
	known := s.keys[string(pub.Marshal())]
	s.mu.Unlock()
	if !known {
		return http.StatusUnauthorized, "key not registered"
	}
	if perms.Exp != 0 && time.Now().Unix() > perms.Exp {
		return http.StatusUnauthorized, "token expired"
	}
	allowed := slices.ContainsFunc(perms.Cmds, func(c string) bool {
		return line == c || strings.HasPrefix(line, c+" ")
	})
	if !allowed {
		return http.StatusForbidden, "command not allowed by token"
	}
	return 0, ""
}

func (s *Server) run(args []string) (int, any) {
	switch {
	case len(args) == 1 && args[0] == "whoami":
		return http.StatusOK, map[string]string{"email": s.Email}
	case len(args) >= 1 && args[0] == "ls":
		s.mu.Lock()
		defer s.mu.Unlock()
		vms := []map[string]string{}
		for _, vm := range s.vms {
			vms = append(vms, map[string]string{"vm_name": vm.Name, "https_url": vm.URL})
		}
		return http.StatusOK, map[string]any{"vms": vms}
	case len(args) >= 1 && args[0] == "new":
		return s.new(args[1:])
	case len(args) == 4 && args[0] == "share" && args[1] == "port":
		return s.withVM(args[2], func(vm *VM) { fmt.Sscan(args[3], &vm.Port) })
	case len(args) == 3 && args[0] == "share" && args[1] == "set-public":
		return s.withVM(args[2], func(vm *VM) { vm.Public = true })
	case len(args) == 2 && args[0] == "rm":
		s.mu.Lock()
		vm := s.vms[args[1]]
		delete(s.vms, args[1])
		s.mu.Unlock()
		if vm == nil {
			return http.StatusUnprocessableEntity, map[string]string{"error": "no such vm"}
		}
		vm.stop()
		return http.StatusOK, map[string]string{"status": "deleted"}
	}
	return http.StatusUnprocessableEntity, map[string]string{"error": "unknown command"}
}

func (s *Server) new(args []string) (int, any) {
	vm := &VM{Env: map[string]string{}}
	for i := 0; i < len(args); i++ {
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch args[i] {
		case "--name":
			vm.Name = next()
		case "--image":
			vm.Image = next()
		case "--env":
			k, v, _ := strings.Cut(next(), "=")
			vm.Env[k] = v
		case "--memory", "--disk":
			next()
		case "--json", "--no-email":
		default:
			return http.StatusUnprocessableEntity, map[string]string{"error": "unknown flag " + args[i]}
		}
	}
	s.mu.Lock()
	taken := s.vms[vm.Name] != nil
	s.mu.Unlock()
	if vm.Name == "" || taken {
		return http.StatusUnprocessableEntity, map[string]string{"error": "name missing or in use"}
	}
	vm.stop = func() {}
	if s.start != nil {
		url, stop, err := s.start(vm.Name, vm.Image, vm.Env)
		if err != nil {
			return http.StatusInternalServerError, map[string]string{"error": err.Error()}
		}
		vm.URL, vm.stop = url, stop
	}
	s.mu.Lock()
	s.vms[vm.Name] = vm
	s.mu.Unlock()
	if s.Shape != nil {
		return http.StatusOK, s.Shape(vm.Name, vm.URL)
	}
	return http.StatusOK, map[string]string{"vm_name": vm.Name, "https_url": vm.URL, "status": "running"}
}

func (s *Server) withVM(name string, f func(*VM)) (int, any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vm := s.vms[name]
	if vm == nil {
		return http.StatusUnprocessableEntity, map[string]string{"error": "no such vm"}
	}
	f(vm)
	return http.StatusOK, map[string]string{"status": "ok"}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// startSSH serves the sign-in: any key may connect; the shell asks for
// an email, says a link was sent, and adds the key to the account.
func (s *Server) startSSH() {
	// Like exe.dev, it presents its host key in a certificate.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := ssh.NewSignerFromKey(priv)
	_, caPriv, _ := ed25519.GenerateKey(rand.Reader)
	ca, _ := ssh.NewSignerFromKey(caPriv)
	cert := &ssh.Certificate{Key: key.PublicKey(), CertType: ssh.HostCert, ValidBefore: ssh.CertTimeInfinity}
	cert.SignCert(rand.Reader, ca)
	signer, _ := ssh.NewCertSigner(cert, key)
	s.HostKey = ssh.FingerprintSHA256(key.PublicKey())
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{Extensions: map[string]string{"key": string(ssh.MarshalAuthorizedKey(key))}}, nil
		},
	}
	config.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	s.ssh, s.SSHAddr = ln, ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serveSSH(c, config)
		}
	}()
}

func (s *Server) serveSSH(c net.Conn, config *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(c, config)
	if err != nil {
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "")
			continue
		}
		ch, reqs, err := nc.Accept()
		if err != nil {
			return
		}
		shell := make(chan struct{})
		go func() {
			for r := range reqs {
				switch r.Type {
				case "pty-req", "window-change":
					r.Reply(true, nil)
				case "shell":
					r.Reply(true, nil)
					close(shell)
				default:
					r.Reply(false, nil)
				}
			}
		}()
		<-shell
		s.signUp(ch, conn.Permissions.Extensions["key"])
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
		ch.Close()
		return
	}
}

// signUp is a small sign-up screen: it clears the screen, asks the
// terminal where its cursor is (as TUIs do), and reads an email with echo.
func (s *Server) signUp(ch ssh.Channel, key string) {
	fmt.Fprint(ch, "\x1b[2J\x1b[H\x1b[1mWelcome to exe.dev\x1b[0m\r\n\r\n\x1b[6n")
	r := bufio.NewReader(ch)
	// The cursor report: ESC [ row ; col R.
	if _, err := r.ReadString('R'); err != nil {
		return
	}
	fmt.Fprint(ch, "Email: ")
	var email []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return
		}
		if b == '\r' || b == '\n' {
			break
		}
		email = append(email, b)
		ch.Write([]byte{b})
	}
	fmt.Fprintf(ch, "\r\n\r\nWe sent a link to %s. Open it to continue.\r\n", email)
	s.AddKey(key)
	time.Sleep(50 * time.Millisecond)
}
