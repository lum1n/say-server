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

	"github.com/lum1n/say-server/internal/bus"
	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/store"
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
	// SetupUntil, when set, closes setup at that time even without an
	// admin, so a node nobody claimed can't be claimed later
	// (docs/adr/0017-own-node.md).
	SetupUntil time.Time
	// Agents is the agent policy of rooms that set no limits of their own
	// (docs/adr/0005-agent-safety.md). Its zero limits take the Default
	// constants; Paused is ignored.
	Agents core.AgentPolicy
	// HookLimit is how many messages a minute each bridge's hook takes on
	// this node; zero is DefaultHookLimit.
	HookLimit int
	// UpdateDir, on a node that updates itself, holds the updater's status
	// file and say's requests to update (internal/update). Empty
	// elsewhere.
	UpdateDir string
}

type Server struct {
	version    string
	assets     fs.FS
	store      store.Store
	bus        bus.Bus
	defaults   core.AgentPolicy
	setupCode  string
	setupUntil time.Time
	updateDir  string
	hooks      *hookLimiter
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
		version:    c.Version,
		assets:     assets,
		store:      c.Store,
		bus:        c.Bus,
		defaults:   c.Agents,
		setupCode:  c.SetupCode,
		setupUntil: c.SetupUntil,
		updateDir:  c.UpdateDir,
		hooks:      newHookLimiter(c.HookLimit),
		resync:     30 * time.Second,
		heartbeat:  25 * time.Second,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.health)
	mux.HandleFunc("GET /api/v1/setup", s.setupStatus)
	mux.HandleFunc("GET /api/v1/node", s.authed(s.getNode))
	mux.HandleFunc("POST /api/v1/node/update", s.authed(s.updateNode))
	mux.HandleFunc("POST /api/v1/setup", s.setup)
	mux.HandleFunc("POST /api/v1/login", s.login)
	mux.HandleFunc("POST /api/v1/logout", s.authed(s.logout))
	mux.HandleFunc("POST /api/v1/devices", s.pairDevice)
	mux.HandleFunc("POST /api/v1/pairings", s.authed(s.createPairing))
	mux.HandleFunc("GET /api/v1/principals/{id}/tokens", s.authed(s.listTokens))
	mux.HandleFunc("DELETE /api/v1/principals/{id}/tokens/{token_id}", s.authed(s.revokeToken))
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
	mux.HandleFunc("GET /api/v1/rooms/{id}/events/{event_id}", s.authed(s.getEvent))
	mux.HandleFunc("POST /api/v1/rooms/{id}/messages", s.authed(s.postMessage))
	mux.HandleFunc("PATCH /api/v1/rooms/{id}/messages/{event_id}", s.authed(s.patchMessage))
	mux.HandleFunc("PUT /api/v1/rooms/{id}/messages/{event_id}/reactions/{key}", s.authed(s.putReaction))
	mux.HandleFunc("DELETE /api/v1/rooms/{id}/messages/{event_id}/reactions/{key}", s.authed(s.deleteReaction))
	mux.HandleFunc("GET /api/v1/ws", s.authed(s.ws))
	mux.HandleFunc("GET /api/v1/builtin-agents", s.authed(s.listBuiltinAgents))
	mux.HandleFunc("POST /api/v1/builtin-agents", s.authed(s.createBuiltinAgent))
	mux.HandleFunc("PATCH /api/v1/builtin-agents/{id}", s.authed(s.updateBuiltinAgent))
	mux.HandleFunc("POST /api/v1/rooms/{id}/handoff", s.authed(s.handoffRoom))
	mux.HandleFunc("GET /api/v1/widgets", s.authed(s.listWidgets))
	mux.HandleFunc("GET /api/v1/widget-threads", s.authed(s.listWidgetThreads))
	mux.HandleFunc("POST /api/v1/widgets", s.authed(s.createWidget))
	mux.HandleFunc("PATCH /api/v1/widgets/{id}", s.authed(s.updateWidget))
	mux.HandleFunc("DELETE /api/v1/widgets/{id}", s.authed(s.deleteWidget))
	mux.HandleFunc("DELETE /api/v1/visitors/{id}", s.authed(s.deleteVisitor))
	mux.HandleFunc("GET /api/v1/networks", s.authed(s.listNetworks))
	mux.HandleFunc("GET /api/v1/bridges", s.authed(s.listBridges))
	mux.HandleFunc("POST /api/v1/bridges", s.authed(s.createBridge))
	mux.HandleFunc("GET /api/v1/bridges/{id}", s.authed(s.getBridge))
	mux.HandleFunc("PATCH /api/v1/bridges/{id}", s.authed(s.updateBridge))
	mux.HandleFunc("DELETE /api/v1/bridges/{id}", s.authed(s.deleteBridge))
	mux.HandleFunc("POST /api/v1/bridges/{id}/answer", s.authed(s.answerBridge))
	mux.HandleFunc("POST /api/v1/bridges/{id}/screen", s.authed(s.createBridgeScreen))
	mux.HandleFunc("GET /api/v1/screens/{code}", s.bridgeScreen)
	mux.HandleFunc("POST /api/v1/bridges/{id}/hook-secret", s.authed(s.rotateHookSecret))
	mux.HandleFunc("GET /api/v1/bridges/{id}/links", s.authed(s.listLinks))
	mux.HandleFunc("POST /api/v1/bridges/{id}/links", s.authed(s.createLink))
	mux.HandleFunc("DELETE /api/v1/bridges/{id}/links/{link_id}", s.authed(s.deleteLink))
	mux.HandleFunc("POST /api/v1/hooks/{id}", s.hook)
	mux.HandleFunc("GET /api/v1/embed/{key}", s.embedConfig)
	mux.HandleFunc("POST /api/v1/embed/{key}/session", s.embedSession)
	mux.HandleFunc("DELETE /api/v1/embed/{key}/session", s.authed(s.deleteEmbedSession))
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
	if strings.HasPrefix(name, "embed/") {
		w.Header().Set("Content-Security-Policy", s.frameAncestors(r, name))
	} else {
		// Only the widget's page may be framed by other sites.
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'self'")
	}
	w.Write(body)
}

const placeholder = `<!doctype html><meta charset="utf-8"><title>say</title>
<p>The web app is not built into this binary. Run <code>make build</code>.</p>`

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
