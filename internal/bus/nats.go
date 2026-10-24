package bus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// NATSConfig configures one node of the cluster bus.
type NATSConfig struct {
	// ClusterAddr is the host:port other nodes reach this node on. Port 0
	// picks a free port; see (*NATS).ClusterAddr.
	ClusterAddr string
	// Peers are the host:port cluster addresses of other nodes. Reaching any
	// one live node is enough: nodes gossip the rest of the cluster to each
	// other. Unreachable peers are retried for as long as the node runs.
	Peers []string
	// Secret, when set, is required of every node that joins. All nodes
	// must use the same one.
	Secret string
}

// NATS is the cluster bus: an embedded NATS server that routes to the other
// nodes, with an in-process client. NATS only starts routing once its client
// port is open, so that port is bound to loopback on a random port and
// guarded by a random token that only this process knows.
//
// Each node subscribes to every subject once, at startup, and fans out to
// its own subscribers in process. Notifications are tiny and broadcast to
// every node, and a follower never waits for its interest to cross the
// cluster, which a per-follower NATS subscription would.
type NATS struct {
	srv   *server.Server
	nc    *nats.Conn
	local *InProc
}

const clusterUser = "say"

func NewNATS(c NATSConfig) (*NATS, error) {
	host, port, err := splitHostPort(c.ClusterAddr)
	if err != nil {
		return nil, fmt.Errorf("bus: cluster address: %w", err)
	}
	if port == 0 {
		port = server.RANDOM_PORT
	}
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	opts := &server.Options{
		Host:          "127.0.0.1",
		Port:          server.RANDOM_PORT,
		Authorization: token,
		NoSigs:        true,
		Cluster: server.ClusterOpts{
			Name: "say",
			Host: host,
			Port: port,
		},
	}
	if c.Secret != "" {
		opts.Cluster.Username = clusterUser
		opts.Cluster.Password = c.Secret
	}
	for _, p := range c.Peers {
		u := &url.URL{Scheme: "nats-route", Host: p}
		if c.Secret != "" {
			u.User = url.UserPassword(clusterUser, c.Secret)
		}
		opts.Routes = append(opts.Routes, u)
	}

	srv, err := server.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("bus: %w", err)
	}
	srv.SetLoggerV2(natsLogger{}, false, false, false)
	srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		srv.Shutdown()
		return nil, errors.New("bus: embedded NATS did not start")
	}
	nc, err := nats.Connect("", nats.InProcessServer(srv), nats.Token(token))
	if err != nil {
		srv.Shutdown()
		return nil, fmt.Errorf("bus: %w", err)
	}
	b := &NATS{srv: srv, nc: nc, local: NewInProc()}
	if _, err := nc.Subscribe(">", func(m *nats.Msg) {
		b.local.Publish(context.Background(), m.Subject, m.Data)
	}); err != nil {
		b.Close()
		return nil, fmt.Errorf("bus: %w", err)
	}
	if err := nc.Flush(); err != nil {
		b.Close()
		return nil, fmt.Errorf("bus: %w", err)
	}
	return b, nil
}

// ClusterAddr is the address this node listens on for other nodes.
func (b *NATS) ClusterAddr() string { return b.srv.ClusterAddr().String() }

// Peers is the number of other nodes this node is connected to. NATS pools
// several connections per peer, so routes are counted by remote server.
func (b *NATS) Peers() int {
	rz, err := b.srv.Routez(nil)
	if err != nil {
		return 0
	}
	ids := map[string]bool{}
	for _, r := range rz.Routes {
		ids[r.RemoteID] = true
	}
	return len(ids)
}

// Publish sends to every node, this one included.
func (b *NATS) Publish(ctx context.Context, subject string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.mapErr(b.nc.Publish(subject, data))
}

func (b *NATS) Subscribe(subject string, h Handler) (func(), error) {
	return b.local.Subscribe(subject, h)
}

func (b *NATS) Close() error {
	b.nc.Close()
	b.local.Close()
	b.srv.Shutdown()
	b.srv.WaitForShutdown()
	return nil
}

func (b *NATS) mapErr(err error) error {
	if errors.Is(err, nats.ErrConnectionClosed) {
		return ErrClosed
	}
	return err
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func splitHostPort(addr string) (string, int, error) {
	host, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(p)
	if err != nil || port < 0 || port > 65535 {
		return "", 0, fmt.Errorf("bad port %q", p)
	}
	if host == "" {
		host = "0.0.0.0"
	}
	return host, port, nil
}

// natsLogger sends the embedded server's notices, warnings and errors to
// slog. Debug and trace output is dropped.
type natsLogger struct{}

func (natsLogger) Noticef(format string, v ...any) { slog.Info("nats: " + fmt.Sprintf(format, v...)) }
func (natsLogger) Warnf(format string, v ...any)   { slog.Warn("nats: " + fmt.Sprintf(format, v...)) }
func (natsLogger) Errorf(format string, v ...any)  { slog.Error("nats: " + fmt.Sprintf(format, v...)) }
func (natsLogger) Fatalf(format string, v ...any)  { slog.Error("nats: " + fmt.Sprintf(format, v...)) }
func (natsLogger) Debugf(string, ...any)           {}
func (natsLogger) Tracef(string, ...any)           {}
