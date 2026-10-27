package exe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/x/vt"
	"golang.org/x/crypto/ssh"
)

// Screen is the sign-in terminal as the app draws it: plain text, styled
// to fit the app, with a text field for input.
type Screen struct {
	Lines        []string // one per row, trailing spaces trimmed
	CursorRow    int
	CursorCol    int
	CursorHidden bool
}

// Text is the screen's rows, with blank rows at the end left out.
func (s Screen) Text() string {
	return strings.TrimRight(strings.Join(s.Lines, "\n"), "\n")
}

// LoginConfig starts a sign-in session.
type LoginConfig struct {
	PrivateKey string // the app's key (NewKey)
	Cols, Rows int    // the terminal's size; 80×24 by default
	// OnScreen gets the screen when it changed, at most every 80 ms, on
	// a goroutine of the session's.
	OnScreen func(Screen)

	Addr    string // SSHAddr by default
	HostKey string // the SHA256 fingerprint to expect; HostKey by default
}

// Login is `ssh exe.dev` with the app's key: exe.dev's own sign-in and
// sign-up screen, kept by a terminal emulator. The app renders whatever
// exe.dev shows rather than matching its prompts, so a changed screen
// keeps working.
type Login struct {
	client *ssh.Client
	sess   *ssh.Session
	stdin  io.WriteCloser

	mu    sync.Mutex
	term  *vt.Emulator
	dirty bool
	shown Screen
	on    func(Screen)

	done    chan struct{}
	err     error
	closing atomic.Bool
}

// screenEvery is how often a changing screen is passed on at most.
const screenEvery = 80 * time.Millisecond

// StartLogin connects, checking exe.dev's host key, and opens a
// terminal. It fails when exe.dev can't be reached or doesn't take the
// key; after that, the session's end shows in Done and Err.
func StartLogin(ctx context.Context, c LoginConfig) (*Login, error) {
	if c.Addr == "" {
		c.Addr = SSHAddr
	}
	if c.HostKey == "" {
		c.HostKey = HostKey
	}
	if c.Cols <= 0 || c.Rows <= 0 {
		c.Cols, c.Rows = 80, 24
	}
	if c.OnScreen == nil {
		c.OnScreen = func(Screen) {}
	}
	signer, err := ssh.ParsePrivateKey([]byte(c.PrivateKey))
	if err != nil {
		return nil, fmt.Errorf("exe.dev key: %w", err)
	}
	config := &ssh.ClientConfig{
		// exe.dev takes any user name; it knows people by their keys.
		User: "say",
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			// exe.dev presents its key in a host certificate; the handshake
			// proves it holds the certified key, which is what's pinned.
			if cert, ok := key.(*ssh.Certificate); ok {
				key = cert.Key
			}
			if ssh.FingerprintSHA256(key) != c.HostKey {
				return errors.New("exe.dev's host key isn't the one it publishes")
			}
			return nil
		},
		Timeout: 15 * time.Second,
	}
	conn, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", c.Addr)
	if err != nil {
		return nil, fmt.Errorf("exe.dev: %w", err)
	}
	cc, chans, reqs, err := ssh.NewClientConn(conn, c.Addr, config)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("exe.dev: %w", err)
	}
	client := ssh.NewClient(cc, chans, reqs)
	l, err := openTerminal(client, c)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("exe.dev: %w", err)
	}
	return l, nil
}

func openTerminal(client *ssh.Client, c LoginConfig) (*Login, error) {
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	if err := sess.RequestPty("xterm-256color", c.Rows, c.Cols, ssh.TerminalModes{ssh.ECHO: 1}); err != nil {
		return nil, err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := sess.Shell(); err != nil {
		return nil, err
	}
	l := &Login{
		client: client,
		sess:   sess,
		stdin:  stdin,
		term:   vt.NewEmulator(c.Cols, c.Rows),
		on:     c.OnScreen,
		done:   make(chan struct{}),
	}
	// The emulator answers exe.dev's queries (cursor position, device
	// attributes) into a pipe it blocks on, so its answers are always
	// read and sent on. This goroutine must not take l.mu.
	go io.Copy(stdin, l.term)
	var out sync.WaitGroup
	for _, r := range []io.Reader{stdout, stderr} {
		out.Go(func() { l.feed(r) })
	}
	go l.publish()
	go func() {
		err := sess.Wait()
		out.Wait()
		var exit *ssh.ExitError
		if errors.As(err, &exit) || errors.Is(err, io.EOF) || l.closing.Load() {
			err = nil // exe.dev ended it, as after signing in
		}
		l.mu.Lock()
		l.err = err
		// Ends the io.Copy above (Emulator.Close would race with its Read).
		l.term.InputPipe().(io.Closer).Close()
		l.mu.Unlock()
		l.flush()
		close(l.done)
		client.Close()
	}()
	return l, nil
}

func (l *Login) feed(r io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			l.mu.Lock()
			l.term.Write(buf[:n])
			l.dirty = true
			l.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (l *Login) publish() {
	tick := time.NewTicker(screenEvery)
	defer tick.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-tick.C:
			l.flush()
		}
	}
}

// flush passes the screen on if it changed since it was last passed on.
func (l *Login) flush() {
	l.mu.Lock()
	if !l.dirty {
		l.mu.Unlock()
		return
	}
	l.dirty = false
	s := l.snapshot()
	same := s.CursorRow == l.shown.CursorRow && s.CursorCol == l.shown.CursorCol &&
		s.CursorHidden == l.shown.CursorHidden && s.Text() == l.shown.Text()
	if !same {
		l.shown = s
	}
	l.mu.Unlock()
	if !same {
		l.on(s)
	}
}

func (l *Login) snapshot() Screen {
	cur := l.term.CursorPosition()
	return Screen{
		Lines:        strings.Split(l.term.String(), "\n"),
		CursorRow:    cur.Y,
		CursorCol:    cur.X,
		CursorHidden: l.term.CursorHidden(),
	}
}

// Screen is the screen now.
func (l *Login) Screen() Screen {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snapshot()
}

// Type sends text as typed; end a line with "\r".
func (l *Login) Type(text string) error {
	_, err := io.WriteString(l.stdin, text)
	return err
}

// Resize changes the terminal's size.
func (l *Login) Resize(cols, rows int) error {
	l.mu.Lock()
	l.term.Resize(cols, rows)
	l.dirty = true
	l.mu.Unlock()
	return l.sess.WindowChange(rows, cols)
}

// Done is closed when the session has ended.
func (l *Login) Done() <-chan struct{} { return l.done }

// Err is why the session ended, after Done: nil when exe.dev ended it.
func (l *Login) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// Close ends the session.
func (l *Login) Close() error {
	l.closing.Store(true)
	return l.client.Close()
}
