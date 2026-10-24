// Package api serves the HTTP API under /api/v1, the widget loader at
// /widget.js and the embedded web app for every other path.
package api

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/vegard/say-v2/internal/bus"
	"github.com/vegard/say-v2/internal/core"
	"github.com/vegard/say-v2/internal/store"
)

//go:embed all:static
var static embed.FS

// The SPA shell that TanStack Start prerenders in SPA mode. Every path that
// is not a file or an API route gets it, and the client router takes over.
const shell = "_shell.html"

// Config is what a Server needs. Store and Bus are required for every
// route except health and the static app.
type Config struct {
	Version string
	Store   store.Store
	Bus     bus.Bus
	// SetupCode must accompany POST /api/v1/setup, which creates the first
	// admin. Empty when the instance is already set up.
	SetupCode string
	// Agents is the agent policy of rooms that set no limits of their own
	// (docs/adr/0005-agent-safety.md). Its zero limits take the Default
	// constants; Paused is ignored.
	Agents core.AgentPolicy
}

type Server struct {
	version   string
	assets    fs.FS
	store     store.Store
	bus       bus.Bus
	defaults  core.AgentPolicy
	setupCode string
	// resync is how often a realtime follower rereads the store without a
	// bus notification; heartbeat is how often idle streams are pinged.
	resync    time.Duration
	heartbeat time.Duration
}

func New(c Config) *Server {
	assets, _ := fs.Sub(static, "static")
	c.Agents.Paused = false
	c.Agents = withDefaults(c.Agents, builtinAgentPolicy)
	return &Server{
		version:   c.Version,
		assets:    assets,
		store:     c.Store,
		bus:       c.Bus,
		defaults:  c.Agents,
		setupCode: c.SetupCode,
		resync:    30 * time.Second,
		heartbeat: 25 * time.Second,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.health)
	mux.HandleFunc("GET /api/v1/setup", s.setupStatus)
	mux.HandleFunc("POST /api/v1/setup", s.setup)
	mux.HandleFunc("POST /api/v1/login", s.login)
	mux.HandleFunc("POST /api/v1/logout", s.authed(s.logout))
	mux.HandleFunc("GET /api/v1/me", s.authed(s.me))
	mux.HandleFunc("GET /api/v1/principals", s.authed(s.listPrincipals))
	mux.HandleFunc("POST /api/v1/principals", s.authed(s.createPrincipal))
	mux.HandleFunc("GET /api/v1/rooms", s.authed(s.listRooms))
	mux.HandleFunc("POST /api/v1/rooms", s.authed(s.createRoom))
	mux.HandleFunc("GET /api/v1/rooms/{id}", s.authed(s.getRoom))
	mux.HandleFunc("PATCH /api/v1/rooms/{id}", s.authed(s.updateRoom))
	mux.HandleFunc("GET /api/v1/rooms/{id}/members", s.authed(s.listMembers))
	mux.HandleFunc("POST /api/v1/rooms/{id}/members", s.authed(s.addMember))
	mux.HandleFunc("PATCH /api/v1/rooms/{id}/members/{principal_id}", s.authed(s.updateMember))
	mux.HandleFunc("GET /api/v1/rooms/{id}/events", s.authed(s.listEvents))
	mux.HandleFunc("GET /api/v1/rooms/{id}/events/stream", s.authed(s.stream))
	mux.HandleFunc("POST /api/v1/rooms/{id}/messages", s.authed(s.postMessage))
	mux.HandleFunc("PATCH /api/v1/rooms/{id}/messages/{event_id}", s.authed(s.patchMessage))
	mux.HandleFunc("GET /api/v1/ws", s.authed(s.ws))
	mux.HandleFunc("GET /api/v1/builtin-agents", s.authed(s.listBuiltinAgents))
	mux.HandleFunc("POST /api/v1/builtin-agents", s.authed(s.createBuiltinAgent))
	mux.HandleFunc("PATCH /api/v1/builtin-agents/{id}", s.authed(s.updateBuiltinAgent))
	mux.Handle("/mcp", s.mcpHandler())
	mux.Handle("/api/", http.NotFoundHandler())
	mux.HandleFunc("/", s.app)
	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.version})
}

func (s *Server) app(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name != "" && name != shell {
		if f, err := s.assets.Open(name); err == nil {
			st, err := f.Stat()
			f.Close()
			if err == nil && !st.IsDir() {
				if strings.HasPrefix(name, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				http.ServeFileFS(w, r, s.assets, name)
				return
			}
		}
		if strings.Contains(name, ".") {
			http.NotFound(w, r)
			return
		}
	}
	body, err := fs.ReadFile(s.assets, shell)
	if err != nil {
		body = []byte(placeholder)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(body)
}

const placeholder = `<!doctype html><meta charset="utf-8"><title>say</title>
<p>The web app is not built into this binary. Run <code>make build</code>.</p>`

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
